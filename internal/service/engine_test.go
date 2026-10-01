package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"hti/internal/api"
	"hti/internal/simulator"
)

const fast = 6_000_000 // RPM high enough that the limiter is not the bottleneck

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A batch with provider failures must complete, list every id exactly once,
// and deliver a callback whose body equals the status the API reports, even
// when the first two deliveries are refused.
func TestBatchCompletesAndCallsBackAfterRetries(t *testing.T) {
	sim := simulator.New(simulator.Config{Models: map[string]api.SimModel{
		"a": {RPM: fast, TPM: fast * 1000, TransientRate: 0.2, PermanentRate: 0.05},
		"b": {RPM: fast, TPM: fast * 1000, TransientRate: 0.2, PermanentRate: 0.05},
	}})

	var mu sync.Mutex
	var bodies [][]byte
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer sink.Close()

	e := New(Config{
		Models:      map[string]api.Limits{"a": {RPM: fast, TPM: fast * 1000}, "b": {RPM: fast, TPM: fast * 1000}},
		RetryBaseMs: 1, RetryMaxMs: 5,
		Callback: CallbackConfig{BaseMs: 10, MaxMs: 20},
	}, sim)
	defer e.Close()

	const n = 2000
	sub := api.BatchSubmit{CallbackURL: sink.URL}
	for i := 0; i < n; i++ {
		sub.Requests = append(sub.Requests, api.RequestSpec{RequestID: fmt.Sprintf("r%d", i), Model: []string{"a", "b"}[i%2], EstimatedTokens: 100})
	}
	b, err := e.SubmitBatch(sub)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "callback delivery", 20*time.Second, func() bool { return b.View().Callback.State == api.CallbackDelivered })

	v := b.View()
	c := v.Counts
	if c.Total != n || c.Pending != 0 || c.Succeeded+c.Failed != n {
		t.Fatalf("counts do not add up: %+v", c)
	}
	if c.Failed == 0 || c.Succeeded == 0 || v.Status != api.BatchWithFailures {
		t.Fatalf("expected a mix of outcomes, got status %s counts %+v", v.Status, c)
	}
	if v.Callback.Attempts != 3 {
		t.Fatalf("callback attempts = %d, want 3", v.Callback.Attempts)
	}

	seen := map[string]int{}
	succeeded := 0
	for _, r := range b.items {
		rv := r.View()
		seen[rv.RequestID]++
		if !isFinal(rv.Status) {
			t.Fatalf("request %s is %s after the batch completed", rv.RequestID, rv.Status)
		}
		if rv.Status == api.StateSucceeded {
			succeeded++
		}
	}
	if len(seen) != n || succeeded != c.Succeeded {
		t.Fatalf("%d unique ids (want %d), %d succeeded rows (summary says %d)", len(seen), n, succeeded, c.Succeeded)
	}

	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	want, _ := json.Marshal(v.BatchSummary)
	if string(last) != string(want) {
		t.Fatalf("callback body differs from API status\ncallback: %s\napi:      %s", last, want)
	}
}

// Under overload every request must end up counted in exactly one place.
func TestOverloadIsFullyAccountedFor(t *testing.T) {
	sim := simulator.New(simulator.Config{Models: map[string]api.SimModel{"a": {RPM: 600, TPM: 600_000}}})
	e := New(Config{Models: map[string]api.Limits{"a": {RPM: 600, TPM: 600_000}}, DefaultDeadlineMs: 1000}, sim)
	defer e.Close()

	var accepted, rejected int
	for i := 0; i < 200; i++ {
		_, _, err := e.Submit(api.RequestSpec{RequestID: fmt.Sprintf("r%d", i), Model: "a", EstimatedTokens: 100})
		if err != nil {
			rejected++
		} else {
			accepted++
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("expected some accepted and some rejected, got %d and %d", accepted, rejected)
	}
	waitFor(t, "queue to drain", 5*time.Second, func() bool { return e.Stats(false).Totals.Waiting() == 0 })

	c := e.Stats(false).Totals
	if c.Submitted != 200 || int(c.Rejected) != rejected {
		t.Fatalf("submitted %d rejected %d, want 200 and %d", c.Submitted, c.Rejected, rejected)
	}
	if c.Submitted != c.Succeeded+c.Failed+c.Expired+c.Rejected {
		t.Fatalf("accounting does not balance: %+v", c)
	}
	if got := sim.Stats(false).Models["a"].LimitRejected; got != 0 {
		t.Fatalf("provider refused %d calls for exceeding its limit", got)
	}

	// Submitting an id again returns the original request instead of a duplicate.
	if _, existing, err := e.Submit(api.RequestSpec{RequestID: "r0", Model: "a", EstimatedTokens: 100}); err != nil || !existing {
		t.Fatalf("resubmitting r0: existing=%v err=%v", existing, err)
	}
}

// A limit change applies to a running lane and creates a lane for a new model.
func TestSetLimits(t *testing.T) {
	sim := simulator.New(simulator.Config{})
	e := New(Config{Models: map[string]api.Limits{"a": {RPM: 100, TPM: 1000}}}, sim)
	defer e.Close()

	rpm := int64(50)
	got, err := e.SetLimits("a", api.LimitsPatch{RPM: &rpm})
	if err != nil || got != (api.Limits{RPM: 50, TPM: 1000}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := e.SetLimits("new", api.LimitsPatch{RPM: &rpm}); err == nil {
		t.Fatal("creating a model with only one limit should fail")
	}
	tpm := int64(5000)
	if _, err := e.SetLimits("new", api.LimitsPatch{RPM: &rpm, TPM: &tpm}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Submit(api.RequestSpec{Model: "new", EstimatedTokens: 10}); err != nil {
		t.Fatalf("submit to the new model: %v", err)
	}
}
