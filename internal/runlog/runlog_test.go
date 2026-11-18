package runlog_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/runlog"
)

const T = core.DefaultTenant

var bg = context.Background()

type env struct {
	dir   string
	clock *coretest.Clock
	ids   *coretest.IDs
}

func (e env) log() *runlog.Log {
	return runlog.New(runlog.Options{RunsDir: func(t core.TenantID) string { return filepath.Join(e.dir, string(t), "runs") }})
}

func newEnv(t *testing.T) env {
	return env{dir: t.TempDir(), clock: coretest.NewClock(time.Time{}), ids: &coretest.IDs{}}
}

func (e env) ev(topic bus.Topic, run core.RunID, payload any) bus.Event {
	return core.NewEvent(e.clock, e.ids, topic, T, run, payload)
}

func (e env) emit(t *testing.T, l core.RunLog, run core.RunID, topic bus.Topic) {
	t.Helper()
	if err := l.Emit(bg, T, run, e.ev(topic, run, core.RunEvent{Tenant: T, Run: run, Flow: "w", Status: core.RunRunning})); err != nil {
		t.Fatal(err)
	}
}

func drain(t *testing.T, r core.EventReader) []bus.Event {
	t.Helper()
	var out []bus.Event
	for {
		ev, err := r.Next(bg)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func seqOf(ev bus.Event) int64 {
	switch v := ev.Extensions[core.ExtSeq].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return -1
}

func TestEmitOrderSeqAndCloudEventLines(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	topics := []bus.Topic{core.TopicRunCreated, core.TopicRunStarted, core.TopicStepStarted, core.TopicRunSucceeded}
	for _, tp := range topics {
		e.emit(t, l, "run_1", tp)
	}
	r, _ := l.Open(bg, T, "run_1", false)
	evs := drain(t, r)
	if len(evs) != 4 {
		t.Fatalf("%d events", len(evs))
	}
	for i, ev := range evs {
		if ev.Topic != topics[i] || seqOf(ev) != int64(i+1) || ev.Subject != "run_1" || ev.ID == "" || ev.Extensions[core.ExtTenant] != "default" {
			t.Errorf("event %d = %+v", i, ev)
		}
		if ev.Source != core.EventSource(T) {
			t.Errorf("source %q", ev.Source)
		}
	}
	// every line validates as a CloudEvent and keeps id, subject, extensions
	f, err := os.Open(filepath.Join(e.dir, "default", "runs", "run_1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		ev, err := bus.UnmarshalCloudEvent(sc.Bytes())
		if err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		ce, err := bus.ToCloudEvent(ev)
		if err != nil || ce.Validate() != nil {
			t.Fatalf("line %d does not validate: %v", n, err)
		}
		if ce.Subject != "run_1" || ce.ID == "" {
			t.Errorf("line %d lost id or subject: %+v", n, ce)
		}
	}
	if n != 4 {
		t.Errorf("%d lines", n)
	}
	// payload round trips as data
	if p, ok := evs[0].Payload.(map[string]any); !ok || p["run"] != "run_1" {
		t.Errorf("payload = %#v", evs[0].Payload)
	}
}

func TestSeqContinuesAcrossReopenAndSecondWriter(t *testing.T) {
	e := newEnv(t)
	l1, l2 := e.log(), e.log()
	e.emit(t, l1, "run_1", core.TopicRunCreated)
	e.emit(t, l1, "run_1", core.TopicRunStarted)
	e.emit(t, l2, "run_1", core.TopicRunParked) // another process appends
	e.emit(t, l1, "run_1", core.TopicRunResumed)
	r, _ := e.log().Open(bg, T, "run_1", false)
	evs := drain(t, r)
	for i, ev := range evs {
		if seqOf(ev) != int64(i+1) {
			t.Errorf("event %d has seq %d", i, seqOf(ev))
		}
	}
	if len(evs) != 4 {
		t.Errorf("%d events", len(evs))
	}
}

func TestTornLineIsSkippedAndNextEventParses(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	e.emit(t, l, "run_1", core.TopicRunCreated)
	p := filepath.Join(e.dir, "default", "runs", "run_1.jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"specversion":"1.0","id":"evt_x","sou`) // a crash mid-write
	_ = f.Close()
	l2 := e.log()
	e.emit(t, l2, "run_1", core.TopicRunStarted)
	r, _ := l2.Open(bg, T, "run_1", false)
	evs := drain(t, r)
	if len(evs) != 2 || evs[0].Topic != core.TopicRunCreated || evs[1].Topic != core.TopicRunStarted {
		t.Fatalf("events = %+v", evs)
	}
	if seqOf(evs[1]) != 3 {
		t.Errorf("seq after a torn line = %d", seqOf(evs[1]))
	}
}

func TestFollowBlocksThenEndsAtTerminalEvent(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	e.emit(t, l, "run_1", core.TopicRunCreated)
	r, _ := l.Open(bg, T, "run_1", true)
	got := make(chan bus.Topic, 8)
	done := make(chan error, 1)
	go func() {
		for {
			ev, err := r.Next(bg)
			if err != nil {
				done <- err
				return
			}
			got <- ev.Topic
		}
	}()
	if tp := <-got; tp != core.TopicRunCreated {
		t.Fatalf("first = %s", tp)
	}
	select {
	case tp := <-got:
		t.Fatalf("delivered %s before it was written", tp)
	case <-time.After(80 * time.Millisecond):
	}
	e.emit(t, l, "run_1", core.TopicStepStarted)
	if tp := <-got; tp != core.TopicStepStarted {
		t.Fatalf("second = %s", tp)
	}
	e.emit(t, l, "run_1", core.TopicRunFailed)
	if tp := <-got; tp != core.TopicRunFailed {
		t.Fatalf("terminal = %s", tp)
	}
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Errorf("after the terminal event: %v", err)
	}
}

func TestFollowSeesAnotherProcessAndCancels(t *testing.T) {
	e := newEnv(t)
	reader, writer := e.log(), e.log()
	r, _ := reader.Open(bg, T, "run_9", true) // file does not exist yet
	got := make(chan bus.Event, 1)
	go func() {
		ev, err := r.Next(bg)
		if err == nil {
			got <- ev
		}
	}()
	time.Sleep(30 * time.Millisecond)
	e.emit(t, writer, "run_9", core.TopicRunCreated)
	select {
	case ev := <-got:
		if ev.Topic != core.TopicRunCreated {
			t.Errorf("topic %s", ev.Topic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow missed a write by another writer")
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := r.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled follow: %v", err)
	}
}

func TestEmptyAndMissingLogs(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	r, _ := l.Open(bg, T, "run_none", false)
	if _, err := r.Next(bg); !errors.Is(err, io.EOF) {
		t.Errorf("missing log: %v", err)
	}
	if _, err := l.Open(bg, "BAD", "run_1", false); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("bad tenant: %v", err)
	}
	if _, err := l.Open(bg, T, "../x", false); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("path traversal: %v", err)
	}
	if err := l.Emit(bg, T, "../x", e.ev(core.TopicRunCreated, "../x", nil)); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("emit traversal: %v", err)
	}
}

func TestEmitRejectsBadTopicAndIsolatesTenantsAndEngineLog(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	if err := l.Emit(bg, T, "run_1", bus.Event{Topic: "nope", Source: "x", Timestamp: e.clock.Now()}); err == nil {
		t.Error("an invalid topic must be refused")
	}
	e.emit(t, l, "run_1", core.TopicRunCreated)
	other := core.NewEvent(e.clock, e.ids, core.TopicRunCreated, "acme", "run_1", core.RunEvent{})
	if err := l.Emit(bg, "acme", "run_1", other); err != nil {
		t.Fatal(err)
	}
	r, _ := l.Open(bg, "acme", "run_1", false)
	if evs := drain(t, r); len(evs) != 1 || evs[0].Extensions[core.ExtTenant] != "acme" {
		t.Errorf("tenant log = %+v", evs)
	}
	// engine events: run ""
	if err := l.Emit(bg, T, "", e.ev(core.TopicTriggerFired, "", core.TriggerEvent{Tenant: T, Flow: "w"})); err != nil {
		t.Fatal(err)
	}
	er, _ := l.Open(bg, T, "", false)
	if evs := drain(t, er); len(evs) != 1 || evs[0].Topic != core.TopicTriggerFired || seqOf(evs[0]) != 1 {
		t.Errorf("engine log = %+v", evs)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "default", "runs", runlog.EngineFile)); err != nil {
		t.Error(err)
	}
}

func TestBusSubscribersReceiveEvents(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	got := make(chan bus.Event, 4)
	unsub := l.Bus().Subscribe("flo.run.#", func(_ context.Context, ev bus.Event) error { got <- ev; return nil })
	defer unsub()
	e.emit(t, l, "run_1", core.TopicRunCreated)
	select {
	case ev := <-got:
		if seqOf(ev) != 1 || ev.Subject != "run_1" {
			t.Errorf("bus event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bus delivery")
	}
}

func TestConcurrentEmitsKeepOneOrder(t *testing.T) {
	e := newEnv(t)
	l := e.log()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Emit(bg, T, "run_1", core.NewEvent(e.clock, e.ids, core.TopicStepStarted, T, "run_1", core.StepEvent{})); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, _ := l.Open(bg, T, "run_1", false)
	for i, ev := range drain(t, r) {
		if seqOf(ev) != int64(i+1) {
			t.Fatalf("line %d has seq %d", i, seqOf(ev))
		}
	}
}
