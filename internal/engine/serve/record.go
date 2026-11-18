package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"hop.top/flo/internal/check/eva"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/nodes/command"
	"hop.top/flo/internal/nodes/dispatch"
	"hop.top/flo/internal/placement"
	"hop.top/flo/internal/runrec"
	"hop.top/flo/spec/cel"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

// Recording a run (`flo run create --record`).
//
// A recorded run is executed by the engine as any other, but its job handler
// opens a recSession (the recording's files, the redactor) that rides on the
// context. The run's Runner and Checker see it and send the exec and HTTP
// calls of the nodes a replay would execute through a Tape of their own, over
// the same live seams: the call is made for real and kept as a cassette, with
// the redact paths of the flow's nodes dropped and secrets scrubbed out.
// Recording never changes the run: what cannot be kept is noted, and the
// recording is partial (a partial recording is refused by replay).

type recKey struct{}

func sessionFrom(ctx context.Context) *recSession {
	s, _ := ctx.Value(recKey{}).(*recSession)
	return s
}

// recSession is the recording of one run for the length of one turn.
type recSession struct {
	e      *Engine
	tenant core.TenantID
	run    core.RunID
	redact *runrec.Redactor // the redact paths of the flow's nodes, and the secrets of the environment
	cass   string

	mu   sync.Mutex
	meta runrec.Meta
}

// advance is the run.advance job handler: the executor's, with the recording
// of a run started with --record around it.
func (e *Engine) advance(ctx context.Context, j core.Job) error {
	var p core.RunAdvancePayload
	if json.Unmarshal(j.Payload, &p) == nil {
		if run, err := e.Services.Runs.GetRun(ctx, p.Tenant, p.Run); err == nil && run.Record && !run.Status.Terminal() {
			if s := e.beginRecording(ctx, run); s != nil {
				err := e.Executor.Handle(context.WithValue(ctx, recKey{}, s), j)
				e.endRecording(context.WithoutCancel(ctx), s)
				return err
			}
		}
	}
	return e.Executor.Handle(ctx, j)
}

// beginRecording opens the recording of run, or takes up the one an earlier
// turn began. A fault is logged and the run goes unrecorded: recording never
// stops a run.
func (e *Engine) beginRecording(ctx context.Context, run core.Run) *recSession {
	s, err := e.openRecording(ctx, run)
	if err != nil {
		e.Config.Logger.Error("run recording not started", "run", string(run.ID), "error", err)
		return nil
	}
	return s
}

func (e *Engine) openRecording(ctx context.Context, run core.Run) (*recSession, error) {
	v, err := e.Services.Flows.GetVersion(ctx, run.Tenant, run.Flow.Name, run.Flow.Hash)
	if err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	f, err := cel.ParseFlow(bytes.NewReader(v.Source), v.Name+".yaml")
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	paths, err := e.redactPaths(v, f)
	if err != nil {
		return nil, err
	}
	var secrets []string
	for _, kv := range os.Environ() {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, SecretPrefix) {
			secrets = append(secrets, val)
		}
	}
	rs := e.Recordings
	if err := rs.Open(run.Tenant, run.ID); err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	cass, _ := rs.Cassettes(run.Tenant, run.ID)
	s := &recSession{e: e, tenant: run.Tenant, run: run.ID, redact: runrec.NewRedactor(paths, secrets...), cass: cass}
	m, found, err := rs.Load(run.Tenant, run.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	if found {
		s.meta = m
		return s, nil
	}
	privacy := set.Privacy
	if privacy == "" {
		privacy = specrun.PrivacyLocalOnly
	}
	s.meta = runrec.Meta{
		Run: string(run.ID), Flow: v.Name, Version: v.Version, Hash: v.Hash, Privacy: string(privacy),
		Status: runrec.StatusRecording, CreatedAt: e.Config.Clock.Now().UTC(), Redacted: slices.Compact(slices.Sorted(slices.Values(paths))),
	}
	// What the replay starts from, except the state, which the run takes at
	// its first turn (closeTurn). Redact paths are dropped from every doc;
	// a replay asks for what was dropped (ReplayRequest overrides).
	docs := map[string]any{}
	for _, d := range []struct {
		file, label string
		in          map[string]any
	}{{runrec.TriggerFile, "trigger", run.Trigger.Payload}, {runrec.VarsFile, "vars", run.Vars}} {
		doc, dropped := s.redact.DocDropped(orEmpty(d.in))
		docs[d.file] = doc
		for _, p := range dropped {
			s.meta.Dropped = append(s.meta.Dropped, d.label+"."+p)
		}
	}
	for name, doc := range docs {
		if err := rs.WriteJSON(run.Tenant, run.ID, name, doc); err != nil {
			return nil, err //nolint:wrapcheck // *core.Error
		}
	}
	if err := rs.Save(run.Tenant, s.meta); err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	return s, nil
}

// endRecording writes what the turn added: the starting state (taken by the
// run at its first turn), the counts, and, when the run ended, the close.
func (e *Engine) endRecording(ctx context.Context, s *recSession) {
	if err := e.closeTurn(ctx, s); err != nil {
		e.Config.Logger.Error("run recording not saved", "run", string(s.run), "error", err)
	}
}

func (e *Engine) closeTurn(ctx context.Context, s *recSession) error {
	run, err := e.Services.Runs.GetRun(ctx, s.tenant, s.run)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	rs := e.Recordings
	if run.StateSnapshot != nil && !rs.Exists(s.tenant, s.run, runrec.StateFile) {
		state := orEmpty(run.StateSnapshot)
		out := make(map[string]any, len(state))
		names := s.redact.Names()
		var dropped []string
		for k, v := range state {
			if slices.Contains(names, k) {
				dropped = append(dropped, "state."+k)
				continue
			}
			out[k] = v
		}
		doc, more := s.redact.DocDropped(out)
		for _, p := range more {
			dropped = append(dropped, "state."+p)
		}
		if err := rs.WriteJSON(s.tenant, s.run, runrec.StateFile, doc); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		s.mu.Lock()
		s.meta.Dropped = append(s.meta.Dropped, dropped...)
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.Status.Terminal() {
		s.meta.Status, s.meta.RunStatus = runrec.StatusComplete, string(run.Status)
		now := e.Config.Clock.Now().UTC()
		s.meta.FinishedAt = &now
	}
	return rs.Save(s.tenant, s.meta) //nolint:wrapcheck // *core.Error
}

// redactorFor is the redactor of one call: the flow's, the secrets any node
// has resolved so far, and the redacted inputs of the node being run.
func (s *recSession) redactorFor(in map[string]any) *runrec.Redactor {
	r := s.redact.With(s.e.secretValues()...)
	if in != nil {
		r = r.With(r.ValuesAt(in)...)
	}
	return r
}

// tape returns a Tape that records the calls of one node attempt (in nil:
// the contract check) over the live seams.
func (s *recSession) tape(in map[string]any) (*flowtest.Tape, error) {
	return flowtest.NewTapeWith(flowtest.ModeRecord, s.cass, flowtest.Options{
		Live:  true,
		Scrub: func(v any) any { return s.redactorFor(in).Payload(v) },
	})
}

// done adds what a Tape kept, or could not keep, to the recording.
func (s *recSession) done(t *flowtest.Tape) {
	calls, _ := t.Stats()
	s.partial(calls, t.Partial()...)
}

func (s *recSession) partial(calls int, why ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.Calls += calls
	for _, w := range why {
		if !slices.Contains(s.meta.Partial, w) {
			s.meta.Partial = append(s.meta.Partial, w)
		}
	}
}

// recordingRunner is the run's core.Runner: placement, and, for a recorded
// run, the node attempts a replay will execute go through a tape.
type recordingRunner struct {
	e     *Engine
	inner core.Runner
}

func (r recordingRunner) Run(ctx context.Context, inv core.Invocation) (core.Result, error) {
	s := sessionFrom(ctx)
	if s == nil || inv.Kind != core.StepKindNode || inv.Node == nil || !dispatch.ExecutesInDryRun(inv.Node.SideEffect) {
		return r.inner.Run(ctx, inv) //nolint:wrapcheck // the runner's own contract
	}
	if k := inv.Node.Kind; (k == node.KindCommand || k == node.KindHTTP) && !inv.Node.EffectiveRecord() {
		s.partial(0, "node "+inv.Node.Node+" has record: false, so its calls are not recorded")
		return r.inner.Run(ctx, inv) //nolint:wrapcheck // the runner's own contract
	}
	tape, err := s.tape(inv.In)
	if err != nil {
		s.partial(0, err.Error())
		return r.inner.Run(ctx, inv) //nolint:wrapcheck // the runner's own contract
	}
	deps := r.e.dispDeps
	deps.Commands = command.NewRunner(tape.Exec(r.e.liveExec))
	deps.HTTP = tape.HTTP(r.e.liveHTTP)
	defer s.done(tape)
	return placement.New(humanDispatcher{dispatch.New(deps)}).Run(ctx, inv) //nolint:wrapcheck // the runner's own contract
}

// recordingChecker is the run's core.Checker; the contract check of a
// recorded run goes through a tape.
type recordingChecker struct {
	e     *Engine
	inner core.Checker
}

func (c recordingChecker) Check(ctx context.Context, req core.CheckRequest) (core.CheckResult, error) {
	s := sessionFrom(ctx)
	if s == nil {
		return c.inner.Check(ctx, req) //nolint:wrapcheck // the checker's own contract
	}
	tape, err := s.tape(nil)
	if err != nil {
		s.partial(0, err.Error())
		return c.inner.Check(ctx, req) //nolint:wrapcheck // the checker's own contract
	}
	defer s.done(tape)
	return eva.New(tape.HTTP(c.e.liveHTTP)).Check(ctx, req) //nolint:wrapcheck // the checker's own contract
}

// secretLog remembers the value of every secret a node resolved, so a
// recording can scrub it from whatever it keeps, whichever store it came from.
type secretLog struct {
	core.Secrets
	mu   sync.Mutex
	seen map[string]struct{}
}

func (l *secretLog) Get(ctx context.Context, tenant core.TenantID, profile, name string) ([]byte, error) {
	v, err := l.Secrets.Get(ctx, tenant, profile, name)
	if err == nil && len(v) > 0 {
		l.mu.Lock()
		if l.seen == nil {
			l.seen = map[string]struct{}{}
		}
		l.seen[string(v)] = struct{}{}
		l.mu.Unlock()
	}
	return v, err //nolint:wrapcheck // the store's own contract
}

func (l *secretLog) values() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.seen))
	for v := range l.seen {
		out = append(out, v)
	}
	return out
}

func (e *Engine) secretValues() []string { return e.secretLog.values() }

// pruneRecordings keeps recordings as long as outputs (core.OutputRetention),
// the retention of run data.
func (e *Engine) pruneRecordings(_ context.Context, tenant core.TenantID, cutoff time.Time) error {
	_, err := e.Recordings.Prune(tenant, cutoff)
	return err //nolint:wrapcheck // *core.Error
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
