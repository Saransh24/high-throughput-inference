// Package api defines the JSON shapes exchanged between the service, the
// simulator, the load generator and clients.
package api

import (
	"encoding/json"
	"time"

	"hti/internal/metrics"
)

// Request states. The last four are final.
const (
	StateQueued    = "queued"
	StateInFlight  = "in_flight"
	StateRetryWait = "retry_wait"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateExpired   = "expired"
	StateRejected  = "rejected"
)

// Reasons attached to failed and expired requests.
const (
	ReasonPermanent = "permanent"
	ReasonExhausted = "retries_exhausted"
	ReasonExpired   = "expired"
)

// RequestSpec is one inference request as submitted by a client.
type RequestSpec struct {
	RequestID       string          `json:"request_id,omitempty"`
	Model           string          `json:"model"`
	EstimatedTokens int64           `json:"estimated_tokens"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	DeadlineMs      int64           `json:"deadline_ms,omitempty"`
}

type RequestView struct {
	RequestID       string          `json:"request_id"`
	BatchID         string          `json:"batch_id,omitempty"`
	Model           string          `json:"model"`
	Status          string          `json:"status"`
	Reason          string          `json:"reason,omitempty"`
	Attempts        int             `json:"attempts"`
	EstimatedTokens int64           `json:"estimated_tokens"`
	Output          json.RawMessage `json:"output,omitempty"`
	Error           string          `json:"error,omitempty"`
	SubmittedAt     time.Time       `json:"submitted_at"`
	FirstSentAt     *time.Time      `json:"first_sent_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}

type ErrorBody struct {
	Error        string `json:"error"`
	RequestID    string `json:"request_id,omitempty"`
	Status       string `json:"status,omitempty"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
}

type LookupRequest struct {
	IDs []string `json:"ids"`
}

// LookupResponse lists one status per requested id, in the same order.
// Unknown ids get "unknown".
type LookupResponse struct {
	Statuses []string `json:"statuses"`
}

// Batches.

const (
	BatchInProgress   = "in_progress"
	BatchCompleted    = "completed"
	BatchWithFailures = "completed_with_failures"
	BatchFailed       = "failed"
)

type BatchSubmit struct {
	CallbackURL string        `json:"callback_url,omitempty"`
	DeadlineMs  int64         `json:"deadline_ms,omitempty"`
	Requests    []RequestSpec `json:"requests"`
}

type BatchAck struct {
	BatchID    string `json:"batch_id"`
	Status     string `json:"status"`
	Total      int    `json:"total"`
	StatusURL  string `json:"status_url"`
	ResultsURL string `json:"results_url"`
}

type BatchCounts struct {
	Total          int            `json:"total"`
	Succeeded      int            `json:"succeeded"`
	Failed         int            `json:"failed"`
	Pending        int            `json:"pending"`
	FailedByReason map[string]int `json:"failed_by_reason"`
}

// BatchSummary is both the callback body and the core of the status response,
// built by one function so the two cannot disagree.
type BatchSummary struct {
	BatchID     string      `json:"batch_id"`
	Status      string      `json:"status"`
	Counts      BatchCounts `json:"counts"`
	CreatedAt   time.Time   `json:"created_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	ResultsURL  string      `json:"results_url"`
}

type CallbackAttempt struct {
	Attempt int       `json:"attempt"`
	At      time.Time `json:"at"`
	Status  int       `json:"status,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// Callback delivery states.
const (
	CallbackNone      = "none"
	CallbackPending   = "pending"
	CallbackDelivered = "delivered"
	CallbackGaveUp    = "gave_up"
)

type CallbackView struct {
	URL         string            `json:"url,omitempty"`
	State       string            `json:"state"`
	Attempts    int               `json:"attempts"`
	DeliveredAt *time.Time        `json:"delivered_at,omitempty"`
	History     []CallbackAttempt `json:"history,omitempty"`
}

type BatchView struct {
	BatchSummary
	Callback CallbackView `json:"callback"`
}

type BatchResults struct {
	BatchID    string        `json:"batch_id"`
	Total      int           `json:"total"`
	Offset     int           `json:"offset"`
	NextOffset *int          `json:"next_offset,omitempty"`
	Results    []RequestView `json:"results"`
}

// Models and statistics.

type Limits struct {
	RPM int64 `json:"rpm"`
	TPM int64 `json:"tpm"`
}

// LimitsPatch changes one or both limits; an omitted field keeps its value.
type LimitsPatch struct {
	RPM *int64 `json:"rpm,omitempty"`
	TPM *int64 `json:"tpm,omitempty"`
}

type Counts struct {
	Submitted           int64 `json:"submitted"`
	Rejected            int64 `json:"rejected"`
	Queued              int64 `json:"queued"`
	InFlight            int64 `json:"in_flight"`
	RetryWait           int64 `json:"retry_wait"`
	Succeeded           int64 `json:"succeeded"`
	Failed              int64 `json:"failed"`
	Expired             int64 `json:"expired"`
	ProviderCalls       int64 `json:"provider_calls"`
	ProviderRateLimited int64 `json:"provider_rate_limited"`
}

func (c Counts) Waiting() int64   { return c.Queued + c.InFlight + c.RetryWait }
func (c Counts) Completed() int64 { return c.Succeeded + c.Failed }

func (c *Counts) Add(o Counts) {
	c.Submitted += o.Submitted
	c.Rejected += o.Rejected
	c.Queued += o.Queued
	c.InFlight += o.InFlight
	c.RetryWait += o.RetryWait
	c.Succeeded += o.Succeeded
	c.Failed += o.Failed
	c.Expired += o.Expired
	c.ProviderCalls += o.ProviderCalls
	c.ProviderRateLimited += o.ProviderRateLimited
}

type Latency struct {
	Queue    metrics.Summary `json:"queue"`    // submit to first send
	Provider metrics.Summary `json:"provider"` // one provider call
	Total    metrics.Summary `json:"total"`    // submit to final state
}

type ModelStats struct {
	Limits  Limits          `json:"limits"`
	Counts  Counts          `json:"counts"`
	Latency Latency         `json:"latency_ms"`
	Series  []metrics.Point `json:"series,omitempty"`
}

type LimitChange struct {
	At    time.Time `json:"at"`
	Model string    `json:"model"`
	Limits
}

type ServiceStats struct {
	StartedAt    time.Time              `json:"started_at"`
	Now          time.Time              `json:"now"`
	Totals       Counts                 `json:"totals"`
	Models       map[string]*ModelStats `json:"models"`
	LimitChanges []LimitChange          `json:"limit_changes,omitempty"`
}

// Simulator.

// LatencyConfig describes simulated response time. Kind is "fixed" (Ms),
// "uniform" (MinMs..MaxMs) or "lognormal" (median P50Ms, 99th percentile P99Ms).
type LatencyConfig struct {
	Kind  string  `json:"kind,omitempty"`
	Ms    float64 `json:"ms,omitempty"`
	MinMs float64 `json:"min_ms,omitempty"`
	MaxMs float64 `json:"max_ms,omitempty"`
	P50Ms float64 `json:"p50_ms,omitempty"`
	P99Ms float64 `json:"p99_ms,omitempty"`
}

type SimModel struct {
	RPM           int64         `json:"rpm"`
	TPM           int64         `json:"tpm"`
	Latency       LatencyConfig `json:"latency"`
	TransientRate float64       `json:"transient_rate"` // share of calls that fail but may be retried
	PermanentRate float64       `json:"permanent_rate"` // share of requests that always fail
}

// SimModelPatch changes any subset of a simulated model's settings.
type SimModelPatch struct {
	RPM           *int64         `json:"rpm,omitempty"`
	TPM           *int64         `json:"tpm,omitempty"`
	Latency       *LatencyConfig `json:"latency,omitempty"`
	TransientRate *float64       `json:"transient_rate,omitempty"`
	PermanentRate *float64       `json:"permanent_rate,omitempty"`
}

type SimModelStats struct {
	Config        SimModel        `json:"config"`
	Accepted      int64           `json:"accepted"`
	Tokens        int64           `json:"tokens"`
	LimitRejected int64           `json:"limit_rejected"` // calls refused with 429 for being over the limit
	Succeeded     int64           `json:"succeeded"`
	Transient     int64           `json:"transient_failures"`
	Permanent     int64           `json:"permanent_failures"`
	Series        []metrics.Point `json:"series,omitempty"`
}

type SimStats struct {
	StartedAt time.Time                 `json:"started_at"`
	Now       time.Time                 `json:"now"`
	Models    map[string]*SimModelStats `json:"models"`
}

// InferRequest and InferResponse are the wire format of one provider call.
type InferRequest struct {
	RequestID       string          `json:"request_id"`
	Model           string          `json:"model"`
	EstimatedTokens int64           `json:"estimated_tokens"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Attempt         int             `json:"attempt"`
}

type InferResponse struct {
	RequestID  string          `json:"request_id"`
	Output     json.RawMessage `json:"output,omitempty"`
	TokensUsed int64           `json:"tokens_used,omitempty"`
	Error      string          `json:"error,omitempty"`
}
