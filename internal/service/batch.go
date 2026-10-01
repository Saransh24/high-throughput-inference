package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"hti/internal/api"
)

// Batch tracks a group of requests submitted together. The requests live in
// items in submission order, so every request id appears exactly once in the
// results by construction.
type Batch struct {
	ID          string
	CallbackURL string
	items       []*Request
	created     time.Time

	mu        sync.Mutex
	pending   int
	succeeded int
	byReason  [3]int // permanent, retries_exhausted, expired
	status    string
	completed time.Time
	callback  api.CallbackView
}

var reasonIndex = map[string]int{api.ReasonPermanent: 0, api.ReasonExhausted: 1, api.ReasonExpired: 2}

// batchItemDone is called once per request when it reaches a final state.
// The batch completes, and its callback is created, only when the last
// request reports in.
func (e *Engine) batchItemDone(b *Batch, state, reason string, now time.Time) {
	b.mu.Lock()
	if state == api.StateSucceeded {
		b.succeeded++
	} else {
		b.byReason[reasonIndex[reason]]++
	}
	b.pending--
	done := b.pending == 0
	if done {
		b.completed = now
		switch {
		case b.succeeded == len(b.items):
			b.status = api.BatchCompleted
		case b.succeeded == 0:
			b.status = api.BatchFailed
		default:
			b.status = api.BatchWithFailures
		}
		if b.CallbackURL != "" {
			b.callback.State = api.CallbackPending
		}
	}
	b.mu.Unlock()
	if done && b.CallbackURL != "" {
		go e.deliverCallback(b)
	}
}

func (b *Batch) summaryLocked() api.BatchSummary {
	failed := b.byReason[0] + b.byReason[1] + b.byReason[2]
	s := api.BatchSummary{
		BatchID: b.ID, Status: b.status, CreatedAt: b.created, ResultsURL: "/v1/batches/" + b.ID + "/results",
		Counts: api.BatchCounts{
			Total: len(b.items), Succeeded: b.succeeded, Failed: failed, Pending: b.pending,
			FailedByReason: map[string]int{api.ReasonPermanent: b.byReason[0], api.ReasonExhausted: b.byReason[1], api.ReasonExpired: b.byReason[2]},
		},
	}
	if !b.completed.IsZero() {
		t := b.completed
		s.CompletedAt = &t
	}
	return s
}

func (b *Batch) View() api.BatchView {
	b.mu.Lock()
	defer b.mu.Unlock()
	cb := b.callback
	cb.URL = b.CallbackURL
	if cb.State == "" {
		cb.State = api.CallbackNone
		if b.CallbackURL != "" {
			cb.State = api.CallbackPending
		}
	}
	cb.History = append([]api.CallbackAttempt(nil), cb.History...)
	return api.BatchView{BatchSummary: b.summaryLocked(), Callback: cb}
}

// deliverCallback posts the batch summary to the client's URL until it gets a
// 2xx response or runs out of attempts. Delivery is at-least-once: the same
// X-HTI-Delivery-Id is sent on every attempt so the receiver can de-duplicate.
func (e *Engine) deliverCallback(b *Batch) {
	b.mu.Lock()
	body, _ := json.Marshal(b.summaryLocked())
	b.mu.Unlock()
	cfg := e.cfg.Callback
	deliveryID := newID("dlv_")

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		at := time.Now()
		status, err := e.postCallback(b.CallbackURL, body, deliveryID, attempt, at)
		rec := api.CallbackAttempt{Attempt: attempt, At: at, Status: status}
		if err != nil {
			rec.Error = err.Error()
		}
		ok := err == nil && status >= 200 && status < 300

		b.mu.Lock()
		b.callback.Attempts = attempt
		b.callback.History = append(b.callback.History, rec)
		if ok {
			done := time.Now()
			b.callback.State, b.callback.DeliveredAt = api.CallbackDelivered, &done
		} else if attempt == cfg.MaxAttempts {
			b.callback.State = api.CallbackGaveUp
		}
		b.mu.Unlock()
		if ok {
			return
		}

		delay := min(float64(cfg.BaseMs)*float64(int64(1)<<min(attempt-1, 20)), float64(cfg.MaxMs))
		select {
		case <-time.After(time.Duration(delay * float64(time.Millisecond))):
		case <-e.stop:
			return
		}
	}
}

func (e *Engine) postCallback(url string, body []byte, deliveryID string, attempt int, at time.Time) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-HTI-Event", "batch.completed")
	req.Header.Set("X-HTI-Delivery-Id", deliveryID)
	req.Header.Set("X-HTI-Attempt", strconv.Itoa(attempt))
	req.Header.Set("X-HTI-Timestamp", ts)
	req.Header.Set("X-HTI-Signature", "sha256="+Sign(e.cfg.Callback.Secret, ts, body))
	resp, err := e.callbackClient.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Sign returns the HMAC-SHA256 of "timestamp.body". A receiver that knows the
// secret recomputes it to confirm the callback came from this service.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
