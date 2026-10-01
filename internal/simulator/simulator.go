// Package simulator is a stand-in for an inference provider.
//
// Each model has its own RPM and TPM limits, response latency and failure
// mix. The simulator keeps its own sliding 60-second counters, independent of
// the service's limiter, refuses calls that would exceed a limit with 429, and
// counts how often that happened. A run is clean when that count is zero.
package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"hti/internal/api"
	"hti/internal/metrics"
	"hti/internal/provider"
	"hti/internal/window"
)

type Config struct {
	Models map[string]api.SimModel `json:"models"`
}

type model struct {
	mu     sync.Mutex
	cfg    api.SimModel
	reqWin *window.Sliding
	tokWin *window.Sliding
	series *metrics.Series

	accepted, tokens, limitRejected int64
	succeeded, transient, permanent int64
}

type Sim struct {
	started time.Time
	mu      sync.RWMutex
	models  map[string]*model
}

func New(cfg Config) *Sim {
	s := &Sim{started: time.Now(), models: map[string]*model{}}
	for name, mc := range cfg.Models {
		s.models[name] = newModel(mc)
	}
	return s
}

func newModel(mc api.SimModel) *model {
	return &model{
		cfg:    mc,
		reqWin: window.New(60*time.Second, 100*time.Millisecond),
		tokWin: window.New(60*time.Second, 100*time.Millisecond),
		series: metrics.NewSeries(mc.RPM, mc.TPM),
	}
}

func (s *Sim) model(name string) *model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.models[name]
}

// Patch changes a model's settings while running, creating it if needed.
func (s *Sim) Patch(name string, p api.SimModelPatch) api.SimModel {
	s.mu.Lock()
	m := s.models[name]
	if m == nil {
		m = newModel(api.SimModel{})
		s.models[name] = m
	}
	s.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if p.RPM != nil {
		m.cfg.RPM = *p.RPM
	}
	if p.TPM != nil {
		m.cfg.TPM = *p.TPM
	}
	if p.Latency != nil {
		m.cfg.Latency = *p.Latency
	}
	if p.TransientRate != nil {
		m.cfg.TransientRate = *p.TransientRate
	}
	if p.PermanentRate != nil {
		m.cfg.PermanentRate = *p.PermanentRate
	}
	m.series.SetLimits(time.Now(), m.cfg.RPM, m.cfg.TPM)
	return m.cfg
}

// Do handles one inference call. It implements provider.Provider, so the
// service can also call the simulator in-process without HTTP.
func (s *Sim) Do(ctx context.Context, req *api.InferRequest) provider.Result {
	status, out := s.infer(ctx, req)
	return provider.Classify(status, out)
}

func (s *Sim) infer(ctx context.Context, req *api.InferRequest) (int, api.InferResponse) {
	out := api.InferResponse{RequestID: req.RequestID}
	m := s.model(req.Model)
	if m == nil {
		out.Error = "unknown model " + req.Model
		return http.StatusNotFound, out
	}

	// Limit check: the call counts on arrival, before any work is done.
	now := time.Now()
	m.mu.Lock()
	if m.reqWin.Sum(now)+1 > m.cfg.RPM || m.tokWin.Sum(now)+req.EstimatedTokens > m.cfg.TPM {
		m.limitRejected++
		m.series.Update(now, func(p *metrics.Point) { p.Rejected++ })
		m.mu.Unlock()
		out.Error = "rate limit exceeded"
		return http.StatusTooManyRequests, out
	}
	m.reqWin.Add(now, 1)
	m.tokWin.Add(now, req.EstimatedTokens)
	m.accepted++
	m.tokens += req.EstimatedTokens
	m.series.Update(now, func(p *metrics.Point) { p.Requests++; p.Tokens += req.EstimatedTokens })
	cfg := m.cfg
	m.mu.Unlock()

	if d := sampleLatency(cfg.Latency); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			out.Error = "cancelled"
			return http.StatusGatewayTimeout, out
		}
	}

	// A permanently failing request fails on every attempt, so the decision
	// comes from its id. Transient failures are random per attempt.
	status := http.StatusOK
	switch {
	case cfg.PermanentRate > 0 && idFraction(req.RequestID) < cfg.PermanentRate:
		status, out.Error = http.StatusBadRequest, "simulated permanent failure"
	case cfg.TransientRate > 0 && rand.Float64() < cfg.TransientRate:
		status, out.Error = http.StatusServiceUnavailable, "simulated transient failure"
	default:
		out.TokensUsed = req.EstimatedTokens
		out.Output, _ = json.Marshal(map[string]any{"text": "simulated completion for " + req.RequestID})
	}

	m.mu.Lock()
	switch status {
	case http.StatusOK:
		m.succeeded++
	case http.StatusBadRequest:
		m.permanent++
	default:
		m.transient++
	}
	m.mu.Unlock()
	return status, out
}

func sampleLatency(c api.LatencyConfig) time.Duration {
	var ms float64
	switch c.Kind {
	case "uniform":
		ms = c.MinMs + rand.Float64()*(c.MaxMs-c.MinMs)
	case "lognormal":
		sigma := 0.0
		if c.P99Ms > c.P50Ms && c.P50Ms > 0 {
			sigma = math.Log(c.P99Ms/c.P50Ms) / 2.326 // 2.326 = z-score of the 99th percentile
		}
		ms = c.P50Ms * math.Exp(sigma*rand.NormFloat64())
	default:
		ms = c.Ms
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// idFraction maps a request id to a stable number in [0, 1).
func idFraction(id string) float64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return float64(h.Sum64()>>11) / float64(1<<53)
}

func (s *Sim) Stats(withSeries bool) api.SimStats {
	now := time.Now()
	out := api.SimStats{StartedAt: s.started, Now: now, Models: map[string]*api.SimModelStats{}}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, m := range s.models {
		m.mu.Lock()
		st := &api.SimModelStats{
			Config: m.cfg, Accepted: m.accepted, Tokens: m.tokens, LimitRejected: m.limitRejected,
			Succeeded: m.succeeded, Transient: m.transient, Permanent: m.permanent,
		}
		m.mu.Unlock()
		if withSeries {
			st.Series = m.series.Snapshot(now)
		}
		out.Models[name] = st
	}
	return out
}

// Handler exposes the provider endpoint and the admin endpoints.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/infer", func(w http.ResponseWriter, r *http.Request) {
		var req api.InferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, api.InferResponse{Error: "invalid JSON: " + err.Error()})
			return
		}
		status, out := s.infer(r.Context(), &req)
		writeJSON(w, status, out)
	})
	mux.HandleFunc("GET /admin/models", func(w http.ResponseWriter, r *http.Request) {
		cfgs := map[string]api.SimModel{}
		for name, st := range s.Stats(false).Models {
			cfgs[name] = st.Config
		}
		writeJSON(w, http.StatusOK, cfgs)
	})
	mux.HandleFunc("PUT /admin/models/{model}", func(w http.ResponseWriter, r *http.Request) {
		var p api.SimModelPatch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, s.Patch(r.PathValue("model"), p))
	})
	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Stats(r.URL.Query().Get("series") != "false"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
