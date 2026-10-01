package loadgen

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"hti/internal/api"
	"hti/internal/metrics"
)

type Environment struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPU       string `json:"cpu"`
	Threads   int    `json:"logical_cpus"`
	GoVersion string `json:"go_version"`
}

type generatorTotals struct {
	Sent     int64            `json:"sent"`
	Accepted int64            `json:"accepted"`
	Rejected int64            `json:"rejected"`
	Errors   int64            `json:"send_errors"`
	Ack      metrics.Summary  `json:"ack_latency_ms"`
	Lookup   map[string]int64 `json:"accepted_ids_by_final_status,omitempty"`
}

// Accounting is the evidence that no request was lost or counted twice.
type Accounting struct {
	Generator generatorTotals `json:"generator"`
	Service   api.Counts      `json:"service"`
	Waiting   int64           `json:"waiting"`
	// submitted = succeeded + failed + expired + rejected + waiting
	EquationHolds bool `json:"equation_holds"`
	// The generator's own tally agrees with the service's counters.
	GeneratorAgrees bool `json:"generator_agrees"`
	// Every id the service accepted was found again with a known status.
	UnknownIDs int64 `json:"unknown_ids"`
}

type Throughput struct {
	LoadSeconds           float64 `json:"load_seconds"`   // how long the generator was sending
	ActiveSeconds         float64 `json:"active_seconds"` // first submit to last completion
	CompletedDuringLoad   int64   `json:"completed"`
	CompletedPerSecond    float64 `json:"completed_per_second"`
	MeasuredSeconds       float64 `json:"measured_seconds"` // load window minus warm-up
	MeasuredPerSecond     float64 `json:"completed_per_second_after_warmup"`
	MeasuredPerMinute     float64 `json:"completed_per_minute_after_warmup"`
	PeakCompletedOneSec   int64   `json:"peak_completed_in_one_second"`
	ProviderCallsPerSec   float64 `json:"provider_calls_per_second"`
	SubmittedPerSecondAvg float64 `json:"submitted_per_second"`
}

type Observed struct {
	PeakRPM60 int64 `json:"peak_requests_in_any_60s"`
	PeakTPM60 int64 `json:"peak_tokens_in_any_60s"`
}

type ProviderObserved struct {
	Observed
	Accepted      int64               `json:"accepted"`
	LimitRejected int64               `json:"limit_rejected"`
	Violations    []metrics.Violation `json:"violations"`
}

type TimelineRow struct {
	OffsetS     int64 `json:"offset_s"`
	RPMLimit    int64 `json:"rpm_limit"`
	TPMLimit    int64 `json:"tpm_limit"`
	Sent        int64 `json:"sent_in_10s"`
	SentRPM60   int64 `json:"sent_rolling_60s"`
	TokensTPM60 int64 `json:"tokens_rolling_60s"`
	Completed   int64 `json:"completed_in_10s"`
	Queued      int64 `json:"queued"`
}

type ModelReport struct {
	Limits       api.Limits        `json:"configured_limits_at_end"`
	Counts       api.Counts        `json:"counts"`
	Latency      api.Latency       `json:"latency_ms"`
	Service      Observed          `json:"observed_by_service"`
	Provider     *ProviderObserved `json:"observed_by_provider,omitempty"`
	MinCompleted int64             `json:"min_completed_in_any_60s_after_warmup"`
	Timeline     []TimelineRow     `json:"timeline"`
}

type BatchReport struct {
	Submitted       int             `json:"submitted"`
	Accepted        int             `json:"accepted"`
	AckMs           metrics.Summary `json:"ack_ms"`
	CompletionSec   Spread          `json:"seconds_to_complete"`
	CallbackDelayMs Spread          `json:"ms_from_completion_to_delivered_callback"`
	Attempts        map[int]int     `json:"batches_by_callback_attempts,omitempty"`
	Delivered       int             `json:"callbacks_delivered"`
	IDsExactlyOnce  int             `json:"batches_with_every_id_exactly_once"`
}

type Spread struct {
	Min, P50, Max float64
}

type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type Report struct {
	Name        string                  `json:"name"`
	StartedAt   time.Time               `json:"started_at"`
	EndedAt     time.Time               `json:"ended_at"`
	Environment Environment             `json:"environment"`
	Options     Options                 `json:"options"`
	Assumptions []string                `json:"assumptions"`
	Throughput  Throughput              `json:"throughput"`
	Accounting  Accounting              `json:"accounting"`
	Models      map[string]*ModelReport `json:"models"`
	Changes     []api.LimitChange       `json:"limit_changes"`
	Batches     *BatchReport            `json:"batches,omitempty"`
	Checks      []Check                 `json:"checks"`
}

func (r *Report) AddCheck(name string, pass bool, format string, args ...any) {
	r.Checks = append(r.Checks, Check{name, pass, fmt.Sprintf(format, args...)})
}

func (r *Report) Passed() bool {
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

// cpuName asks the operating system for the processor's marketing name.
func cpuName() string {
	var out []byte
	switch runtime.GOOS {
	case "windows":
		out, _ = exec.Command("reg", "query", `HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "/v", "ProcessorNameString").Output()
		if _, name, ok := strings.Cut(string(out), "REG_SZ"); ok {
			return strings.TrimSpace(name)
		}
	case "darwin":
		out, _ = exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		return strings.TrimSpace(string(out))
	default:
		out, _ = os.ReadFile("/proc/cpuinfo")
		for _, line := range strings.Split(string(out), "\n") {
			if key, name, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "model name" {
				return strings.TrimSpace(name)
			}
		}
	}
	return ""
}

func environment() Environment {
	cpu := cpuName()
	if cpu == "" {
		cpu = "unknown"
	}
	return Environment{OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: cpu, Threads: runtime.NumCPU(), GoVersion: runtime.Version()}
}

func peakIn(pts []metrics.Point, rolled []int64, from, to int64) int64 {
	var peak int64
	for i, p := range pts {
		if p.T >= from && p.T <= to {
			peak = max(peak, rolled[i])
		}
	}
	return peak
}

func spread(v []float64) Spread {
	if len(v) == 0 {
		return Spread{}
	}
	sort.Float64s(v)
	return Spread{Min: v[0], P50: v[len(v)/2], Max: v[len(v)-1]}
}

// minus removes the counts a service had before this run started.
func minus(a, b api.Counts) api.Counts {
	a.Submitted -= b.Submitted
	a.Rejected -= b.Rejected
	a.Succeeded -= b.Succeeded
	a.Failed -= b.Failed
	a.Expired -= b.Expired
	a.ProviderCalls -= b.ProviderCalls
	a.ProviderRateLimited -= b.ProviderRateLimited
	return a
}

func buildReport(o Options, res *Result, before api.ServiceStats, gen generatorTotals) *Report {
	rep := &Report{
		Name: o.Name, StartedAt: res.Start, EndedAt: time.Now(), Environment: environment(), Options: o,
		Models: map[string]*ModelReport{},
		Assumptions: []string{
			"The provider is simulated; no real model is called. Latency and failures come from the simulator's configuration.",
			"Service, simulator and load generator ran on the same machine and share its CPU.",
			"Token usage equals each request's estimated_tokens.",
			"A request counts as completed only when the provider call ended in success or final failure. Expired and rejected requests are not completed.",
		},
	}
	from, loadEnd, end := res.Start.Unix(), res.LoadEnd.Unix(), rep.EndedAt.Unix()
	measuredFrom := res.Start.Add(o.Warmup).Unix()

	var completedLoad, completedMeasured, peak1s, calls, lastCompletion int64
	perSecond := map[int64]int64{}
	for name, ms := range res.Service.Models {
		counts := ms.Counts
		if b := before.Models[name]; b != nil {
			counts = minus(counts, b.Counts)
		}
		if counts.Submitted == 0 && counts.Waiting() == 0 {
			continue // model saw no traffic in this run
		}
		mr := &ModelReport{Limits: ms.Limits, Counts: counts, Latency: ms.Latency}
		pts := ms.Series
		sentRoll := metrics.Rolling(pts, metrics.Sent)
		tokRoll := metrics.Rolling(pts, metrics.Tokens)
		doneRoll := metrics.Rolling(pts, metrics.Completed)
		mr.Service = Observed{PeakRPM60: peakIn(pts, sentRoll, from, end), PeakTPM60: peakIn(pts, tokRoll, from, end)}

		// Lowest completed count in any full 60-second window after warm-up.
		mr.MinCompleted = -1
		for i, p := range pts {
			if p.T-59 >= measuredFrom && p.T < loadEnd {
				if mr.MinCompleted < 0 || doneRoll[i] < mr.MinCompleted {
					mr.MinCompleted = doneRoll[i]
				}
			}
		}
		for i, p := range pts {
			if p.T < from || p.T > end {
				continue
			}
			c := metrics.Completed(p)
			perSecond[p.T] += c
			completedLoad += c
			if c > 0 {
				lastCompletion = max(lastCompletion, p.T)
			}
			if p.T < loadEnd && p.T >= measuredFrom {
				completedMeasured += c
			}
			if off := p.T - from; off%10 == 9 {
				row := TimelineRow{OffsetS: off + 1, RPMLimit: p.RPMLimit, TPMLimit: p.TPMLimit, SentRPM60: sentRoll[i], TokensTPM60: tokRoll[i], Queued: p.Queued}
				for j := max(0, i-9); j <= i; j++ {
					row.Sent += pts[j].Requests
					row.Completed += metrics.Completed(pts[j])
				}
				mr.Timeline = append(mr.Timeline, row)
			}
		}
		calls += counts.ProviderCalls

		if res.Sim != nil {
			if sm := res.Sim.Models[name]; sm != nil {
				sp := sm.Series
				mr.Provider = &ProviderObserved{
					Observed:   Observed{PeakRPM60: peakIn(sp, metrics.Rolling(sp, metrics.Sent), from, end), PeakTPM60: peakIn(sp, metrics.Rolling(sp, metrics.Tokens), from, end)},
					Accepted:   metrics.Sum(metrics.Slice(sp, from, end+1), metrics.Sent),
					Violations: metrics.Violations(sp),
				}
				mr.Provider.LimitRejected = metrics.Sum(metrics.Slice(sp, from, end+1), func(p metrics.Point) int64 { return p.Rejected })
			}
		}
		rep.Models[name] = mr
		rep.Accounting.Service.Add(counts)
	}
	for _, c := range perSecond {
		peak1s = max(peak1s, c)
	}
	for _, ch := range res.Service.LimitChanges {
		if !ch.At.Before(res.Start) {
			rep.Changes = append(rep.Changes, ch)
		}
	}

	// The processing window runs from the first submit to the last completion,
	// which for a batch is long after sending stopped.
	loadSecs := res.LoadEnd.Sub(res.Start).Seconds()
	activeSecs := max(loadSecs, float64(lastCompletion+1-from))
	measuredSecs := float64(loadEnd - measuredFrom)
	rep.Throughput = Throughput{
		LoadSeconds: round1(loadSecs), ActiveSeconds: round1(activeSecs),
		CompletedDuringLoad: completedLoad, CompletedPerSecond: round1(float64(completedLoad) / activeSecs),
		PeakCompletedOneSec: peak1s, ProviderCallsPerSec: round1(float64(calls) / rep.EndedAt.Sub(res.Start).Seconds()),
		SubmittedPerSecondAvg: round1(float64(gen.Sent) / loadSecs),
	}
	if measuredSecs > 0 {
		rep.Throughput.MeasuredSeconds = measuredSecs
		rep.Throughput.MeasuredPerSecond = round1(float64(completedMeasured) / measuredSecs)
		rep.Throughput.MeasuredPerMinute = round1(float64(completedMeasured) / measuredSecs * 60)
	}

	acc := &rep.Accounting
	acc.Generator = gen
	c := acc.Service
	acc.Waiting = c.Waiting()
	acc.EquationHolds = c.Submitted == c.Succeeded+c.Failed+c.Expired+c.Rejected+acc.Waiting
	acc.GeneratorAgrees = gen.Errors == 0 && gen.Sent == c.Submitted && gen.Rejected == c.Rejected
	acc.UnknownIDs = gen.Lookup["unknown"] + gen.Lookup["lookup_error"]
	rep.AddCheck("Every request is accounted for", acc.EquationHolds && acc.GeneratorAgrees && acc.UnknownIDs == 0,
		"submitted %d = succeeded %d + failed %d + expired %d + rejected %d + waiting %d; generator sent %d (rejected %d, send errors %d); unknown ids %d",
		c.Submitted, c.Succeeded, c.Failed, c.Expired, c.Rejected, acc.Waiting, gen.Sent, gen.Rejected, gen.Errors, acc.UnknownIDs)

	if len(res.Batches) > 0 {
		br := &BatchReport{Submitted: len(res.Batches), AckMs: gen.Ack, Attempts: map[int]int{}}
		var completion, delay []float64
		for _, b := range res.Batches {
			if !b.Accepted || b.View == nil {
				continue
			}
			br.Accepted++
			if b.IDsMatch {
				br.IDsExactlyOnce++
			}
			if b.View.CompletedAt != nil {
				completion = append(completion, b.View.CompletedAt.Sub(b.View.CreatedAt).Seconds())
			}
			if b.View.Callback.State == api.CallbackDelivered {
				br.Delivered++
				br.Attempts[b.View.Callback.Attempts]++
				delay = append(delay, float64(b.View.Callback.DeliveredAt.Sub(*b.View.CompletedAt).Milliseconds()))
			}
		}
		br.CompletionSec, br.CallbackDelayMs = spread(completion), spread(delay)
		rep.Batches = br
	}
	return rep
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// Write saves the report as JSON and Markdown, plus the raw per-second series.
func (res *Result) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	save := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	if err := save("report.json", res.Report); err != nil {
		return err
	}
	raw := map[string]any{"service": res.Service, "simulator": res.Sim, "batches": res.Batches}
	if err := save("raw.json", raw); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report.Markdown()), 0o644)
}

func num(v int64) string {
	s := fmt.Sprint(v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func pct(a, b int64) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

// Markdown renders the report for people.
func (r *Report) Markdown() string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	verdict := "PASS"
	if !r.Passed() {
		verdict = "FAIL"
	}
	p("# %s: %s", r.Name, verdict)
	p("")
	p("Run from %s to %s. Everything below was measured in this run; nothing is projected.", r.StartedAt.Format(time.RFC3339), r.EndedAt.Format(time.RFC3339))
	p("")
	p("## Checks")
	p("")
	p("| Result | Check | Evidence |")
	p("|---|---|---|")
	for _, c := range r.Checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		p("| %s | %s | %s |", mark, c.Name, c.Detail)
	}
	p("")
	p("## Requests")
	p("")
	c := r.Accounting.Service
	p("| Submitted | Completed | Successful | Failed | Expired | Rejected | Still waiting |")
	p("|---:|---:|---:|---:|---:|---:|---:|")
	p("| %s | %s | %s | %s | %s | %s | %s |", num(c.Submitted), num(c.Completed()), num(c.Succeeded), num(c.Failed), num(c.Expired), num(c.Rejected), num(r.Accounting.Waiting))
	p("")
	t := r.Throughput
	p("- Achieved completion rate: **%.1f per second** (%s completed in the %.0f seconds from first submit to last completion; sending lasted %.0f seconds).", t.CompletedPerSecond, num(t.CompletedDuringLoad), t.ActiveSeconds, t.LoadSeconds)
	if t.MeasuredSeconds > 0 && r.Options.Warmup > 0 {
		p("- After the %.0f-second warm-up: **%.1f per second**, %.0f per minute.", r.Options.Warmup.Seconds(), t.MeasuredPerSecond, t.MeasuredPerMinute)
	}
	p("- Highest completions in a single second: %s. Provider calls per second including retries: %.1f.", num(t.PeakCompletedOneSec), t.ProviderCallsPerSec)
	p("- Submitted at %.1f per second. Submit acknowledgement latency: p50 %.2f ms, p99 %.2f ms.", t.SubmittedPerSecondAvg, r.Accounting.Generator.Ack.P50Ms, r.Accounting.Generator.Ack.P99Ms)
	if len(r.Accounting.Generator.Lookup) > 0 {
		keys := make([]string, 0, len(r.Accounting.Generator.Lookup))
		for k := range r.Accounting.Generator.Lookup {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %s", k, num(r.Accounting.Generator.Lookup[k])))
		}
		p("- Accepted request ids looked up one by one after the run: %s.", strings.Join(parts, ", "))
	}
	p("")

	names := make([]string, 0, len(r.Models))
	for name := range r.Models {
		names = append(names, name)
	}
	sort.Strings(names)

	p("## Limits and observed throughput per model")
	p("")
	p("Observed values are the largest total in any 60 consecutive seconds. The provider columns come from the simulator's own counters.")
	p("")
	p("| Model | RPM limit | Sent, peak 60 s | Provider accepted, peak 60 s | TPM limit | Tokens, peak 60 s | Provider tokens, peak 60 s | Refused by provider (429) | Windows over limit |")
	p("|---|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, name := range names {
		m := r.Models[name]
		pr := m.Provider
		if pr == nil {
			pr = &ProviderObserved{}
		}
		p("| %s | %s | %s | %s | %s | %s | %s | %s | %d |", name, num(m.Limits.RPM), num(m.Service.PeakRPM60), num(pr.PeakRPM60),
			num(m.Limits.TPM), num(m.Service.PeakTPM60), num(pr.PeakTPM60), num(pr.LimitRejected), len(pr.Violations))
	}
	if len(r.Changes) > 0 {
		p("")
		p("Limit changes during the run:")
		p("")
		for _, ch := range r.Changes {
			p("- +%.0f s: %s set to %s RPM, %s TPM", ch.At.Sub(r.StartedAt).Seconds(), ch.Model, num(ch.RPM), num(ch.TPM))
		}
	}
	p("")
	p("## Latency (milliseconds)")
	p("")
	p("Queue is submit to first send. Provider is one provider call. Total is submit to final state, including waiting and retries.")
	p("")
	p("| Model | Stage | p50 | p95 | p99 | Max | Samples |")
	p("|---|---|---:|---:|---:|---:|---:|")
	for _, name := range names {
		l := r.Models[name].Latency
		for _, row := range []struct {
			stage string
			s     metrics.Summary
		}{{"queue", l.Queue}, {"provider", l.Provider}, {"total", l.Total}} {
			p("| %s | %s | %.1f | %.1f | %.1f | %.1f | %s |", name, row.stage, row.s.P50Ms, row.s.P95Ms, row.s.P99Ms, row.s.MaxMs, num(int64(row.s.Count)))
		}
	}
	p("")
	p("## Per-model outcomes")
	p("")
	p("| Model | Submitted | Successful | Failed | Expired | Rejected | Provider calls | Lowest completed in any 60 s after warm-up |")
	p("|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, name := range names {
		m := r.Models[name]
		lowest := "n/a"
		if m.MinCompleted >= 0 {
			lowest = num(m.MinCompleted)
		}
		p("| %s | %s | %s | %s | %s | %s | %s | %s |", name, num(m.Counts.Submitted), num(m.Counts.Succeeded), num(m.Counts.Failed), num(m.Counts.Expired), num(m.Counts.Rejected), num(m.Counts.ProviderCalls), lowest)
	}

	if bt := r.Batches; bt != nil {
		p("")
		p("## Batches and callbacks")
		p("")
		p("- Batches submitted: %d, accepted: %d.", bt.Submitted, bt.Accepted)
		p("- Acknowledgement time: p50 %.1f ms, max %.1f ms.", bt.AckMs.P50Ms, bt.AckMs.MaxMs)
		p("- Time to complete a batch: min %.1f s, median %.1f s, max %.1f s.", bt.CompletionSec.Min, bt.CompletionSec.P50, bt.CompletionSec.Max)
		p("- Callbacks delivered: %d of %d. Time from batch completion to the accepted delivery: min %.0f ms, median %.0f ms, max %.0f ms.", bt.Delivered, bt.Accepted, bt.CallbackDelayMs.Min, bt.CallbackDelayMs.P50, bt.CallbackDelayMs.Max)
		attempts := make([]int, 0, len(bt.Attempts))
		for a := range bt.Attempts {
			attempts = append(attempts, a)
		}
		sort.Ints(attempts)
		for _, a := range attempts {
			p("- %d batch(es) needed %d delivery attempt(s).", bt.Attempts[a], a)
		}
		p("- Batches whose results contain every submitted id exactly once: %d of %d.", bt.IDsExactlyOnce, bt.Accepted)
	}

	p("")
	p("## Throughput over time")
	p("")
	p("One row per 10 seconds. \"Draining\" marks the period after a limit was lowered: the last 60 seconds still hold sends made under the old limit, so the service sends nothing until they age out.")
	for _, name := range names {
		m := r.Models[name]
		p("")
		p("### %s", name)
		p("")
		p("| Time | RPM limit | Sent in 10 s | Sent, last 60 s | Share of limit | Tokens, last 60 s | Completed in 10 s | Queued |")
		p("|---:|---:|---:|---:|---:|---:|---:|---:|")
		for _, row := range m.Timeline {
			share := pct(row.SentRPM60, row.RPMLimit)
			if row.SentRPM60 > row.RPMLimit {
				share = "draining"
			}
			p("| %d s | %s | %s | %s | %s | %s | %s | %s |", row.OffsetS, num(row.RPMLimit), num(row.Sent), num(row.SentRPM60), share, num(row.TokensTPM60), num(row.Completed), num(row.Queued))
		}
	}

	p("")
	p("## Run configuration")
	p("")
	o := r.Options
	shares := make([]string, len(o.Models))
	for i, m := range o.Models {
		shares[i] = fmt.Sprintf("%s=%g", m.Name, m.Weight)
	}
	p("- Rate %.0f requests per second for %s (ramp-up %s), model mix %s, batch size %d, about %d tokens per request (jitter %.0f%%).",
		o.Rate, o.Duration, o.RampUp, strings.Join(shares, ", "), o.BatchSize, o.Tokens, o.Jitter*100)
	e := r.Environment
	p("- Environment: %s/%s, %d logical CPUs (%s), %s.", e.OS, e.Arch, e.Threads, e.CPU, e.GoVersion)
	p("")
	p("## Simulation assumptions")
	p("")
	for _, a := range r.Assumptions {
		p("- %s", a)
	}
	return b.String()
}
