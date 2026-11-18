// Package runlog is the run's ordered log of CloudEvents (flo.run.*): events
// go on kit's bus through a TeeBus whose flo sink appends each one, as a
// CloudEvents JSON line, to the run's file (runs/<run>.jsonl). kit's own
// JSONLSink is not used: it keeps only topic, source, time and payload, and
// the run log must keep id, subject and extensions (tenant, seq) so a line
// validates as a CloudEvent and replays into the same event.
package runlog

import (
	"context"
	"errors"
	"regexp"
	"sync"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
)

// EngineFile is the log of events that belong to no run (trigger events).
const EngineFile = "_engine.jsonl"

// Options configure New.
type Options struct {
	// RunsDir maps a tenant to the directory of its run logs
	// (tenants.Registry.RunsDir).
	RunsDir func(core.TenantID) string
	// Bus is the bus events are published on; a new in-memory bus when nil.
	// The log wraps it in a TeeBus with its file sink.
	Bus bus.Bus
}

// Log implements core.RunLog.
type Log struct {
	sink *fileSink
	bus  *bus.TeeBus

	mu      sync.Mutex // serializes Emit: file order is emit order
	sinkErr error
	notify  *notifier
}

var _ core.RunLog = (*Log)(nil)

// New returns a Log writing under opts.RunsDir.
func New(opts Options) *Log {
	l := &Log{notify: newNotifier()}
	l.sink = &fileSink{dir: opts.RunsDir, counts: map[string]*count{}}
	inner := opts.Bus
	if inner == nil {
		inner = bus.New()
	}
	l.bus = bus.NewTeeBus(inner, []bus.Sink{l.sink}, func(err error) { l.sinkErr = errors.Join(l.sinkErr, err) })
	return l
}

// Bus returns the bus events are published on, for subscribers.
func (l *Log) Bus() bus.Bus { return l.bus }

// Close closes the bus and the sink.
func (l *Log) Close(ctx context.Context) error { return l.bus.Close(ctx) } //nolint:wrapcheck // kit bus error

var runRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func checkRun(run core.RunID) error {
	if run != "" && !runRe.MatchString(string(run)) {
		return core.Errorf(core.CodeUsage, "run id %q is not usable in a file name", string(run))
	}
	return nil
}

func terminalTopic(t bus.Topic) bool {
	return t == core.TopicRunSucceeded || t == core.TopicRunFailed || t == core.TopicRunCancelled
}

// Emit implements core.RunLog. The event is validated as a CloudEvent,
// stamped with its sequence number, written to the run's file, then published
// on the bus. A failed write is returned; the event is still published.
func (l *Log) Emit(ctx context.Context, tenant core.TenantID, run core.RunID, ev bus.Event) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	if err := checkRun(run); err != nil {
		return err
	}
	if err := bus.ValidateTopic(ev.Topic); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	ev.Subject = string(run)
	exts := make(map[string]any, len(ev.Extensions)+2)
	for k, v := range ev.Extensions {
		exts[k] = v
	}
	exts[core.ExtTenant] = string(tenant)
	ev.Extensions = exts

	l.mu.Lock()
	defer l.mu.Unlock()
	seq, err := l.sink.next(tenant, run)
	if err != nil {
		return err
	}
	ev.Extensions[core.ExtSeq] = seq
	if err := validate(ev); err != nil {
		return err
	}
	l.sinkErr = nil
	pubErr := l.bus.Publish(ctx, ev)
	werr := l.sinkErr
	if werr == nil {
		if terminalTopic(ev.Topic) {
			l.sink.forget(tenant, run)
		}
		l.notify.wake()
	}
	return errors.Join(pubErr, werr)
}

func validate(ev bus.Event) error {
	ce, err := bus.ToCloudEvent(ev)
	if err == nil {
		err = ce.Validate()
	}
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

// notifier wakes following readers in this process the moment an event is
// written; readers also poll, to see writes of another process.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) wake() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}
