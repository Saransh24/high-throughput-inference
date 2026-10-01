// Package limiter decides when a request may be sent to the provider.
//
// It has two parts, each applied to requests (RPM) and to tokens (TPM):
//
//   - a pacer: a token bucket with a small burst that refills slightly
//     below the limit, so traffic is smooth;
//   - a guard: a sliding window over the last ~60 seconds. A send is allowed
//     only if the window plus this send still fits under the current limit.
//
// The pacer gives high, even utilisation. The guard is the hard guarantee and
// is what makes limit decreases safe: after a decrease nothing is sent until
// the trailing window has drained below the new limit.
//
// A Limiter is not safe for concurrent use. Each model's lane owns one and
// calls it under the lane's lock. Time is passed in, so tests use a fake clock.
package limiter

import (
	"time"

	"hti/internal/window"
)

type Options struct {
	Headroom float64       // fraction of the limit left unused by the pacer
	Burst    time.Duration // how much unused pace may accumulate
	Span     time.Duration // guard window; slightly over 60 s to absorb network delay
	Slot     time.Duration // guard resolution
}

func DefaultOptions() Options {
	return Options{Headroom: 0.02, Burst: 200 * time.Millisecond, Span: 60500 * time.Millisecond, Slot: 100 * time.Millisecond}
}

type Limiter struct {
	opt      Options
	rpm, tpm int64

	reqRate, tokRate   float64 // pace, per second
	reqBurst, tokBurst float64
	reqAvail, tokAvail float64
	last               time.Time

	reqWin, tokWin *window.Sliding
}

func New(rpm, tpm int64, opt Options, now time.Time) *Limiter {
	l := &Limiter{
		opt:    opt,
		last:   now,
		reqWin: window.New(opt.Span, opt.Slot),
		tokWin: window.New(opt.Span, opt.Slot),
	}
	l.apply(rpm, tpm)
	l.reqAvail, l.tokAvail = l.reqBurst, l.tokBurst
	return l
}

func (l *Limiter) apply(rpm, tpm int64) {
	l.rpm, l.tpm = max(rpm, 0), max(tpm, 0)
	keep := 1 - l.opt.Headroom
	l.reqRate = float64(l.rpm) * keep / 60
	l.tokRate = float64(l.tpm) * keep / 60
	l.reqBurst = l.reqRate * l.opt.Burst.Seconds()
	if l.rpm > 0 {
		l.reqBurst = max(l.reqBurst, 1)
	}
	l.tokBurst = l.tokRate * l.opt.Burst.Seconds()
}

// SetLimits changes the limits. It takes effect on the next TryAcquire.
func (l *Limiter) SetLimits(rpm, tpm int64, now time.Time) {
	l.refill(now)
	l.apply(rpm, tpm)
	l.reqAvail = min(l.reqAvail, l.reqBurst)
	l.tokAvail = min(l.tokAvail, l.tokBurst)
}

func (l *Limiter) Limits() (rpm, tpm int64) { return l.rpm, l.tpm }

// Rates returns the pace in requests and tokens per second.
func (l *Limiter) Rates() (req, tok float64) { return l.reqRate, l.tokRate }

func (l *Limiter) refill(now time.Time) {
	dt := now.Sub(l.last).Seconds()
	if dt <= 0 {
		return
	}
	l.last = now
	l.reqAvail = min(l.reqAvail+dt*l.reqRate, l.reqBurst)
	l.tokAvail = min(l.tokAvail+dt*l.tokRate, l.tokBurst)
}

// TryAcquire reports whether one request of the given token size may be sent
// now, and records it if so.
func (l *Limiter) TryAcquire(now time.Time, tokens int64) bool {
	l.refill(now)
	// Pacer. The token bucket may go into debt for a request larger than the
	// burst; later requests then wait until it is positive again.
	if l.reqAvail < 1 || l.tokAvail < min(float64(tokens), l.tokBurst) || l.tokBurst <= 0 {
		return false
	}
	// Guard.
	if l.reqWin.Sum(now)+1 > l.rpm || l.tokWin.Sum(now)+tokens > l.tpm {
		return false
	}
	l.reqAvail--
	l.tokAvail -= float64(tokens)
	l.reqWin.Add(now, 1)
	l.tokWin.Add(now, tokens)
	return true
}
