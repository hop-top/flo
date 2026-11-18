package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/ratelimit"
)

func TestSharedBucket(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	l := ratelimit.New(clock)
	lim := ratelimit.Limit{Bucket: "vcs", Max: 2, Per: time.Hour}
	ctx := context.Background()
	// two different nodes draw on one bucket
	for i := 0; i < 2; i++ {
		if err := l.Take(ctx, "t1", lim, 0); err != nil {
			t.Fatal(err)
		}
	}
	err := l.Take(ctx, "t1", lim, 0)
	if !core.IsCode(err, core.CodeProviderRateLimited) {
		t.Fatalf("err = %v", err)
	}
	if !err.(*core.Error).Retryable() { //nolint:errorlint // test
		t.Fatal("provider_rate_limited is transient")
	}
	// another tenant has its own bucket; another bucket name too
	if err := l.Take(ctx, "t2", lim, 0); err != nil {
		t.Fatal(err)
	}
	if err := l.Take(ctx, "t1", ratelimit.Limit{Bucket: "other", Max: 1, Per: time.Hour}, 0); err != nil {
		t.Fatal(err)
	}
	// the window slides
	clock.Advance(time.Hour)
	if err := l.Take(ctx, "t1", lim, 0); err != nil {
		t.Fatal(err)
	}
}

func TestWaitsForSlotWithinTimeout(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	l := ratelimit.New(clock)
	lim := ratelimit.Limit{Bucket: "b", Max: 1, Per: time.Minute}
	ctx := context.Background()
	if err := l.Take(ctx, "t", lim, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Take(ctx, "t", lim, 2*time.Minute) }()
	clock.BlockUntil(1)
	clock.Advance(time.Minute)
	if err := <-done; err != nil {
		t.Fatalf("waited call: %v", err)
	}
	// a wait shorter than the delay fails at once
	if err := l.Take(ctx, "t", lim, 10*time.Second); !core.IsCode(err, core.CodeProviderRateLimited) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelledWait(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	l := ratelimit.New(clock)
	lim := ratelimit.Limit{Bucket: "b", Max: 1, Per: time.Minute}
	_ = l.Take(context.Background(), "t", lim, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Take(ctx, "t", lim, time.Hour) }()
	clock.BlockUntil(1)
	cancel()
	if err := <-done; err == nil || core.IsCode(err, core.CodeProviderRateLimited) {
		t.Fatalf("err = %v", err)
	}
}
