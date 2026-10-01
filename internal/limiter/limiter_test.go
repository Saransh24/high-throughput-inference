package limiter

import (
	"math/rand/v2"
	"testing"
	"time"
)

// grant is one allowed send, kept in an exact log so the tests can check the
// limiter against the definition of the limit instead of its own counters.
type grant struct {
	at       time.Time
	tokens   int64
	rpm, tpm int64 // limits in force when it was granted
}

// checkWindows asserts that for every grant, the grants in the 60 seconds
// ending at it (inclusive) fit under the limits in force at that moment.
func checkWindows(t *testing.T, log []grant) {
	t.Helper()
	lo := 0
	var tokSum int64
	for hi, g := range log {
		tokSum += g.tokens
		for log[lo].at.Before(g.at.Add(-60 * time.Second)) {
			tokSum -= log[lo].tokens
			lo++
		}
		if n := int64(hi - lo + 1); n > g.rpm {
			t.Fatalf("grant %d at %v: %d requests in 60s, limit %d", hi, g.at.Sub(log[0].at), n, g.rpm)
		}
		if tokSum > g.tpm {
			t.Fatalf("grant %d at %v: %d tokens in 60s, limit %d", hi, g.at.Sub(log[0].at), tokSum, g.tpm)
		}
	}
}

var t0 = time.Unix(1_700_000_000, 0)

// drive runs the limiter with unlimited demand for d, one step per millisecond.
func drive(l *Limiter, start time.Time, d time.Duration, tokens func() int64, log []grant) []grant {
	for now := start; now.Before(start.Add(d)); now = now.Add(time.Millisecond) {
		for {
			tk := tokens()
			if !l.TryAcquire(now, tk) {
				break
			}
			rpm, tpm := l.Limits()
			log = append(log, grant{now, tk, rpm, tpm})
		}
	}
	return log
}

func countBetween(log []grant, from, to time.Time) (n, tokens int64) {
	for _, g := range log {
		if !g.at.Before(from) && g.at.Before(to) {
			n++
			tokens += g.tokens
		}
	}
	return
}

func TestSteadyStateUsesCapacityWithoutExceedingIt(t *testing.T) {
	l := New(50_000, 100_000_000, DefaultOptions(), t0)
	log := drive(l, t0, 5*time.Minute, func() int64 { return 1000 }, nil)
	checkWindows(t, log)
	for m := 1; m < 5; m++ {
		n, _ := countBetween(log, t0.Add(time.Duration(m)*time.Minute), t0.Add(time.Duration(m+1)*time.Minute))
		if n < 48_500 || n > 50_000 {
			t.Fatalf("minute %d: %d requests, want 48,500..50,000", m, n)
		}
	}
}

func TestTokenLimitBinds(t *testing.T) {
	// 2,500-token requests: 100M TPM allows 40,000 per minute, below the RPM limit.
	l := New(50_000, 100_000_000, DefaultOptions(), t0)
	log := drive(l, t0, 3*time.Minute, func() int64 { return 2500 }, nil)
	checkWindows(t, log)
	n, tok := countBetween(log, t0.Add(time.Minute), t0.Add(2*time.Minute))
	if tok < 97_000_000 || tok > 100_000_000 {
		t.Fatalf("tokens in a minute = %d, want 97M..100M (requests %d)", tok, n)
	}
}

func TestOversizedRequestStillPasses(t *testing.T) {
	// One request is larger than the token burst; it must not be stuck forever.
	l := New(600, 60_000, DefaultOptions(), t0)
	log := drive(l, t0, 3*time.Minute, func() int64 { return 5000 }, nil)
	checkWindows(t, log)
	if n, _ := countBetween(log, t0.Add(time.Minute), t0.Add(2*time.Minute)); n < 10 {
		t.Fatalf("only %d large requests in a minute, want about 11", n)
	}
}

func TestDecreasePausesThenResumes(t *testing.T) {
	l := New(30_000, 60_000_000, DefaultOptions(), t0)
	tok := func() int64 { return 1000 }
	log := drive(l, t0, 2*time.Minute, tok, nil)

	cut := t0.Add(2 * time.Minute)
	l.SetLimits(5_000, 60_000_000, cut)
	log = drive(l, cut, 3*time.Minute, tok, log)

	restore := cut.Add(3 * time.Minute)
	l.SetLimits(30_000, 60_000_000, restore)
	log = drive(l, restore, 2*time.Minute, tok, log)
	checkWindows(t, log)

	if n, _ := countBetween(log, cut, cut.Add(45*time.Second)); n != 0 {
		t.Fatalf("%d requests sent in the 45s after the cut, want 0 (strict pause)", n)
	}
	if n, _ := countBetween(log, cut.Add(time.Minute), cut.Add(2*time.Minute)); n < 4_700 || n > 5_000 {
		t.Fatalf("constrained minute sent %d, want 4,700..5,000", n)
	}
	if n, _ := countBetween(log, restore, restore.Add(10*time.Second)); n < 4_500 {
		t.Fatalf("10s after restore sent %d, want at least 4,500", n)
	}
	if n, _ := countBetween(log, restore.Add(time.Minute), restore.Add(2*time.Minute)); n < 29_000 {
		t.Fatalf("restored minute sent %d, want at least 29,000", n)
	}
}

func TestRandomLimitChangesAndBurstyDemand(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	rpms := []int64{5_000, 12_000, 30_000, 50_000, 800}
	tpms := []int64{8_000_000, 40_000_000, 100_000_000}
	l := New(30_000, 40_000_000, DefaultOptions(), t0)
	var log []grant
	now := t0
	nextChange := now.Add(30 * time.Second)
	idleUntil := now
	for now.Before(t0.Add(20 * time.Minute)) {
		if !now.Before(nextChange) {
			l.SetLimits(rpms[rng.IntN(len(rpms))], tpms[rng.IntN(len(tpms))], now)
			nextChange = now.Add(time.Duration(10+rng.IntN(80)) * time.Second)
		}
		if rng.IntN(20_000) == 0 { // occasional idle gap, then a burst of demand
			idleUntil = now.Add(time.Duration(rng.IntN(90)) * time.Second)
		}
		if !now.Before(idleUntil) {
			for {
				tk := int64(200 + rng.IntN(3000))
				if !l.TryAcquire(now, tk) {
					break
				}
				rpm, tpm := l.Limits()
				log = append(log, grant{now, tk, rpm, tpm})
			}
		}
		now = now.Add(time.Duration(1+rng.IntN(5)) * time.Millisecond)
	}
	if len(log) < 50_000 {
		t.Fatalf("only %d grants; the test did not exercise the limiter", len(log))
	}
	checkWindows(t, log)
}
