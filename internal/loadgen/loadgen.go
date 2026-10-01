// Package loadgen sends synthetic traffic to the service and reports what
// happened to it.
//
// The generator is open-loop: it sends at the requested rate whether or not
// the service keeps up, the way independent clients would. A closed-loop
// generator (send, wait, send) slows down with the system under test and
// hides queueing delay.
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"hti/internal/api"
	"hti/internal/metrics"
)

type ModelShare struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

type Options struct {
	Name       string        `json:"name"`
	ServiceURL string        `json:"service_url"`
	SimURL     string        `json:"simulator_url,omitempty"` // optional; adds the provider's own counters to the report
	Rate       float64       `json:"rate_per_second"`
	Duration   time.Duration `json:"duration_ns"`
	RampUp     time.Duration `json:"ramp_up_ns"`
	Warmup     time.Duration `json:"warmup_ns"` // excluded from the "after warm-up" figures
	Drain      time.Duration `json:"drain_ns"`  // how long to wait for outstanding work after sending stops
	Models     []ModelShare  `json:"models"`
	BatchSize  int           `json:"batch_size"` // 1 sends single requests; more sends batches of this size
	Tokens     int64         `json:"tokens"`
	Jitter     float64       `json:"token_jitter"` // 0.2 means each request is within 20% of Tokens
	DeadlineMs int64         `json:"deadline_ms,omitempty"`

	Callback          bool   `json:"callback"`
	CallbackAddr      string `json:"callback_addr,omitempty"`
	CallbackFailFirst int    `json:"callback_fail_first"`
	CallbackSecret    string `json:"-"`

	Concurrency int    `json:"concurrency"`
	IDPrefix    string `json:"id_prefix"`
	Seed        uint64 `json:"seed"`

	Log func(format string, args ...any) `json:"-"`
}

func (o *Options) setDefaults() {
	if o.Name == "" {
		o.Name = "loadgen"
	}
	if o.BatchSize < 1 {
		o.BatchSize = 1
	}
	if o.Tokens <= 0 {
		o.Tokens = 1000
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4096
	}
	if o.Drain <= 0 {
		o.Drain = 60 * time.Second
	}
	if o.IDPrefix == "" {
		o.IDPrefix = fmt.Sprintf("lg%d", time.Now().Unix())
	}
	if o.CallbackAddr == "" {
		o.CallbackAddr = "127.0.0.1:9191"
	}
	if o.CallbackSecret == "" {
		o.CallbackSecret = "dev-secret"
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// Acknowledgement kinds recorded for every request the generator sends.
const (
	ackPending uint8 = iota
	ackAccepted
	ackRejected
	ackError
)

type entry struct {
	model uint8
	ack   uint8
}

// BatchRun is what the generator knows about one batch it submitted.
type BatchRun struct {
	BatchID     string         `json:"batch_id"`
	Size        int            `json:"size"`
	SubmittedAt time.Time      `json:"submitted_at"`
	AckMs       float64        `json:"ack_ms"`
	HTTPStatus  int            `json:"http_status"`
	Accepted    bool           `json:"accepted"`
	View        *api.BatchView `json:"view,omitempty"`

	ResultRows   int            `json:"result_rows"`
	UniqueIDs    int            `json:"unique_ids"`
	IDsMatch     bool           `json:"ids_match"` // results hold exactly the submitted ids, each once
	StatusCounts map[string]int `json:"status_counts,omitempty"`
	LastFinalAt  *time.Time     `json:"last_request_final_at,omitempty"`
	Deliveries   []Delivery     `json:"deliveries,omitempty"`

	ids []string
}

// Result carries the report plus the raw data it was computed from.
type Result struct {
	Report  *Report
	Service api.ServiceStats
	Sim     *api.SimStats
	Batches []*BatchRun
	Sink    *Sink
	Start   time.Time
	LoadEnd time.Time
}

type client struct {
	base string
	http *http.Client
}

func (c *client) do(method, path string, body []byte, out any, headers ...string) (int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// planned returns how many requests should have been sent after t.
func (o *Options) planned(t time.Duration) float64 {
	s, ramp := t.Seconds(), o.RampUp.Seconds()
	if ramp > 0 && s < ramp {
		return o.Rate * s * s / (2 * ramp)
	}
	return o.Rate * (s - ramp/2)
}

func Run(ctx context.Context, o Options) (*Result, error) {
	o.setDefaults()
	if o.Rate <= 0 || o.Duration <= 0 || len(o.Models) == 0 {
		return nil, fmt.Errorf("rate, duration and at least one model are required")
	}
	var totalWeight float64
	for _, m := range o.Models {
		totalWeight += m.Weight
	}
	tr := &http.Transport{MaxIdleConns: o.Concurrency, MaxIdleConnsPerHost: o.Concurrency, IdleConnTimeout: 90 * time.Second}
	svc := &client{base: o.ServiceURL, http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}

	res := &Result{}
	if o.Callback {
		sink, err := StartSink(o.CallbackAddr, o.CallbackFailFirst, o.CallbackSecret)
		if err != nil {
			return nil, fmt.Errorf("callback receiver: %w", err)
		}
		res.Sink = sink
		defer sink.Close()
	}

	var before api.ServiceStats
	if _, err := svc.do("GET", "/v1/stats?series=false", nil, &before); err != nil {
		return nil, fmt.Errorf("service not reachable at %s: %w", o.ServiceURL, err)
	}

	rng := rand.New(rand.NewPCG(o.Seed, 0x9E3779B97F4A7C15))
	pickModel := func() int {
		x := rng.Float64() * totalWeight
		for i, m := range o.Models {
			if x -= m.Weight; x < 0 {
				return i
			}
		}
		return len(o.Models) - 1
	}
	pickTokens := func() int64 {
		return max(1, int64(float64(o.Tokens)*(1+o.Jitter*(2*rng.Float64()-1))))
	}

	total := int(o.planned(o.Duration))
	ledger := make([]entry, total)
	var ackHist metrics.Histogram
	var accepted, rejected, sendErrors atomic.Int64
	var batchMu sync.Mutex
	sem := make(chan struct{}, o.Concurrency)
	var wg sync.WaitGroup

	sendOne := func(i int, model string, tokens int64) {
		defer wg.Done()
		defer func() { <-sem }()
		body := fmt.Appendf(nil, `{"request_id":"%s-%d","model":"%s","estimated_tokens":%d,"payload":{"prompt":"load test"}`, o.IDPrefix, i, model, tokens)
		if o.DeadlineMs > 0 {
			body = fmt.Appendf(body, `,"deadline_ms":%d`, o.DeadlineMs)
		}
		body = append(body, '}')
		t := time.Now()
		status, err := svc.do("POST", "/v1/requests", body, nil)
		ackHist.Observe(time.Since(t))
		switch {
		case err != nil:
			ledger[i].ack = ackError
			sendErrors.Add(1)
		case status == http.StatusAccepted || status == http.StatusOK:
			ledger[i].ack = ackAccepted
			accepted.Add(1)
		case status == http.StatusTooManyRequests:
			ledger[i].ack = ackRejected
			rejected.Add(1)
		default:
			ledger[i].ack = ackError
			sendErrors.Add(1)
		}
	}

	sendBatch := func(no, first int, sub api.BatchSubmit, br *BatchRun) {
		defer wg.Done()
		defer func() { <-sem }()
		body, _ := json.Marshal(sub)
		var ack api.BatchAck
		t := time.Now()
		br.SubmittedAt = t
		status, err := svc.do("POST", "/v1/batches", body, &ack, "Idempotency-Key", fmt.Sprintf("%s-b%d", o.IDPrefix, no))
		br.AckMs = float64(time.Since(t).Microseconds()) / 1000
		ackHist.Observe(time.Since(t))
		br.HTTPStatus = status
		n := int64(len(sub.Requests))
		kind := ackError
		switch {
		case err != nil:
			sendErrors.Add(n)
		case status == http.StatusAccepted || status == http.StatusOK:
			br.Accepted, br.BatchID, kind = true, ack.BatchID, ackAccepted
			accepted.Add(n)
		case status == http.StatusTooManyRequests:
			kind = ackRejected
			rejected.Add(n)
		default:
			sendErrors.Add(n)
		}
		for j := range sub.Requests {
			ledger[first+j].ack = kind
		}
	}

	// Progress line every five seconds.
	start := time.Now()
	res.Start = start
	progressDone := make(chan struct{})
	var sent atomic.Int64
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				var st api.ServiceStats
				if _, err := svc.do("GET", "/v1/stats?series=false", nil, &st); err == nil {
					c := st.Totals
					o.Log("%4.0fs  sent %d  accepted %d  rejected %d | service: completed %d  expired %d  queued %d  in flight %d",
						time.Since(start).Seconds(), sent.Load(), accepted.Load(), rejected.Load(),
						c.Completed()-before.Totals.Completed(), c.Expired-before.Totals.Expired, c.Queued, c.InFlight)
				}
			}
		}
	}()

	// Pacing loop.
	ticker := time.NewTicker(5 * time.Millisecond)
	n, batchNo := 0, 0
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
			continue
		case <-ticker.C:
		}
		elapsed := time.Since(start)
		if elapsed >= o.Duration {
			elapsed, done = o.Duration, true
		}
		target := min(int(o.planned(elapsed)), total)
		if o.BatchSize == 1 {
			for ; n < target; n++ {
				m := pickModel()
				ledger[n].model = uint8(m)
				sem <- struct{}{}
				wg.Add(1)
				go sendOne(n, o.Models[m].Name, pickTokens())
			}
		} else {
			for target-n >= o.BatchSize || (done && n < total) {
				size := min(o.BatchSize, total-n)
				sub := api.BatchSubmit{Requests: make([]api.RequestSpec, size), DeadlineMs: o.DeadlineMs}
				if res.Sink != nil {
					sub.CallbackURL = res.Sink.URL
				}
				br := &BatchRun{Size: size, ids: make([]string, size)}
				for j := range sub.Requests {
					m := pickModel()
					ledger[n+j].model = uint8(m)
					br.ids[j] = fmt.Sprintf("%s-b%d-%d", o.IDPrefix, batchNo, j)
					sub.Requests[j] = api.RequestSpec{RequestID: br.ids[j], Model: o.Models[m].Name, EstimatedTokens: pickTokens(), Payload: json.RawMessage(`{"prompt":"load test"}`)}
				}
				batchMu.Lock()
				res.Batches = append(res.Batches, br)
				batchMu.Unlock()
				sem <- struct{}{}
				wg.Add(1)
				go sendBatch(batchNo, n, sub, br)
				n += size
				batchNo++
			}
		}
		sent.Store(int64(n))
	}
	ticker.Stop()
	wg.Wait()
	res.LoadEnd = time.Now()
	o.Log("sending finished: %d sent in %.1fs; waiting up to %s for outstanding work", n, res.LoadEnd.Sub(start).Seconds(), o.Drain)

	// Drain: wait until nothing is queued or in flight and callbacks are settled.
	drainUntil := time.Now().Add(o.Drain)
	for time.Now().Before(drainUntil) && ctx.Err() == nil {
		var st api.ServiceStats
		svc.do("GET", "/v1/stats?series=false", nil, &st)
		if st.Totals.Waiting() == 0 && callbacksSettled(svc, res.Batches) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	close(progressDone)

	// Collect.
	if _, err := svc.do("GET", "/v1/stats", nil, &res.Service); err != nil {
		return nil, fmt.Errorf("reading service stats: %w", err)
	}
	if o.SimURL != "" {
		sim := &client{base: o.SimURL, http: svc.http}
		var st api.SimStats
		if _, err := sim.do("GET", "/admin/stats", nil, &st); err == nil {
			res.Sim = &st
		}
	}
	for _, br := range res.Batches {
		collectBatch(svc, br, res.Sink)
	}

	lookup := map[string]int64{}
	if o.BatchSize == 1 {
		ids := make([]string, 0, 5000)
		flush := func() {
			if len(ids) == 0 {
				return
			}
			body, _ := json.Marshal(api.LookupRequest{IDs: ids})
			var out api.LookupResponse
			if _, err := svc.do("POST", "/v1/requests/lookup", body, &out); err != nil {
				lookup["lookup_error"] += int64(len(ids))
			}
			for _, s := range out.Statuses {
				lookup[s]++
			}
			ids = ids[:0]
		}
		for i := 0; i < n; i++ {
			if ledger[i].ack == ackAccepted {
				if ids = append(ids, fmt.Sprintf("%s-%d", o.IDPrefix, i)); len(ids) == cap(ids) {
					flush()
				}
			}
		}
		flush()
	}

	res.Report = buildReport(o, res, before, generatorTotals{
		Sent: int64(n), Accepted: accepted.Load(), Rejected: rejected.Load(), Errors: sendErrors.Load(),
		Ack: ackHist.Summary(), Lookup: lookup,
	})
	return res, nil
}

// callbacksSettled reports whether every accepted batch has finished and its
// callback has been delivered or abandoned.
func callbacksSettled(svc *client, batches []*BatchRun) bool {
	for _, br := range batches {
		if !br.Accepted {
			continue
		}
		var v api.BatchView
		if _, err := svc.do("GET", "/v1/batches/"+br.BatchID, nil, &v); err != nil {
			return false
		}
		if v.Status == api.BatchInProgress || v.Callback.State == api.CallbackPending {
			return false
		}
	}
	return true
}

// collectBatch reads a batch's final status and every result page, and checks
// that the results contain exactly the submitted ids.
func collectBatch(svc *client, br *BatchRun, sink *Sink) {
	if !br.Accepted {
		return
	}
	var v api.BatchView
	if _, err := svc.do("GET", "/v1/batches/"+br.BatchID, nil, &v); err != nil {
		return
	}
	br.View = &v
	seen := make(map[string]int, br.Size)
	br.StatusCounts = map[string]int{}
	for offset := 0; ; {
		var page api.BatchResults
		if _, err := svc.do("GET", fmt.Sprintf("/v1/batches/%s/results?offset=%d&limit=5000", br.BatchID, offset), nil, &page); err != nil {
			break
		}
		for _, r := range page.Results {
			br.ResultRows++
			seen[r.RequestID]++
			br.StatusCounts[r.Status]++
			if r.FinishedAt != nil && (br.LastFinalAt == nil || r.FinishedAt.After(*br.LastFinalAt)) {
				t := *r.FinishedAt
				br.LastFinalAt = &t
			}
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	br.UniqueIDs = len(seen)
	br.IDsMatch = br.ResultRows == br.Size && len(seen) == br.Size
	for _, id := range br.ids {
		if seen[id] != 1 {
			br.IDsMatch = false
			break
		}
	}
	if sink != nil {
		br.Deliveries = sink.For(br.BatchID)
	}
}
