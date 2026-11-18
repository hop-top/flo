package coretest

import (
	"context"
	"io"
	"sync"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
)

type logKey struct {
	tenant core.TenantID
	run    core.RunID
}

// RunLog is the fake core.RunLog. It keeps every run's events in memory,
// stamps the sequence number, and refuses an event that is not a valid
// CloudEvent, so a test that emits through it also checks the encoding.
type RunLog struct {
	mu     sync.Mutex
	logs   map[logKey][]bus.Event
	closed map[logKey]bool // terminal event seen
	notify chan struct{}
	// Bus, when set, also receives every emitted event.
	Bus bus.Bus
}

// NewRunLog returns an empty log.
func NewRunLog() *RunLog {
	return &RunLog{logs: map[logKey][]bus.Event{}, closed: map[logKey]bool{}, notify: make(chan struct{})}
}

func terminalTopic(t bus.Topic) bool {
	return t == core.TopicRunSucceeded || t == core.TopicRunFailed || t == core.TopicRunCancelled
}

// Emit implements core.RunLog.
func (l *RunLog) Emit(ctx context.Context, tenant core.TenantID, run core.RunID, ev bus.Event) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	if err := bus.ValidateTopic(ev.Topic); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	ev = clone(ev)
	l.mu.Lock()
	k := logKey{tenant, run}
	if ev.Extensions == nil {
		ev.Extensions = map[string]any{}
	}
	ev.Extensions[core.ExtSeq] = int64(len(l.logs[k]) + 1)
	ce, err := bus.ToCloudEvent(ev)
	if err == nil {
		err = ce.Validate()
	}
	if err != nil {
		l.mu.Unlock()
		return core.Wrap(core.CodeInternal, err)
	}
	l.logs[k] = append(l.logs[k], ev)
	if terminalTopic(ev.Topic) {
		l.closed[k] = true
	}
	close(l.notify)
	l.notify = make(chan struct{})
	b := l.Bus
	l.mu.Unlock()
	if b != nil {
		return b.Publish(ctx, ev) //nolint:wrapcheck // fake passes the bus error through
	}
	return nil
}

// Events returns a copy of a run's events.
func (l *RunLog) Events(tenant core.TenantID, run core.RunID) []bus.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return clone(l.logs[logKey{tenant, run}])
}

// Topics returns the topics of a run's events in order.
func (l *RunLog) Topics(tenant core.TenantID, run core.RunID) []bus.Topic {
	evs := l.Events(tenant, run)
	out := make([]bus.Topic, len(evs))
	for i, e := range evs {
		out[i] = e.Topic
	}
	return out
}

// Open implements core.RunLog.
func (l *RunLog) Open(_ context.Context, tenant core.TenantID, run core.RunID, follow bool) (core.EventReader, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	return &reader{l: l, k: logKey{tenant, run}, follow: follow}, nil
}

type reader struct {
	l      *RunLog
	k      logKey
	follow bool
	pos    int
}

func (r *reader) Next(ctx context.Context) (bus.Event, error) {
	for {
		r.l.mu.Lock()
		evs, done, ch := r.l.logs[r.k], r.l.closed[r.k], r.l.notify
		if r.pos < len(evs) {
			ev := clone(evs[r.pos])
			r.pos++
			r.l.mu.Unlock()
			return ev, nil
		}
		r.l.mu.Unlock()
		if !r.follow || done {
			return bus.Event{}, io.EOF
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return bus.Event{}, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
		}
	}
}

func (r *reader) Close() error { return nil }
