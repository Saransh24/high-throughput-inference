package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"hti/internal/api"
)

const maxBodyBytes = 512 << 20

// Handler returns the service's HTTP API.
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", e.handleSubmit)
	mux.HandleFunc("POST /v1/requests/lookup", e.handleLookup)
	mux.HandleFunc("GET /v1/requests/{id}", e.handleGetRequest)
	mux.HandleFunc("POST /v1/batches", e.handleSubmitBatch)
	mux.HandleFunc("GET /v1/batches/{id}", e.handleGetBatch)
	mux.HandleFunc("GET /v1/batches/{id}/results", e.handleBatchResults)
	mux.HandleFunc("GET /v1/models", e.handleModels)
	mux.HandleFunc("PUT /v1/models/{model}/limits", e.handleSetLimits)
	mux.HandleFunc("GET /v1/stats", e.handleStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError turns a submission error into the matching HTTP response.
func writeError(w http.ResponseWriter, err error, requestID string) {
	var rej *RejectedError
	var inv *invalidError
	switch {
	case errors.As(err, &rej):
		secs := int64(rej.RetryAfter.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		writeJSON(w, http.StatusTooManyRequests, api.ErrorBody{Error: err.Error(), RequestID: requestID, Status: api.StateRejected, RetryAfterMs: rej.RetryAfter.Milliseconds()})
	case errors.Is(err, ErrUnknownModel):
		writeJSON(w, http.StatusNotFound, api.ErrorBody{Error: err.Error(), RequestID: requestID})
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: err.Error(), RequestID: requestID})
	default:
		writeJSON(w, http.StatusInternalServerError, api.ErrorBody{Error: err.Error(), RequestID: requestID})
	}
}

func (e *Engine) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var spec api.RequestSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&spec); err != nil {
		writeError(w, &invalidError{"invalid JSON: " + err.Error()}, "")
		return
	}
	req, existing, err := e.Submit(spec)
	if err != nil {
		writeError(w, err, spec.RequestID)
		return
	}
	status := http.StatusAccepted
	if existing {
		status = http.StatusOK
	}
	writeJSON(w, status, req.View())
}

func (e *Engine) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	v, ok := e.requests.Load(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, api.ErrorBody{Error: "request not found"})
		return
	}
	writeJSON(w, http.StatusOK, v.(*Request).View())
}

func (e *Engine) handleLookup(w http.ResponseWriter, r *http.Request) {
	var in api.LookupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&in); err != nil {
		writeError(w, &invalidError{"invalid JSON: " + err.Error()}, "")
		return
	}
	out := api.LookupResponse{Statuses: make([]string, len(in.IDs))}
	for i, id := range in.IDs {
		out.Statuses[i] = "unknown"
		if v, ok := e.requests.Load(id); ok {
			req := v.(*Request)
			req.mu.Lock()
			out.Statuses[i] = req.state
			req.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleSubmitBatch(w http.ResponseWriter, r *http.Request) {
	// A repeated submit with the same Idempotency-Key returns the first batch.
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		if prev, ok := e.idem.Load(key); ok {
			writeJSON(w, http.StatusOK, ack(prev.(*Batch)))
			return
		}
	}
	var sub api.BatchSubmit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&sub); err != nil {
		writeError(w, &invalidError{"invalid JSON: " + err.Error()}, "")
		return
	}
	b, err := e.SubmitBatch(sub)
	if err != nil {
		writeError(w, err, "")
		return
	}
	if key != "" {
		e.idem.Store(key, b)
	}
	writeJSON(w, http.StatusAccepted, ack(b))
}

func ack(b *Batch) api.BatchAck {
	return api.BatchAck{
		BatchID: b.ID, Status: api.BatchInProgress, Total: len(b.items),
		StatusURL: "/v1/batches/" + b.ID, ResultsURL: "/v1/batches/" + b.ID + "/results",
	}
}

func (e *Engine) batch(w http.ResponseWriter, r *http.Request) *Batch {
	v, ok := e.batches.Load(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, api.ErrorBody{Error: "batch not found"})
		return nil
	}
	return v.(*Batch)
}

func (e *Engine) handleGetBatch(w http.ResponseWriter, r *http.Request) {
	if b := e.batch(w, r); b != nil {
		writeJSON(w, http.StatusOK, b.View())
	}
}

// handleBatchResults pages through a batch's requests in submission order.
// ?offset= and ?limit= select the page; ?status= filters within it.
func (e *Engine) handleBatchResults(w http.ResponseWriter, r *http.Request) {
	b := e.batch(w, r)
	if b == nil {
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 10_000 {
		limit = 1000
	}
	offset = min(max(offset, 0), len(b.items))
	end := min(offset+limit, len(b.items))
	out := api.BatchResults{BatchID: b.ID, Total: len(b.items), Offset: offset, Results: make([]api.RequestView, 0, end-offset)}
	want := q.Get("status")
	for _, req := range b.items[offset:end] {
		if v := req.View(); want == "" || v.Status == want {
			out.Results = append(out.Results, v)
		}
	}
	if end < len(b.items) {
		out.NextOffset = &end
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleModels(w http.ResponseWriter, r *http.Request) {
	st := e.Stats(false)
	type row struct {
		Model string `json:"model"`
		api.Limits
		Counts api.Counts `json:"counts"`
	}
	rows := make([]row, 0, len(st.Models))
	for name, m := range st.Models {
		rows = append(rows, row{name, m.Limits, m.Counts})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	writeJSON(w, http.StatusOK, rows)
}

func (e *Engine) handleSetLimits(w http.ResponseWriter, r *http.Request) {
	var p api.LimitsPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, &invalidError{"invalid JSON: " + err.Error()}, "")
		return
	}
	limits, err := e.SetLimits(r.PathValue("model"), p)
	if err != nil {
		writeError(w, &invalidError{err.Error()}, "")
		return
	}
	writeJSON(w, http.StatusOK, limits)
}

func (e *Engine) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, e.Stats(r.URL.Query().Get("series") != "false"))
}
