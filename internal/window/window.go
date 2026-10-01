// Package window counts events over a trailing span of time.
//
// Time is cut into fixed slots (100 ms by default). The counter keeps one
// number per slot and a running total, so reading "how many in the last
// 60 seconds" is O(1). It is not safe for concurrent use; the owner locks.
package window

import "time"

type Sliding struct {
	slot  time.Duration
	slots []int64
	cur   int64 // absolute index of the newest slot
	sum   int64
}

// New returns a counter covering at least span. It keeps one extra slot, so
// the covered period is between span and span+slot: it never under-counts.
func New(span, slot time.Duration) *Sliding {
	n := int(span/slot) + 1
	return &Sliding{slot: slot, slots: make([]int64, n)}
}

// advance drops slots that have fallen out of the window.
func (s *Sliding) advance(now time.Time) {
	idx := now.UnixNano() / int64(s.slot)
	if idx <= s.cur {
		return
	}
	n := int64(len(s.slots))
	if idx-s.cur >= n {
		clear(s.slots)
		s.sum = 0
	} else {
		for i := s.cur + 1; i <= idx; i++ {
			p := i % n
			s.sum -= s.slots[p]
			s.slots[p] = 0
		}
	}
	s.cur = idx
}

// Sum returns the total inside the window ending at now.
func (s *Sliding) Sum(now time.Time) int64 {
	s.advance(now)
	return s.sum
}

// Add records v at time now.
func (s *Sliding) Add(now time.Time, v int64) {
	s.advance(now)
	s.slots[s.cur%int64(len(s.slots))] += v
	s.sum += v
}
