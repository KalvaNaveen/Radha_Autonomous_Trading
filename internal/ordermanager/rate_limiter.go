// Package ordermanager is the single choke point for every order-changing REST
// call: a priority queue (emergency > stop > entry), a strict multi-window rate
// limiter, bounded concurrent execution, retry/de-duplication policy, and the
// router that delivers broker order updates back to the owning agent.
package ordermanager

import (
	"context"
	"sync"
	"time"
)

// RateLimiter enforces hard caps over several sliding windows at once
// (per second / per minute / per day).
//
// Why not a classic token bucket? A bucket with rate R and burst B admits up to
// B + R·T requests in any window of length T. At R=8/s, B=8 that is 16 requests
// inside one second — over SEBI's 10-OPS registration threshold and over Kite's
// 10/s order limit (excess requests get HTTP 429, they are not queued). A
// sliding-window log admits at most `limit` requests in ANY window of the
// given size, which is the property the regulation actually cares about.
type RateLimiter struct {
	mu      sync.Mutex
	windows []*window
	now     func() time.Time
	total   uint64
}

type window struct {
	size  time.Duration
	limit int
	ring  []time.Time // timestamps of admitted requests, circular
	head  int         // index of oldest
	n     int
}

func (w *window) delay(now time.Time) time.Duration {
	if w.n < w.limit {
		return 0
	}
	oldest := w.ring[w.head]
	if d := oldest.Add(w.size).Sub(now); d > 0 {
		return d
	}
	return 0
}

func (w *window) admit(now time.Time) {
	if w.n == w.limit {
		// Evict the oldest (delay() guaranteed it is outside the window).
		w.head = (w.head + 1) % w.limit
		w.n--
	}
	idx := (w.head + w.n) % w.limit
	w.ring[idx] = now
	w.n++
}

func (w *window) count(now time.Time) int {
	c := 0
	for i := 0; i < w.n; i++ {
		if now.Sub(w.ring[(w.head+i)%w.limit]) < w.size {
			c++
		}
	}
	return c
}

// Limit describes one window.
type Limit struct {
	Window time.Duration
	Max    int
}

// NewRateLimiter builds a limiter; now may be nil (time.Now).
func NewRateLimiter(now func() time.Time, limits ...Limit) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	rl := &RateLimiter{now: now}
	for _, l := range limits {
		if l.Max < 1 {
			l.Max = 1
		}
		rl.windows = append(rl.windows, &window{size: l.Window, limit: l.Max, ring: make([]time.Time, l.Max)})
	}
	return rl
}

// TryAcquire admits a request now if every window allows it; otherwise it
// returns how long to wait before trying again.
func (r *RateLimiter) TryAcquire() (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var wait time.Duration
	for _, w := range r.windows {
		if d := w.delay(now); d > wait {
			wait = d
		}
	}
	if wait > 0 {
		return false, wait
	}
	for _, w := range r.windows {
		w.admit(now)
	}
	r.total++
	return true, 0
}

// Wait blocks until a request is admitted or ctx is done.
func (r *RateLimiter) Wait(ctx context.Context) error {
	for {
		ok, d := r.TryAcquire()
		if ok {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Remaining returns the free capacity of window i right now.
func (r *RateLimiter) Remaining(i int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.windows) {
		return 0
	}
	w := r.windows[i]
	return w.limit - w.count(r.now())
}

// Total returns the number of admitted requests since creation.
func (r *RateLimiter) Total() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}
