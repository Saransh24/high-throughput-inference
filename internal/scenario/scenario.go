// Package scenario runs the three validation scenarios from the assignment.
//
// Each scenario starts a simulator and a service on local ports, drives them
// with the load generator over real HTTP, checks the pass criteria against
// the collected data, and writes a report.
package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"hti/internal/api"
	"hti/internal/loadgen"
	"hti/internal/metrics"
	"hti/internal/provider"
	"hti/internal/service"
	"hti/internal/simulator"
)

type Options struct {
	Short    bool   // shorter phases, for a quick check; the full run is the evidence
	Host     string // interface to listen on
	BasePort int    // simulator on BasePort, service on +1, callback receiver on +2
	OutDir   string
	Log      func(format string, args ...any)
}

// Stack is a simulator and a service wired together over HTTP.
type Stack struct {
	Sim        *simulator.Sim
	Engine     *service.Engine
	SimURL     string
	ServiceURL string
	servers    []*http.Server
}

func StartStack(simCfg simulator.Config, svcCfg service.Config, simAddr, svcAddr string) (*Stack, error) {
	st := &Stack{Sim: simulator.New(simCfg)}
	listen := func(addr string, h http.Handler) (string, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return "", err
		}
		srv := &http.Server{Handler: h}
		st.servers = append(st.servers, srv)
		go srv.Serve(ln)
		return "http://" + ln.Addr().String(), nil
	}
	var err error
	if st.SimURL, err = listen(simAddr, st.Sim.Handler()); err != nil {
		return nil, fmt.Errorf("simulator: %w", err)
	}
	svcCfg.ProviderURL = st.SimURL
	st.Engine = service.New(svcCfg, provider.NewHTTP(st.SimURL))
	if st.ServiceURL, err = listen(svcAddr, st.Engine.Handler()); err != nil {
		st.Close()
		return nil, fmt.Errorf("service: %w", err)
	}
	return st, nil
}

func (st *Stack) Close() {
	if st.Engine != nil {
		st.Engine.Close()
	}
	for _, s := range st.servers {
		s.Close()
	}
}

func put(url string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: %s", url, resp.Status)
	}
	return nil
}

// SetLimits changes a model's limits on the service and the simulator through
// their HTTP APIs, in an order that never lets the service send against a
// limit the provider has already lowered: the service first takes the lower
// of old and new, then the provider changes, then the service takes the new
// values.
func SetLimits(serviceURL, simURL, model string, old, next api.Limits) error {
	low := api.Limits{RPM: min(old.RPM, next.RPM), TPM: min(old.TPM, next.TPM)}
	if err := put(serviceURL+"/v1/models/"+model+"/limits", low); err != nil {
		return err
	}
	if simURL != "" {
		if err := put(simURL+"/admin/models/"+model, next); err != nil {
			return err
		}
	}
	return put(serviceURL+"/v1/models/"+model+"/limits", next)
}

var latency = api.LatencyConfig{Kind: "lognormal", P50Ms: 80, P99Ms: 400}

func (o *Options) addrs() (sim, svc, sink string) {
	return fmt.Sprintf("%s:%d", o.Host, o.BasePort), fmt.Sprintf("%s:%d", o.Host, o.BasePort+1), fmt.Sprintf("%s:%d", o.Host, o.BasePort+2)
}

func (o *Options) finish(res *loadgen.Result, name string) error {
	dir := filepath.Join(o.OutDir, name+"-"+res.Start.Format("20060102-150405"))
	if err := res.Write(dir); err != nil {
		return err
	}
	o.Log("")
	for _, c := range res.Report.Checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		o.Log("  [%s] %s: %s", mark, c.Name, c.Detail)
	}
	o.Log("")
	o.Log("report written to %s", filepath.Join(dir, "report.md"))
	return nil
}

// withinLimits adds the check that the provider never saw a model over its limit.
func withinLimits(rep *loadgen.Report, models ...string) {
	pass := true
	detail := ""
	for _, name := range models {
		m := rep.Models[name]
		if m == nil || m.Provider == nil {
			pass = false
			detail += name + ": no provider data; "
			continue
		}
		p := m.Provider
		if len(p.Violations) > 0 || p.LimitRejected > 0 {
			pass = false
		}
		detail += fmt.Sprintf("%s: peak %s requests and %s tokens in any 60 s, %d windows over limit, %d calls refused by the provider; ",
			name, comma(p.PeakRPM60), comma(p.PeakTPM60), len(p.Violations), p.LimitRejected)
	}
	rep.AddCheck("No 60-second period exceeds a configured limit", pass, "%s", detail)
}

func comma(v int64) string {
	s := fmt.Sprint(v)
	out := ""
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(ch)
	}
	return out
}

// One: a single model at 50,000 RPM and 100M TPM, overloaded with
// 1,000-token requests.
func One(ctx context.Context, o Options) (*loadgen.Result, error) {
	limits := api.Limits{RPM: 50_000, TPM: 100_000_000}
	warmup, measure := 60*time.Second, 300*time.Second
	if o.Short {
		measure = 90 * time.Second
	}
	simAddr, svcAddr, sinkAddr := o.addrs()
	st, err := StartStack(
		simulator.Config{Models: map[string]api.SimModel{"model-a": {RPM: limits.RPM, TPM: limits.TPM, Latency: latency}}},
		service.Config{Models: map[string]api.Limits{"model-a": limits}},
		simAddr, svcAddr)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	o.Log("scenario 1: model-a at %s RPM / %s TPM, sending 1,000 requests per second (120%% of capacity) for %s", comma(limits.RPM), comma(limits.TPM), warmup+measure)
	res, err := loadgen.Run(ctx, loadgen.Options{
		Name: "Scenario 1: reach the available provider capacity", ServiceURL: st.ServiceURL, SimURL: st.SimURL,
		Rate: 1000, Duration: warmup + measure, Warmup: warmup, Drain: 60 * time.Second,
		Models: []loadgen.ModelShare{{Name: "model-a", Weight: 1}}, Tokens: 1000, CallbackAddr: sinkAddr, Log: o.Log,
	})
	if err != nil {
		return nil, err
	}
	rep := res.Report
	m := rep.Models["model-a"]
	need := limits.RPM * 9 / 10
	rep.AddCheck("At least 90% of allowed request capacity is completed after warm-up",
		m != nil && m.MinCompleted >= need && rep.Throughput.MeasuredPerMinute >= float64(need),
		"average %.0f completed per minute after warm-up (%.1f%% of %s); lowest 60-second window %s; required %s",
		rep.Throughput.MeasuredPerMinute, 100*rep.Throughput.MeasuredPerMinute/float64(limits.RPM), comma(limits.RPM), comma(m.MinCompleted), comma(need))
	withinLimits(rep, "model-a")
	return res, o.finish(res, "scenario-1")
}

// Two: two models with separate limits; model A is cut to 5,000 RPM during
// the run and later restored, while model B is left alone.
func Two(ctx context.Context, o Options) (*loadgen.Result, error) {
	a := api.Limits{RPM: 30_000, TPM: 60_000_000}
	aLow := api.Limits{RPM: 5_000, TPM: 60_000_000}
	b := api.Limits{RPM: 20_000, TPM: 40_000_000}
	base, constrained, restored := 120*time.Second, 180*time.Second, 180*time.Second
	if o.Short {
		base, constrained, restored = 75*time.Second, 130*time.Second, 80*time.Second
	}
	simAddr, svcAddr, sinkAddr := o.addrs()
	st, err := StartStack(
		simulator.Config{Models: map[string]api.SimModel{
			"model-a": {RPM: a.RPM, TPM: a.TPM, Latency: latency},
			"model-b": {RPM: b.RPM, TPM: b.TPM, Latency: latency},
		}},
		service.Config{Models: map[string]api.Limits{"model-a": a, "model-b": b}},
		simAddr, svcAddr)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	startedBefore := st.Engine.Stats(false).StartedAt

	o.Log("scenario 2: model-a %s RPM, model-b %s RPM; model-a drops to %s RPM at %s and is restored at %s",
		comma(a.RPM), comma(b.RPM), comma(aLow.RPM), base, base+constrained)
	var cutAt, restoreAt time.Time
	var changeErr error
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(base):
		}
		changeErr = SetLimits(st.ServiceURL, st.SimURL, "model-a", a, aLow)
		cutAt = time.Now()
		o.Log(">>> model-a limit lowered to %s RPM", comma(aLow.RPM))
		select {
		case <-ctx.Done():
			return
		case <-time.After(constrained):
		}
		if err := SetLimits(st.ServiceURL, st.SimURL, "model-a", aLow, a); err != nil {
			changeErr = err
		}
		restoreAt = time.Now()
		o.Log(">>> model-a limit restored to %s RPM", comma(a.RPM))
	}()

	res, err := loadgen.Run(ctx, loadgen.Options{
		Name: "Scenario 2: different models and changing limits", ServiceURL: st.ServiceURL, SimURL: st.SimURL,
		Rate: 1000, Duration: base + constrained + restored, Warmup: 60 * time.Second, Drain: 60 * time.Second,
		Models: []loadgen.ModelShare{{Name: "model-a", Weight: 0.6}, {Name: "model-b", Weight: 0.4}},
		Tokens: 1000, CallbackAddr: sinkAddr, Log: o.Log,
	})
	if err != nil {
		return nil, err
	}
	if changeErr != nil || cutAt.IsZero() || restoreAt.IsZero() {
		return nil, fmt.Errorf("limit changes were not applied: %v", changeErr)
	}
	rep := res.Report
	withinLimits(rep, "model-a", "model-b")

	// What the provider saw from model A around each change.
	sp := res.Sim.Models["model-a"].Series
	sentBetween := func(from, to time.Time) int64 {
		return metrics.Sum(metrics.Slice(sp, from.Unix(), to.Unix()), metrics.Sent)
	}
	paused := sentBetween(cutAt.Add(2*time.Second), cutAt.Add(40*time.Second))
	lowMinute := sentBetween(restoreAt.Add(-61*time.Second), restoreAt.Add(-1*time.Second))
	rep.AddCheck("Model A adjusts to the lower limit without a restart",
		paused == 0 && lowMinute >= aLow.RPM*9/10 && lowMinute <= aLow.RPM,
		"sent %d requests in the 2 to 40 seconds after the cut (the last minute was already over the new limit, so it must wait); then %s in the final constrained minute against a limit of %s",
		paused, comma(lowMinute), comma(aLow.RPM))

	quick := sentBetween(restoreAt.Add(2*time.Second), restoreAt.Add(12*time.Second))
	highMinute := sentBetween(res.LoadEnd.Add(-61*time.Second), res.LoadEnd.Add(-1*time.Second))
	rep.AddCheck("Model A adjusts to the restored limit without a restart",
		quick >= a.RPM/6*9/10 && highMinute >= a.RPM*9/10 && highMinute <= a.RPM,
		"sent %s requests in seconds 2 to 12 after the restore (full pace is %s per 10 s); %s in the final minute against a limit of %s",
		comma(quick), comma(a.RPM/6), comma(highMinute), comma(a.RPM))

	startedAfter := st.Engine.Stats(false).StartedAt
	mb := rep.Models["model-b"]
	rep.AddCheck("Model B continues processing while Model A is constrained",
		mb != nil && mb.MinCompleted >= b.RPM*9/10,
		"model-b's lowest completed count in any 60-second window after warm-up was %s (limit %s, required %s)",
		comma(mb.MinCompleted), comma(b.RPM), comma(b.RPM*9/10))
	rep.AddCheck("The service was not restarted", startedBefore.Equal(startedAfter) && startedBefore.Before(res.Start),
		"service start time %s is unchanged and earlier than the run", startedBefore.Format(time.TimeOnly))
	rep.AddCheck("The report shows configured limits and observed throughput over time", len(rep.Models["model-a"].Timeline) > 0,
		"see the per-model tables under \"Throughput over time\": one row per 10 seconds with the limit in force and the observed rate")
	return res, o.finish(res, "scenario-2")
}

// Three: one batch of 10,000 requests over two models with transient and
// permanent provider failures; the callback receiver rejects the first two
// deliveries.
func Three(ctx context.Context, o Options) (*loadgen.Result, error) {
	a := api.Limits{RPM: 30_000, TPM: 60_000_000}
	b := api.Limits{RPM: 20_000, TPM: 40_000_000}
	simAddr, svcAddr, sinkAddr := o.addrs()
	st, err := StartStack(
		simulator.Config{Models: map[string]api.SimModel{
			"model-a": {RPM: a.RPM, TPM: a.TPM, Latency: latency, TransientRate: 0.05, PermanentRate: 0.02},
			"model-b": {RPM: b.RPM, TPM: b.TPM, Latency: latency, TransientRate: 0.05, PermanentRate: 0.02},
		}},
		service.Config{Models: map[string]api.Limits{"model-a": a, "model-b": b}},
		simAddr, svcAddr)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	const size = 10_000
	o.Log("scenario 3: one batch of %s requests across model-a and model-b, 5%% transient and 2%% permanent failures, first two callbacks rejected", comma(size))
	res, err := loadgen.Run(ctx, loadgen.Options{
		Name: "Scenario 3: asynchronous batch completion", ServiceURL: st.ServiceURL, SimURL: st.SimURL,
		Rate: size, Duration: time.Second, BatchSize: size, Drain: 180 * time.Second,
		Models:   []loadgen.ModelShare{{Name: "model-a", Weight: 0.5}, {Name: "model-b", Weight: 0.5}},
		Tokens:   1000,
		Callback: true, CallbackAddr: sinkAddr, CallbackFailFirst: 2, Log: o.Log,
	})
	if err != nil {
		return nil, err
	}
	rep := res.Report
	if len(res.Batches) != 1 || res.Batches[0].View == nil {
		return nil, fmt.Errorf("expected one accepted batch, got %d", len(res.Batches))
	}
	br := res.Batches[0]
	v := br.View

	rep.AddCheck("The acknowledgement is returned within one second", br.Accepted && br.AckMs < 1000,
		"POST /v1/batches with %s requests answered %d in %.1f ms", comma(int64(br.Size)), br.HTTPStatus, br.AckMs)

	ds := br.Deliveries
	afterFinal := len(ds) > 0 && br.LastFinalAt != nil && v.CompletedAt != nil && !ds[0].At.Before(*br.LastFinalAt) && !ds[0].At.Before(*v.CompletedAt)
	detail := "no callback received"
	if len(ds) > 0 && br.LastFinalAt != nil {
		detail = fmt.Sprintf("last request reached a final state at %s; first callback attempt arrived at %s (%d ms later)",
			br.LastFinalAt.Format("15:04:05.000"), ds[0].At.Format("15:04:05.000"), ds[0].At.Sub(*br.LastFinalAt).Milliseconds())
	}
	rep.AddCheck("The callback is sent only after all requests reach a final state", afterFinal, "%s", detail)

	recovered := len(ds) == 3 && ds[0].Responded == 503 && ds[1].Responded == 503 && ds[2].Responded == 200 &&
		v.Callback.State == api.CallbackDelivered && v.Callback.Attempts == 3
	signed := true
	codes := ""
	for _, d := range ds {
		signed = signed && d.SignatureOK
		codes += fmt.Sprintf("%d ", d.Responded)
	}
	rep.AddCheck("Callback delivery succeeds after the destination recovers", recovered && signed,
		"receiver answered %s; service records state %q after %d attempts; all signatures valid: %v", codes, v.Callback.State, v.Callback.Attempts, signed)

	matches := false
	if len(ds) > 0 {
		got, _ := json.Marshal(ds[len(ds)-1].Body)
		want, _ := json.Marshal(v.BatchSummary)
		matches = bytes.Equal(got, want)
	}
	c := v.Counts
	rep.AddCheck("The callback summary matches the batch status returned by the API", matches,
		"both report status %q, total %d, succeeded %d, failed %d (permanent %d, retries exhausted %d, expired %d)",
		v.Status, c.Total, c.Succeeded, c.Failed, c.FailedByReason[api.ReasonPermanent], c.FailedByReason[api.ReasonExhausted], c.FailedByReason[api.ReasonExpired])

	countsAgree := br.StatusCounts[api.StateSucceeded] == c.Succeeded && c.Succeeded+c.Failed == c.Total && c.Pending == 0
	rep.AddCheck("Every request id appears exactly once in the final batch result", br.IDsMatch && countsAgree,
		"%d result rows, %d unique ids, all %d submitted ids present once; per-request statuses add up to the summary: %v",
		br.ResultRows, br.UniqueIDs, br.Size, countsAgree)

	var transient, permanent int64
	for _, sm := range res.Sim.Models {
		transient += sm.Transient
		permanent += sm.Permanent
	}
	rep.AddCheck("Transient and permanent failures were exercised", transient > 0 && permanent > 0 && c.Failed > 0,
		"the provider returned %d transient and %d permanent failures; the service made %d retry calls; %d requests ended failed",
		transient, permanent, rep.Accounting.Service.ProviderCalls-int64(c.Total), c.Failed)
	withinLimits(rep, "model-a", "model-b")
	return res, o.finish(res, "scenario-3")
}
