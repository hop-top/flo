package runlog

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
)

// pollEvery is how often a following reader looks at the file, to see writes
// by another process; writes in this process wake it at once.
const pollEvery = 50 * time.Millisecond

// Open implements core.RunLog: it reads the run's events from the first. With
// follow it keeps delivering as events are appended, until the run's terminal
// event (never, for the engine log of run "").
func (l *Log) Open(_ context.Context, tenant core.TenantID, run core.RunID, follow bool) (core.EventReader, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	if err := checkRun(run); err != nil {
		return nil, err
	}
	return &reader{l: l, path: l.sink.path(tenant, run), follow: follow, engine: run == ""}, nil
}

type reader struct {
	l      *Log
	path   string
	follow bool
	engine bool
	off    int64
	done   bool // the terminal event was delivered
}

// Next implements core.EventReader.
func (r *reader) Next(ctx context.Context) (bus.Event, error) {
	for {
		if r.done {
			return bus.Event{}, io.EOF
		}
		ev, ok, err := r.read()
		if err != nil {
			return bus.Event{}, err
		}
		if ok {
			if !r.engine && terminalTopic(ev.Topic) {
				r.done = r.follow
			}
			return ev, nil
		}
		if !r.follow {
			return bus.Event{}, io.EOF
		}
		wake := r.l.notify.wait()
		// re-check once after taking the channel: a write between the read
		// and the wait would otherwise be missed until the next poll
		if ev, ok, err := r.read(); err != nil || ok {
			if err != nil {
				return bus.Event{}, err
			}
			if !r.engine && terminalTopic(ev.Topic) {
				r.done = true
			}
			return ev, nil
		}
		select {
		case <-wake:
		case <-time.After(pollEvery):
		case <-ctx.Done():
			return bus.Event{}, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
		}
	}
}

// read returns the next complete, parsable line after the offset. A line a
// crash cut short, or one that does not parse, is skipped.
func (r *reader) read() (bus.Event, bool, error) {
	f, err := os.Open(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return bus.Event{}, false, nil
	}
	if err != nil {
		return bus.Event{}, false, core.Wrap(core.CodeInternal, err)
	}
	defer f.Close() //nolint:errcheck // read only
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return bus.Event{}, false, core.Wrap(core.CodeInternal, err)
	}
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil { // no newline: nothing complete yet
			if errors.Is(err, io.EOF) {
				return bus.Event{}, false, nil
			}
			return bus.Event{}, false, core.Wrap(core.CodeInternal, err)
		}
		r.off += int64(len(line))
		if len(line) <= 1 {
			continue
		}
		ev, err := bus.UnmarshalCloudEvent(line)
		if err != nil {
			continue
		}
		return ev, true, nil
	}
}

// Close implements core.EventReader.
func (r *reader) Close() error { return nil }
