package coretest

import (
	"context"
	"sort"
	"sync"
	"time"

	"hop.top/flo/internal/core"
)

// Epoch is the instant a Clock starts at unless told otherwise.
var Epoch = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// Clock is a manual clock: time moves only through Advance and Set, and
// Sleep blocks until the clock has been advanced past its deadline.
type Clock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	at time.Time
	ch chan struct{}
}

// NewClock returns a clock at start (Epoch when zero).
func NewClock(start time.Time) *Clock {
	if start.IsZero() {
		start = Epoch
	}
	c := &Clock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep blocks until the clock reaches Now()+d or ctx is done.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	w := &waiter{at: c.now.Add(d), ch: make(chan struct{})}
	c.waiters = append(c.waiters, w)
	c.cond.Broadcast()
	c.mu.Unlock()
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		c.drop(w)
		c.cond.Broadcast()
		c.mu.Unlock()
		return ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	}
}

func (c *Clock) drop(w *waiter) {
	for i, x := range c.waiters {
		if x == w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return
		}
	}
}

// Advance moves the clock forward by d and wakes the sleepers whose deadline
// has passed, earliest first.
func (c *Clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// Set moves the clock to t (never backwards) and wakes due sleepers.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
	sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].at.Before(c.waiters[j].at) })
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			close(w.ch)
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
	c.cond.Broadcast()
}

// Sleepers returns how many goroutines are blocked in Sleep.
func (c *Clock) Sleepers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// BlockUntil blocks until n goroutines are sleeping on the clock, so a test
// advances time only once the code under test is waiting for it.
func (c *Clock) BlockUntil(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.waiters) < n {
		c.cond.Wait()
	}
}

// NextDeadline returns the earliest sleeper's deadline; ok is false when
// nobody sleeps.
func (c *Clock) NextDeadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waiters) == 0 {
		return time.Time{}, false
	}
	min := c.waiters[0].at
	for _, w := range c.waiters[1:] {
		if w.at.Before(min) {
			min = w.at
		}
	}
	return min, true
}

var _ core.Clock = (*Clock)(nil)
