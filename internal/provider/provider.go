// Package provider is the boundary between the service and whatever answers
// inference calls. The service only sees the Provider interface, so the HTTP
// client below, the in-process simulator, or a real provider adapter are
// interchangeable.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"hti/internal/api"
)

type Outcome int

const (
	Success   Outcome = iota
	Transient         // worth retrying: rate limited, server error, timeout
	Permanent         // retrying cannot help: the request itself is bad
)

type Result struct {
	Outcome     Outcome
	RateLimited bool // the provider refused the call for being over its limit
	Output      json.RawMessage
	Error       string
}

type Provider interface {
	Do(ctx context.Context, req *api.InferRequest) Result
}

// HTTP calls a provider over HTTP, one call per request.
type HTTP struct {
	url    string
	client *http.Client
}

func NewHTTP(baseURL string) *HTTP {
	tr := &http.Transport{
		MaxIdleConns:        20000,
		MaxIdleConnsPerHost: 20000,
		IdleConnTimeout:     90 * time.Second,
	}
	return &HTTP{url: baseURL + "/v1/infer", client: &http.Client{Transport: tr}}
}

func (h *HTTP) Do(ctx context.Context, req *api.InferRequest) Result {
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return Result{Outcome: Permanent, Error: err.Error()}
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(hr)
	if err != nil {
		return Result{Outcome: Transient, Error: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out api.InferResponse
	_ = json.Unmarshal(raw, &out)
	return Classify(resp.StatusCode, out)
}

// Classify maps an HTTP status to an outcome. 429 and 5xx are transient;
// any other 4xx is permanent.
func Classify(status int, out api.InferResponse) Result {
	switch {
	case status >= 200 && status < 300:
		return Result{Outcome: Success, Output: out.Output}
	case status == http.StatusTooManyRequests:
		return Result{Outcome: Transient, RateLimited: true, Error: orDefault(out.Error, "rate limited")}
	case status >= 500:
		return Result{Outcome: Transient, Error: orDefault(out.Error, http.StatusText(status))}
	default:
		return Result{Outcome: Permanent, Error: orDefault(out.Error, http.StatusText(status))}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
