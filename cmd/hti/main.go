// Command hti runs every part of the system:
//
//	hti simulator   the provider simulator
//	hti serve       the inference service
//	hti loadgen     the load generator
//	hti limits      change a model's limits on a running service and simulator
//	hti scenario    run a validation scenario end to end and print pass or fail
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hti/internal/api"
	"hti/internal/loadgen"
	"hti/internal/provider"
	"hti/internal/scenario"
	"hti/internal/service"
	"hti/internal/simulator"
)

const usage = `usage: hti <command> [flags]

  simulator   run the provider simulator          (default 127.0.0.1:9090)
  serve       run the inference service           (default 127.0.0.1:8080)
  loadgen     send load and write a report
  limits      change a model's RPM and TPM while everything is running
  scenario    run validation scenario 1, 2, 3 or all

Run "hti <command> -h" for the flags of a command.`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "simulator":
		err = runSimulator(ctx, args)
	case "serve":
		err = runServe(ctx, args)
	case "loadgen":
		err = runLoadgen(ctx, args)
	case "limits":
		err = runLimits(args)
	case "scenario":
		err = runScenario(ctx, args)
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) {
	fmt.Printf(time.Now().Format("15:04:05 ")+format+"\n", args...)
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// serveUntilDone runs an HTTP server until the context is cancelled (Ctrl+C).
func serveUntilDone(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return srv.Close()
	}
}

func runSimulator(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("simulator", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/simulator.json", "simulator configuration file")
	addr := fs.String("addr", "127.0.0.1:9090", "address to listen on")
	fs.Parse(args)
	var cfg simulator.Config
	if err := loadJSON(*cfgPath, &cfg); err != nil {
		return err
	}
	logf("provider simulator listening on http://%s with %d model(s)", *addr, len(cfg.Models))
	return serveUntilDone(ctx, *addr, simulator.New(cfg).Handler())
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/service.json", "service configuration file")
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	fs.Parse(args)
	var cfg service.Config
	if err := loadJSON(*cfgPath, &cfg); err != nil {
		return err
	}
	if cfg.ProviderURL == "" {
		cfg.ProviderURL = "http://127.0.0.1:9090"
	}
	eng := service.New(cfg, provider.NewHTTP(cfg.ProviderURL))
	defer eng.Close()
	logf("inference service listening on http://%s, provider at %s, %d model(s)", *addr, cfg.ProviderURL, len(cfg.Models))
	return serveUntilDone(ctx, *addr, eng.Handler())
}

// parseModels reads "model-a=0.6,model-b=0.4". A bare name gets weight 1.
func parseModels(s string) ([]loadgen.ModelShare, error) {
	var out []loadgen.ModelShare
	for _, part := range strings.Split(s, ",") {
		name, weight, found := strings.Cut(strings.TrimSpace(part), "=")
		w := 1.0
		if found {
			var err error
			if w, err = strconv.ParseFloat(weight, 64); err != nil || w <= 0 {
				return nil, fmt.Errorf("bad model weight %q", part)
			}
		}
		if name != "" {
			out = append(out, loadgen.ModelShare{Name: name, Weight: w})
		}
	}
	return out, nil
}

func runLoadgen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("loadgen", flag.ExitOnError)
	var o loadgen.Options
	fs.StringVar(&o.ServiceURL, "url", "http://127.0.0.1:8080", "service URL")
	fs.StringVar(&o.SimURL, "simulator-url", "http://127.0.0.1:9090", "simulator URL, for the provider's own counters (empty to skip)")
	fs.Float64Var(&o.Rate, "rate", 1000, "requests per second")
	fs.DurationVar(&o.Duration, "duration", 60*time.Second, "how long to send")
	fs.DurationVar(&o.RampUp, "ramp-up", 0, "time to climb from zero to the full rate")
	fs.DurationVar(&o.Warmup, "warmup", 0, "initial period excluded from the after-warm-up figures")
	fs.DurationVar(&o.Drain, "drain", 60*time.Second, "how long to wait for outstanding work after sending stops")
	models := fs.String("models", "model-a", `model mix, for example "model-a=0.6,model-b=0.4"`)
	fs.IntVar(&o.BatchSize, "batch-size", 1, "1 sends single requests; larger sends batches of this many")
	fs.Int64Var(&o.Tokens, "tokens", 1000, "approximate tokens per request")
	fs.Float64Var(&o.Jitter, "token-jitter", 0, "vary tokens per request by up to this fraction (0.2 = within 20%)")
	fs.Int64Var(&o.DeadlineMs, "deadline-ms", 0, "per-request deadline; 0 uses the service default")
	fs.BoolVar(&o.Callback, "callback", false, "start a callback receiver and attach it to every batch")
	fs.StringVar(&o.CallbackAddr, "callback-addr", "127.0.0.1:9191", "address for the callback receiver")
	fs.IntVar(&o.CallbackFailFirst, "callback-fail-first", 0, "reject this many callback deliveries per batch before accepting")
	fs.IntVar(&o.Concurrency, "concurrency", 4096, "maximum submits in progress at once")
	fs.StringVar(&o.Name, "name", "loadgen", "name for the report")
	out := fs.String("out", "reports", "directory for reports")
	fs.Parse(args)

	var err error
	if o.Models, err = parseModels(*models); err != nil {
		return err
	}
	o.Log = logf
	res, err := loadgen.Run(ctx, o)
	if err != nil {
		return err
	}
	dir := filepath.Join(*out, "loadgen-"+res.Start.Format("20060102-150405"))
	if err := res.Write(dir); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println(res.Report.Markdown())
	logf("report written to %s", filepath.Join(dir, "report.md"))
	return nil
}

func runLimits(args []string) error {
	fs := flag.NewFlagSet("limits", flag.ExitOnError)
	svc := fs.String("url", "http://127.0.0.1:8080", "service URL")
	sim := fs.String("simulator-url", "http://127.0.0.1:9090", "simulator URL (empty to change the service only)")
	model := fs.String("model", "", "model to change")
	rpm := fs.Int64("rpm", -1, "new requests-per-minute limit")
	tpm := fs.Int64("tpm", -1, "new tokens-per-minute limit")
	fs.Parse(args)
	if *model == "" || (*rpm < 0 && *tpm < 0) {
		return fmt.Errorf("give --model and at least one of --rpm, --tpm")
	}
	resp, err := http.Get(*svc + "/v1/stats?series=false")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var st api.ServiceStats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return err
	}
	m := st.Models[*model]
	if m == nil {
		return fmt.Errorf("the service has no model %q", *model)
	}
	next := m.Limits
	if *rpm >= 0 {
		next.RPM = *rpm
	}
	if *tpm >= 0 {
		next.TPM = *tpm
	}
	if err := scenario.SetLimits(*svc, *sim, *model, m.Limits, next); err != nil {
		return err
	}
	logf("%s: %d RPM, %d TPM  ->  %d RPM, %d TPM", *model, m.Limits.RPM, m.Limits.TPM, next.RPM, next.TPM)
	return nil
}

func runScenario(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scenario", flag.ExitOnError)
	short := fs.Bool("short", false, "shorter phases for a quick check")
	host := fs.String("host", "127.0.0.1", "interface to listen on")
	port := fs.Int("base-port", 19090, "first of three consecutive local ports to use")
	out := fs.String("out", "reports", "directory for reports")
	// Accept "scenario 1 --short" as well as "scenario --short 1".
	var which []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		which, args = append(which, args[0]), args[1:]
	}
	fs.Parse(args)
	which = append(which, fs.Args()...)
	if len(which) == 0 {
		return fmt.Errorf("say which scenario: 1, 2, 3 or all")
	}
	if which[0] == "all" {
		which = []string{"1", "2", "3"}
	}
	run := map[string]func(context.Context, scenario.Options) (*loadgen.Result, error){"1": scenario.One, "2": scenario.Two, "3": scenario.Three}
	failed := false
	for _, w := range which {
		f := run[w]
		if f == nil {
			return fmt.Errorf("unknown scenario %q", w)
		}
		res, err := f(ctx, scenario.Options{Short: *short, Host: *host, BasePort: *port, OutDir: *out, Log: logf})
		if err != nil {
			return fmt.Errorf("scenario %s: %w", w, err)
		}
		verdict := "PASS"
		if !res.Report.Passed() {
			verdict, failed = "FAIL", true
		}
		logf("scenario %s: %s", w, verdict)
		time.Sleep(time.Second) // let the ports close before the next scenario
	}
	if failed {
		os.Exit(1)
	}
	return nil
}
