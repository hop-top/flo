// Package ratelimit counts the calls that draw on a provider quota (node.md
// §13.1). Buckets are in memory and shared by name: nodes that declare the
// same bucket share its count. A bucket is per tenant.
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"hop.top/flo/internal/core"
)

// Limit is the quota a node declares: Max calls per Per.
type Limit struct {
	Bucket string
	Max    int
	Per    time.Duration
}

// Limiter holds the buckets. It is safe for concurrent use.
type Limiter struct {
	clock core.Clock

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is a sliding window: the instants of the calls still inside the
// longest window any user declared.
type bucket struct {
	calls  []time.Time
	maxPer time.Duration
}

// New returns a Limiter reading time from clock (core.SystemClock when nil).
func New(clock core.Clock) *Limiter {
	if clock == nil {
		clock = core.SystemClock{}
	}
	return &Limiter{clock: clock, buckets: map[string]*bucket{}}
}

// Take counts one call against the bucket. When the bucket is exhausted it
// waits for a slot, for at most wait, then fails with provider_rate_limited
// (transient) naming how long until a slot frees. Calls that later fail with
// a client error still count: providers may count them. A cancelled ctx
// returns its error.
func (l *Limiter) Take(ctx context.Context, tenant core.TenantID, lim Limit, wait time.Duration) error {
	if lim.Bucket == "" || lim.Max <= 0 || lim.Per <= 0 {
		return nil
	}
	key := string(tenant) + "\x00" + lim.Bucket
	deadline := l.clock.Now().Add(wait)
	for {
		free, delay := l.try(key, lim)
		if free {
			return nil
		}
		if l.clock.Now().Add(delay).After(deadline) {
			return core.Errorf(core.CodeProviderRateLimited,
				"rate limit bucket %q is exhausted (%d per %s); a slot frees in %s", lim.Bucket, lim.Max, lim.Per, delay.Round(time.Millisecond)).
				WithDetail("bucket", lim.Bucket).
				WithDetail("retry_after_ms", delay.Milliseconds())
		}
		if err := l.clock.Sleep(ctx, delay); err != nil {
			return fmt.Errorf("rate limit wait: %w", err)
		}
	}
}

// try takes a slot when one is free; otherwise it returns how long until the
// oldest call in the window ages out.
func (l *Limiter) try(key string, lim Limit) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{}
		l.buckets[key] = b
	}
	if lim.Per > b.maxPer {
		b.maxPer = lim.Per
	}
	// Drop what no declared window can still count.
	keep := b.calls[:0]
	for _, t := range b.calls {
		if now.Sub(t) < b.maxPer {
			keep = append(keep, t)
		}
	}
	b.calls = keep
	var inWindow []time.Time
	for _, t := range b.calls {
		if now.Sub(t) < lim.Per {
			inWindow = append(inWindow, t)
		}
	}
	if len(inWindow) < lim.Max {
		b.calls = append(b.calls, now)
		return true, 0
	}
	// The window frees a slot when the call that is Max-from-newest ages out.
	pivot := inWindow[len(inWindow)-lim.Max]
	delay := lim.Per - now.Sub(pivot)
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	return false, delay
}
