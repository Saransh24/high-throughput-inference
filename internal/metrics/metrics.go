// Package metrics holds the two measurement tools shared by the service, the
// simulator and the report: a per-second time series and a latency histogram.
package metrics

import (
	"math"
	"sync"
	"time"
)

// Point is one second of activity for one model.
type Point struct {
	T         int64 `json:"t"` // unix second
	RPMLimit  int64 `json:"rpm_limit"`
	TPMLimit  int64 `json:"tpm_limit"`
	Requests  int64 `json:"requests"` // sent by the service, or accepted by the simulator
	Tokens    int64 `json:"tokens"`
	Submitted int64 `json:"submitted,omitempty"`
	Rejected  int64 `json:"rejected,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Expired   int64 `json:"expired,omitempty"`
	Queued    int64 `json:"queued,omitempty"` // queue depth sampled during this second
}

// Series is a gap-free list of Points, one per second since the first event.
// Events are filed under the second in which they happened, so sums over any
// run of N points are exact counts for those N seconds.
type Series struct {
	mu       sync.Mutex
	start    int64
	pts      []Point
	rpm, tpm int64
}

func NewSeries(rpm, tpm int64) *Series { return &Series{rpm: rpm, tpm: tpm} }

func (s *Series) at(now time.Time) *Point {
	sec := now.Unix()
	if s.pts == nil {
		s.start = sec
	}
	sec = max(sec, s.start)
	for int64(len(s.pts)) <= sec-s.start {
		s.pts = append(s.pts, Point{T: s.start + int64(len(s.pts)), RPMLimit: s.rpm, TPMLimit: s.tpm})
	}
	return &s.pts[sec-s.start]
}

func (s *Series) Update(now time.Time, f func(*Point)) {
	s.mu.Lock()
	f(s.at(now))
	s.mu.Unlock()
}

// SetLimits records a limit change. The second in which the change happens
// keeps the higher of the two limits, because sends made earlier in that
// second were legal under the old one.
func (s *Series) SetLimits(now time.Time, rpm, tpm int64) {
	s.mu.Lock()
	p := s.at(now)
	s.rpm, s.tpm = rpm, tpm
	p.RPMLimit = max(p.RPMLimit, rpm)
	p.TPMLimit = max(p.TPMLimit, tpm)
	s.mu.Unlock()
}

func (s *Series) Snapshot(now time.Time) []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at(now)
	return append([]Point(nil), s.pts...)
}

// Violation is a second in which something was sent although the 60 seconds
// ending with it were already over the limit in force.
type Violation struct {
	T      int64  `json:"t"`
	Kind   string `json:"kind"` // "rpm" or "tpm"
	Rolled int64  `json:"rolling_60s"`
	Limit  int64  `json:"limit"`
}

// Violations checks every run of 60 consecutive seconds against the limit in
// force during its last second. Seconds with no sends are skipped: after a
// limit is lowered the trailing window is over the new limit through no new
// action, and the rule is that nothing more may be sent until it drains.
func Violations(pts []Point) []Violation {
	var out []Violation
	var req, tok int64
	for i, p := range pts {
		req += p.Requests
		tok += p.Tokens
		if i >= 60 {
			req -= pts[i-60].Requests
			tok -= pts[i-60].Tokens
		}
		if p.Requests == 0 {
			continue
		}
		if req > p.RPMLimit {
			out = append(out, Violation{p.T, "rpm", req, p.RPMLimit})
		}
		if tok > p.TPMLimit {
			out = append(out, Violation{p.T, "tpm", tok, p.TPMLimit})
		}
	}
	return out
}

// Rolling returns, for each point, the sum of f over the 60 seconds ending at it.
func Rolling(pts []Point, f func(Point) int64) []int64 {
	out := make([]int64, len(pts))
	var sum int64
	for i, p := range pts {
		sum += f(p)
		if i >= 60 {
			sum -= f(pts[i-60])
		}
		out[i] = sum
	}
	return out
}

// Slice returns the points with from <= T < to.
func Slice(pts []Point, from, to int64) []Point {
	var out []Point
	for _, p := range pts {
		if p.T >= from && p.T < to {
			out = append(out, p)
		}
	}
	return out
}

func Sum(pts []Point, f func(Point) int64) int64 {
	var s int64
	for _, p := range pts {
		s += f(p)
	}
	return s
}

func Sent(p Point) int64      { return p.Requests }
func Tokens(p Point) int64    { return p.Tokens }
func Completed(p Point) int64 { return p.Succeeded + p.Failed }

// Histogram records durations in buckets that grow by 4% each, from one
// microsecond to several hours. Percentiles are therefore accurate to 4%.
type Histogram struct {
	mu     sync.Mutex
	counts [640]uint64
	n      uint64
	sum    float64 // microseconds
	max    float64
}

const growth = 1.04

var logGrowth = math.Log(growth)

func (h *Histogram) Observe(d time.Duration) {
	us := float64(d) / 1e3
	i := 0
	if us > 1 {
		i = min(int(math.Log(us)/logGrowth)+1, len(h.counts)-1)
	}
	h.mu.Lock()
	h.counts[i]++
	h.n++
	h.sum += us
	h.max = max(h.max, us)
	h.mu.Unlock()
}

type Summary struct {
	Count  uint64  `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
}

func (h *Histogram) Summary() Summary {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == 0 {
		return Summary{}
	}
	q := func(p float64) float64 {
		target := uint64(math.Ceil(p * float64(h.n)))
		var cum uint64
		for i, c := range h.counts {
			cum += c
			if cum >= target {
				return round(min(math.Pow(growth, float64(i)), h.max) / 1e3)
			}
		}
		return round(h.max / 1e3)
	}
	return Summary{Count: h.n, MeanMs: round(h.sum / float64(h.n) / 1e3), P50Ms: q(0.50), P95Ms: q(0.95), P99Ms: q(0.99), MaxMs: round(h.max / 1e3)}
}

func round(ms float64) float64 { return math.Round(ms*1000) / 1000 }
