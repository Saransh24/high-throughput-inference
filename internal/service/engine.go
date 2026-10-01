// Package service is the inference service: it accepts requests, queues them
// per model, releases them to the provider under that model's limits, retries
// transient failures, and records exactly one final state for every request.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"hti/internal/api"
	"hti/internal/limiter"
	"hti/internal/metrics"
	"hti/internal/provider"
)

type CallbackConfig struct {
	MaxAttempts int    `json:"max_attempts"`
	BaseMs      int    `json:"base_ms"`
	MaxMs       int    `json:"max_ms"`
	TimeoutMs   int    `json:"timeout_ms"`
	Secret      string `json:"secret"`
}

type Config struct {
	ProviderURL       string                `json:"provider_url"`
	Models            map[string]api.Limits `json:"models"`
	Headroom          float64               `json:"headroom"`            // share of each limit left unused (default 0.02)
	BurstMs           int                   `json:"burst_ms"`            // pacer burst (default 200)
	DefaultDeadlineMs int64                 `json:"default_deadline_ms"` // single requests (default 30 s)
	BatchDeadlineMs   int64                 `json:"batch_deadline_ms"`   // batch requests (default 1 h)
	AdmitFraction     float64               `json:"admit_fraction"`      // admit if expected wait <= this share of the deadline (default 0.8)
	MaxAttempts       int                   `json:"max_attempts"`        // provider calls per request (default 4)
	RetryBaseMs       int                   `json:"retry_base_ms"`       // default 200
	RetryMaxMs        int                   `json:"retry_max_ms"`        // default 10 s
	ProviderTimeoutMs int                   `json:"provider_timeout_ms"` // default 30 s
	MaxInFlight       int64                 `json:"max_in_flight"`       // per model (default 10,000)
	Callback          CallbackConfig        `json:"callback"`
}

func (c *Config) setDefaults() {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	if c.Headroom <= 0 {
		c.Headroom = 0.02
	}
	if c.AdmitFraction <= 0 {
		c.AdmitFraction = 0.8
	}
	if c.DefaultDeadlineMs <= 0 {
		c.DefaultDeadlineMs = 30_000
	}
	if c.BatchDeadlineMs <= 0 {
		c.BatchDeadlineMs = 3_600_000
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 10_000
	}
	def(&c.BurstMs, 200)
	def(&c.MaxAttempts, 4)
	def(&c.RetryBaseMs, 200)
	def(&c.RetryMaxMs, 10_000)
	def(&c.ProviderTimeoutMs, 30_000)
	def(&c.Callback.MaxAttempts, 10)
	def(&c.Callback.BaseMs, 1000)
	def(&c.Callback.MaxMs, 60_000)
	def(&c.Callback.TimeoutMs, 5000)
	if c.Callback.Secret == "" {
		c.Callback.Secret = "dev-secret"
	}
}

// Request is one inference request and its current state.
type Request struct {
	ID        string
	Model     string
	Tokens    int64
	Payload   json.RawMessage
	batch     *Batch
	submitted time.Time
	deadline  time.Time // latest moment the request may be sent

	mu        sync.Mutex
	state     string
	reason    string
	attempts  int
	firstSent time.Time
	finished  time.Time
	output    json.RawMessage
	errMsg    string
}

func isFinal(state string) bool {
	return state == api.StateSucceeded || state == api.StateFailed || state == api.StateExpired
}

func (r *Request) setState(s string) {
	r.mu.Lock()
	if !isFinal(r.state) {
		r.state = s
	}
	r.mu.Unlock()
}

func (r *Request) View() api.RequestView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := api.RequestView{
		RequestID: r.ID, Model: r.Model, Status: r.state, Reason: r.reason, Attempts: r.attempts,
		EstimatedTokens: r.Tokens, Output: r.output, Error: r.errMsg, SubmittedAt: r.submitted,
	}
	if r.batch != nil {
		v.BatchID = r.batch.ID
	}
	if !r.firstSent.IsZero() {
		t := r.firstSent
		v.FirstSentAt = &t
	}
	if !r.finished.IsZero() {
		t := r.finished
		v.FinishedAt = &t
	}
	return v
}

// fifo is a simple queue of requests.
type fifo struct {
	items []*Request
	head  int
}

func (q *fifo) len() int        { return len(q.items) - q.head }
func (q *fifo) push(r *Request) { q.items = append(q.items, r) }
func (q *fifo) peek() *Request  { return q.items[q.head] }
func (q *fifo) pop() {
	q.items[q.head] = nil
	q.head++
	if q.head >= 1024 && q.head*2 >= len(q.items) { // reclaim the consumed front
		n := copy(q.items, q.items[q.head:])
		clear(q.items[n:])
		q.items = q.items[:n]
		q.head = 0
	}
}

// lane holds everything for one model: its queues, limiter and counters.
// Models never share a lane, so one throttled model cannot hold up another.
type lane struct {
	model string

	mu           sync.Mutex // guards the limiter and both queues
	lim          *limiter.Limiter
	retryQ       fifo // requests coming back from a transient failure; served first
	mainQ        fifo
	queuedTokens int64

	queued, inFlight, retryWait atomic.Int64
	submitted, rejected         atomic.Int64
	succeeded, failed, expired  atomic.Int64
	calls, rateLimited          atomic.Int64

	series                  *metrics.Series
	hQueue, hProvider, hTot metrics.Histogram
}

type Engine struct {
	cfg      Config
	provider provider.Provider
	started  time.Time
	tick     time.Duration

	lanesMu sync.RWMutex
	lanes   map[string]*lane

	requests sync.Map // request id -> *Request (single requests)
	batches  sync.Map // batch id -> *Batch
	idem     sync.Map // Idempotency-Key -> *Batch

	changesMu sync.Mutex
	changes   []api.LimitChange

	callbackClient *http.Client
	stop           chan struct{}
}

func New(cfg Config, p provider.Provider) *Engine {
	cfg.setDefaults()
	e := &Engine{
		cfg: cfg, provider: p, started: time.Now(), tick: 2 * time.Millisecond,
		lanes:          map[string]*lane{},
		callbackClient: &http.Client{Timeout: time.Duration(cfg.Callback.TimeoutMs) * time.Millisecond},
		stop:           make(chan struct{}),
	}
	for name, l := range cfg.Models {
		e.addLane(name, l.RPM, l.TPM)
	}
	go e.sampleQueues()
	return e
}

func (e *Engine) Close() { close(e.stop) }

func (e *Engine) addLane(name string, rpm, tpm int64) *lane {
	opt := limiter.DefaultOptions()
	opt.Headroom = e.cfg.Headroom
	opt.Burst = time.Duration(e.cfg.BurstMs) * time.Millisecond
	now := time.Now()
	ln := &lane{model: name, lim: limiter.New(rpm, tpm, opt, now), series: metrics.NewSeries(rpm, tpm)}
	e.lanes[name] = ln
	e.changes = append(e.changes, api.LimitChange{At: now, Model: name, Limits: api.Limits{RPM: rpm, TPM: tpm}})
	go e.runLane(ln)
	return ln
}

func (e *Engine) lane(model string) *lane {
	e.lanesMu.RLock()
	defer e.lanesMu.RUnlock()
	return e.lanes[model]
}

// SetLimits changes a model's limits while running. An unknown model is
// created, which needs both values.
func (e *Engine) SetLimits(model string, p api.LimitsPatch) (api.Limits, error) {
	e.lanesMu.Lock()
	ln := e.lanes[model]
	if ln == nil {
		if p.RPM == nil || p.TPM == nil {
			e.lanesMu.Unlock()
			return api.Limits{}, fmt.Errorf("model %q does not exist; give both rpm and tpm to create it", model)
		}
		e.changesMu.Lock()
		e.addLane(model, *p.RPM, *p.TPM)
		e.changesMu.Unlock()
		e.lanesMu.Unlock()
		return api.Limits{RPM: *p.RPM, TPM: *p.TPM}, nil
	}
	e.lanesMu.Unlock()

	now := time.Now()
	ln.mu.Lock()
	rpm, tpm := ln.lim.Limits()
	if p.RPM != nil {
		rpm = *p.RPM
	}
	if p.TPM != nil {
		tpm = *p.TPM
	}
	if rpm < 0 || tpm < 0 {
		ln.mu.Unlock()
		return api.Limits{}, errors.New("limits cannot be negative")
	}
	ln.lim.SetLimits(rpm, tpm, now)
	ln.series.SetLimits(now, rpm, tpm)
	ln.mu.Unlock()

	e.changesMu.Lock()
	e.changes = append(e.changes, api.LimitChange{At: now, Model: model, Limits: api.Limits{RPM: rpm, TPM: tpm}})
	e.changesMu.Unlock()
	return api.Limits{RPM: rpm, TPM: tpm}, nil
}

// Submission.

var ErrUnknownModel = errors.New("unknown model")

// RejectedError means the request was refused because the queue is too long
// for it to be served before its deadline.
type RejectedError struct{ RetryAfter time.Duration }

func (e *RejectedError) Error() string {
	return "over capacity: request would expire before it could be sent"
}

type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }

func newID(prefix string) string {
	var b [10]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (e *Engine) validate(spec *api.RequestSpec) (*lane, error) {
	if spec.Model == "" {
		return nil, &invalidError{"model is required"}
	}
	ln := e.lane(spec.Model)
	if ln == nil {
		return nil, fmt.Errorf("%w %q", ErrUnknownModel, spec.Model)
	}
	if spec.EstimatedTokens <= 0 {
		return nil, &invalidError{"estimated_tokens must be greater than zero"}
	}
	return ln, nil
}

// admit reports whether n more requests carrying the given tokens can be
// expected to leave the queue before the deadline. Caller holds ln.mu.
func (ln *lane) admit(now time.Time, n, tokens int64, deadline time.Time, fraction float64) (bool, time.Duration) {
	_, tpm := ln.lim.Limits()
	reqRate, tokRate := ln.lim.Rates()
	if reqRate <= 0 || tokRate <= 0 || tokens/n > tpm {
		return false, time.Minute
	}
	ahead := float64(ln.retryQ.len() + ln.mainQ.len())
	wait := max((ahead+float64(n))/reqRate, float64(ln.queuedTokens+tokens)/tokRate)
	budget := deadline.Sub(now).Seconds() * fraction
	if wait <= budget {
		return true, 0
	}
	return false, time.Duration((wait - budget) * float64(time.Second))
}

func (ln *lane) enqueue(r *Request) {
	ln.mainQ.push(r)
	ln.queuedTokens += r.Tokens
	ln.queued.Add(1)
}

// Submit accepts one request. If a request with the same id already exists it
// is returned unchanged, which makes client retries of a submit harmless.
func (e *Engine) Submit(spec api.RequestSpec) (r *Request, existing bool, err error) {
	ln, err := e.validate(&spec)
	if err != nil {
		return nil, false, err
	}
	now := time.Now()
	if spec.RequestID == "" {
		spec.RequestID = newID("req_")
	}
	deadlineMs := spec.DeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = e.cfg.DefaultDeadlineMs
	}
	r = &Request{
		ID: spec.RequestID, Model: spec.Model, Tokens: spec.EstimatedTokens, Payload: spec.Payload,
		submitted: now, deadline: now.Add(time.Duration(deadlineMs) * time.Millisecond), state: api.StateQueued,
	}
	if prev, loaded := e.requests.LoadOrStore(r.ID, r); loaded {
		return prev.(*Request), true, nil
	}

	ln.submitted.Add(1)
	ln.mu.Lock()
	ok, retryAfter := ln.admit(now, 1, r.Tokens, r.deadline, e.cfg.AdmitFraction)
	if ok {
		ln.enqueue(r)
	}
	ln.mu.Unlock()

	if !ok {
		e.requests.Delete(r.ID)
		ln.rejected.Add(1)
		ln.series.Update(now, func(p *metrics.Point) { p.Submitted++; p.Rejected++ })
		return nil, false, &RejectedError{RetryAfter: retryAfter}
	}
	ln.series.Update(now, func(p *metrics.Point) { p.Submitted++ })
	return r, false, nil
}

// SubmitBatch accepts a whole batch or none of it.
func (e *Engine) SubmitBatch(sub api.BatchSubmit) (*Batch, error) {
	if len(sub.Requests) == 0 {
		return nil, &invalidError{"requests must not be empty"}
	}
	now := time.Now()
	deadlineMs := sub.DeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = e.cfg.BatchDeadlineMs
	}
	deadline := now.Add(time.Duration(deadlineMs) * time.Millisecond)
	b := &Batch{ID: newID("bat_"), CallbackURL: sub.CallbackURL, created: now, status: api.BatchInProgress,
		pending: len(sub.Requests), items: make([]*Request, len(sub.Requests))}

	type group struct {
		ln     *lane
		reqs   []*Request
		tokens int64
	}
	groups := map[string]*group{}
	seen := make(map[string]struct{}, len(sub.Requests))
	for i := range sub.Requests {
		spec := &sub.Requests[i]
		ln, err := e.validate(spec)
		if err != nil {
			return nil, &invalidError{fmt.Sprintf("requests[%d]: %v", i, err)}
		}
		if spec.RequestID == "" {
			spec.RequestID = fmt.Sprintf("%s-%d", b.ID, i)
		}
		if _, dup := seen[spec.RequestID]; dup {
			return nil, &invalidError{fmt.Sprintf("requests[%d]: duplicate request_id %q", i, spec.RequestID)}
		}
		seen[spec.RequestID] = struct{}{}
		r := &Request{ID: spec.RequestID, Model: spec.Model, Tokens: spec.EstimatedTokens, Payload: spec.Payload,
			batch: b, submitted: now, deadline: deadline, state: api.StateQueued}
		b.items[i] = r
		g := groups[spec.Model]
		if g == nil {
			g = &group{ln: ln}
			groups[spec.Model] = g
		}
		g.reqs = append(g.reqs, r)
		g.tokens += r.Tokens
	}

	// Lock the lanes involved in a fixed order, check all, then enqueue all.
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		groups[name].ln.mu.Lock()
	}
	admitted := true
	var retryAfter time.Duration
	for _, name := range names {
		g := groups[name]
		ok, ra := g.ln.admit(now, int64(len(g.reqs)), g.tokens, deadline, e.cfg.AdmitFraction)
		if !ok {
			admitted, retryAfter = false, max(retryAfter, ra)
		}
	}
	if admitted {
		for _, name := range names {
			for _, r := range groups[name].reqs {
				groups[name].ln.enqueue(r)
			}
		}
	}
	for _, name := range names {
		groups[name].ln.mu.Unlock()
	}

	for _, g := range groups {
		n := int64(len(g.reqs))
		g.ln.submitted.Add(n)
		if !admitted {
			g.ln.rejected.Add(n)
		}
		g.ln.series.Update(now, func(p *metrics.Point) {
			p.Submitted += n
			if !admitted {
				p.Rejected += n
			}
		})
	}
	if !admitted {
		return nil, &RejectedError{RetryAfter: retryAfter}
	}
	e.batches.Store(b.ID, b)
	return b, nil
}

// Dispatch.

// runLane is the dispatcher loop for one model. Every tick it releases as many
// queued requests as the limiter allows and expires those past their deadline.
func (e *Engine) runLane(ln *lane) {
	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()
	var send, dead []*Request
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		send, dead = send[:0], dead[:0]
		var sentTokens int64

		ln.mu.Lock()
		for {
			q := &ln.retryQ
			if q.len() == 0 {
				q = &ln.mainQ
			}
			if q.len() == 0 {
				break
			}
			r := q.peek()
			if now.After(r.deadline) {
				q.pop()
				ln.queuedTokens -= r.Tokens
				dead = append(dead, r)
				continue
			}
			if ln.inFlight.Load()+int64(len(send)) >= e.cfg.MaxInFlight || !ln.lim.TryAcquire(now, r.Tokens) {
				break
			}
			q.pop()
			ln.queuedTokens -= r.Tokens
			sentTokens += r.Tokens
			send = append(send, r)
		}
		ln.mu.Unlock()

		if n := int64(len(send)); n > 0 {
			ln.queued.Add(-n)
			ln.inFlight.Add(n)
			ln.series.Update(now, func(p *metrics.Point) { p.Requests += n; p.Tokens += sentTokens })
			for _, r := range send {
				go e.execute(ln, r, now)
			}
		}
		for _, r := range dead {
			ln.queued.Add(-1)
			e.finalize(ln, r, api.StateExpired, api.ReasonExpired, nil, "deadline passed while waiting")
		}
	}
}

// execute makes one provider call for r and acts on the outcome.
func (e *Engine) execute(ln *lane, r *Request, sentAt time.Time) {
	r.mu.Lock()
	r.state = api.StateInFlight
	r.attempts++
	attempt := r.attempts
	if r.firstSent.IsZero() {
		r.firstSent = sentAt
		ln.hQueue.Observe(sentAt.Sub(r.submitted))
	}
	payload := r.Payload
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(e.cfg.ProviderTimeoutMs)*time.Millisecond)
	start := time.Now()
	res := e.provider.Do(ctx, &api.InferRequest{RequestID: r.ID, Model: r.Model, EstimatedTokens: r.Tokens, Payload: payload, Attempt: attempt})
	cancel()
	ln.hProvider.Observe(time.Since(start))
	ln.calls.Add(1)
	ln.inFlight.Add(-1)

	switch res.Outcome {
	case provider.Success:
		e.finalize(ln, r, api.StateSucceeded, "", res.Output, "")
	case provider.Permanent:
		e.finalize(ln, r, api.StateFailed, api.ReasonPermanent, nil, res.Error)
	case provider.Transient:
		if res.RateLimited {
			ln.rateLimited.Add(1)
		}
		if attempt >= e.cfg.MaxAttempts {
			e.finalize(ln, r, api.StateFailed, api.ReasonExhausted, nil, res.Error)
			return
		}
		delay := e.backoff(attempt)
		if time.Now().Add(delay).After(r.deadline) {
			e.finalize(ln, r, api.StateExpired, api.ReasonExpired, nil, "deadline would pass before the next retry: "+res.Error)
			return
		}
		r.setState(api.StateRetryWait)
		ln.retryWait.Add(1)
		time.AfterFunc(delay, func() {
			r.setState(api.StateQueued)
			ln.mu.Lock()
			ln.retryQ.push(r)
			ln.queuedTokens += r.Tokens
			ln.mu.Unlock()
			ln.retryWait.Add(-1)
			ln.queued.Add(1)
		})
	}
}

// backoff doubles from the base on each attempt, capped, with random jitter
// so retries from many requests do not arrive together.
func (e *Engine) backoff(attempt int) time.Duration {
	d := float64(e.cfg.RetryBaseMs) * float64(int64(1)<<min(attempt-1, 20))
	d = min(d, float64(e.cfg.RetryMaxMs))
	return time.Duration(d * (0.5 + 0.5*mrand.Float64()) * float64(time.Millisecond))
}

// finalize moves r to a final state. Only the first call for a request has
// any effect, so each request is counted exactly once.
func (e *Engine) finalize(ln *lane, r *Request, state, reason string, output json.RawMessage, errMsg string) {
	now := time.Now()
	r.mu.Lock()
	if isFinal(r.state) {
		r.mu.Unlock()
		return
	}
	r.state, r.reason, r.output, r.errMsg, r.finished = state, reason, output, errMsg, now
	r.Payload = nil // no longer needed; free the memory
	r.mu.Unlock()

	switch state {
	case api.StateSucceeded:
		ln.succeeded.Add(1)
		ln.series.Update(now, func(p *metrics.Point) { p.Succeeded++ })
	case api.StateFailed:
		ln.failed.Add(1)
		ln.series.Update(now, func(p *metrics.Point) { p.Failed++ })
	case api.StateExpired:
		ln.expired.Add(1)
		ln.series.Update(now, func(p *metrics.Point) { p.Expired++ })
	}
	ln.hTot.Observe(now.Sub(r.submitted))
	if r.batch != nil {
		e.batchItemDone(r.batch, state, reason, now)
	}
}

// sampleQueues records each model's queue depth once a second.
func (e *Engine) sampleQueues() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case now := <-t.C:
			e.lanesMu.RLock()
			for _, ln := range e.lanes {
				q := ln.queued.Load()
				ln.series.Update(now, func(p *metrics.Point) { p.Queued = q })
			}
			e.lanesMu.RUnlock()
		}
	}
}

// Stats.

func (ln *lane) counts() api.Counts {
	return api.Counts{
		Submitted: ln.submitted.Load(), Rejected: ln.rejected.Load(),
		Queued: ln.queued.Load(), InFlight: ln.inFlight.Load(), RetryWait: ln.retryWait.Load(),
		Succeeded: ln.succeeded.Load(), Failed: ln.failed.Load(), Expired: ln.expired.Load(),
		ProviderCalls: ln.calls.Load(), ProviderRateLimited: ln.rateLimited.Load(),
	}
}

func (e *Engine) Stats(withSeries bool) api.ServiceStats {
	now := time.Now()
	out := api.ServiceStats{StartedAt: e.started, Now: now, Models: map[string]*api.ModelStats{}}
	e.lanesMu.RLock()
	for name, ln := range e.lanes {
		ln.mu.Lock()
		rpm, tpm := ln.lim.Limits()
		ln.mu.Unlock()
		ms := &api.ModelStats{
			Limits:  api.Limits{RPM: rpm, TPM: tpm},
			Counts:  ln.counts(),
			Latency: api.Latency{Queue: ln.hQueue.Summary(), Provider: ln.hProvider.Summary(), Total: ln.hTot.Summary()},
		}
		if withSeries {
			ms.Series = ln.series.Snapshot(now)
		}
		out.Models[name] = ms
		out.Totals.Add(ms.Counts)
	}
	e.lanesMu.RUnlock()
	e.changesMu.Lock()
	out.LimitChanges = append([]api.LimitChange(nil), e.changes...)
	e.changesMu.Unlock()
	return out
}
