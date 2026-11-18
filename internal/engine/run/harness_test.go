package run_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/engine/run"
	"hop.top/flo/spec/node"
)

const owner = "owner"

// ctxRunner makes the scripted Runner honor its context: a Script handler
// only sees the invocation, so one that blocks cannot observe a timeout or a
// cancel. The handler goroutine is left to finish on its own.
type ctxRunner struct{ inner *coretest.Runner }

func (r ctxRunner) Run(ctx context.Context, inv core.Invocation) (core.Result, error) {
	type out struct {
		res core.Result
		err error
	}
	ch := make(chan out, 1)
	go func() { res, err := r.inner.Run(ctx, inv); ch <- out{res, err} }()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-ctx.Done():
		return core.Result{}, ctx.Err() //nolint:wrapcheck // test fake
	}
}

// h is a test engine: the executor over the coretest fakes, on a manual
// clock. Steps run through fx.Runner, scripted per test.
type h struct {
	t   *testing.T
	fx  *coretest.Fixture
	ex  *run.Executor
	ctx context.Context
	// release unblocks scripted handlers that hang on purpose, at cleanup.
	release chan struct{}

	mu   sync.Mutex
	last core.RunID
}

func newH(t *testing.T) *h {
	t.Helper()
	fx := coretest.New()
	svc := fx.Services()
	svc.Runner = ctxRunner{fx.Runner}
	ex := run.New(svc, run.Config{CancelPoll: -1})
	ex.Register(fx.Worker)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h := &h{t: t, fx: fx, ex: ex, ctx: ctx, release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })
	// The owner holds every capability.
	if _, err := fx.Grants.Set(ctx, core.DefaultTenant, owner, core.CapabilityAny); err != nil {
		t.Fatal(err)
	}
	return h
}

var flowName = regexp.MustCompile(`(?m)^flow: (\S+)`)

// flow stores and activates a flow, returning its name.
func (h *h) flow(src string) string {
	h.t.Helper()
	name := flowName.FindStringSubmatch(src)[1]
	sum := sha256.Sum256([]byte(src))
	hash := hex.EncodeToString(sum[:])
	v, _, err := h.fx.DB.Flows.PutVersion(h.ctx, core.NewFlowVersion{
		Tenant: core.DefaultTenant, Name: name, Version: "1.0.0", Hash: hash, Source: []byte(src), At: h.fx.Clock.Now(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.fx.DB.Flows.SetState(h.ctx, core.DefaultTenant, name, v.Hash, core.FlowActive, h.fx.Clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	return name
}

// start creates a run of the active version and leaves its advance job queued.
func (h *h) start(name string, vars map[string]any) core.RunID {
	h.t.Helper()
	res, err := h.fx.Starter.Start(h.ctx, core.StartRequest{
		Tenant: core.DefaultTenant, Flow: name, StartedBy: owner, Vars: vars,
		Trigger: core.TriggerInfo{Kind: "manual", At: h.fx.Clock.Now()},
	})
	if err != nil || !res.Created {
		h.t.Fatalf("start %s = %+v, %v", name, res, err)
	}
	return res.Run.ID
}

// do stores a flow, starts it and runs the queue dry.
func (h *h) do(src string, vars map[string]any) core.RunID {
	h.t.Helper()
	id := h.start(h.flow(src), vars)
	h.drain()
	return id
}

func (h *h) drain() { h.fx.Worker.Drain(h.ctx) }

func (h *h) run(id core.RunID) core.Run {
	h.t.Helper()
	r, err := h.fx.DB.Runs.GetRun(h.ctx, core.DefaultTenant, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// steps returns the step-level records by step id.
func (h *h) steps(id core.RunID) map[string]core.StepRun {
	h.t.Helper()
	all, err := h.fx.DB.Runs.ListSteps(h.ctx, core.DefaultTenant, id)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]core.StepRun{}
	for _, s := range all {
		if s.Element == nil {
			out[s.StepID] = s
		}
	}
	return out
}

// elements returns the element records of a step by index.
func (h *h) elements(id core.RunID, step string) []core.StepRun {
	h.t.Helper()
	all, _ := h.fx.DB.Runs.ListSteps(h.ctx, core.DefaultTenant, id)
	var out []core.StepRun
	for _, s := range all {
		if s.Element != nil && s.StepID == step {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Element.Index < out[j].Element.Index })
	return out
}

// wantRun asserts the run's status and, for a failure, its code.
func (h *h) wantRun(id core.RunID, status core.RunStatus, code core.Code) core.Run {
	h.t.Helper()
	r := h.run(id)
	if r.Status != status {
		h.t.Fatalf("run status = %s (failure %+v), want %s", r.Status, r.Failure, status)
	}
	if code != "" && (r.Failure == nil || r.Failure.Code != code) {
		h.t.Fatalf("run failure = %+v, want code %s", r.Failure, code)
	}
	return r
}

// wantStep asserts a step record's status and reason.
func (h *h) wantStep(id core.RunID, step string, st core.StepStatus, why core.StepReason) core.StepRun {
	h.t.Helper()
	s, ok := h.steps(id)[step]
	if !ok {
		h.t.Fatalf("step %s has no record", step)
	}
	if s.Status != st || s.Reason != why {
		h.t.Fatalf("step %s = %s/%s (failure %+v), want %s/%s", step, s.Status, s.Reason, s.Failure, st, why)
	}
	return s
}

// node registers a node spec, returning its type.
func (h *h) node(s *node.Spec) string {
	h.t.Helper()
	if s.Capabilities == nil {
		s.Capabilities = []string{}
	}
	if s.Kind == "" {
		s.Kind = node.KindCommand
	}
	if s.Privacy == "" {
		s.Privacy = node.PrivacyLocal
	}
	h.fx.Nodes.AddBuiltin(s)
	return s.Node
}

// reads is a node with side effect read (idempotent).
func (h *h) reads(typ string) string {
	return h.node(&node.Spec{Node: typ, SideEffect: node.SideEffectRead})
}

// writes is a non-idempotent write-shared node.
func (h *h) writes(typ string) string {
	return h.node(&node.Spec{Node: typ, SideEffect: node.SideEffectWriteShared})
}

// eventTopics lists the topics of a run's log, optionally of one step.
func (h *h) topics(id core.RunID) []string {
	var out []string
	for _, ev := range h.fx.RunLog.Events(core.DefaultTenant, id) {
		out = append(out, string(ev.Topic))
	}
	return out
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// barrier blocks callers until n have arrived, failing the test when they do
// not all come: proof that n steps ran at the same time.
func barrier(t *testing.T, n int) func() {
	var (
		mu      sync.Mutex
		arrived int
		all     = make(chan struct{})
	)
	return func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			t.Error("steps did not overlap")
		}
	}
}

// starts returns the step ids of the calls in arrival order.
func calledSteps(s *coretest.Script) []string {
	var out []string
	for _, c := range s.Calls() {
		out = append(out, fmt.Sprintf("%s#%d", c.StepID, c.Attempt))
	}
	return out
}

func indexOf(xs []string, x string) int { return slices.Index(xs, x) }

// lastRun is the run a test registered with setLast, for fakes that need to
// know it from inside a callback.
func (h *h) lastRun() core.RunID { h.mu.Lock(); defer h.mu.Unlock(); return h.last }

func (h *h) setLast(id core.RunID) { h.mu.Lock(); h.last = id; h.mu.Unlock() }

func newWithConfig(svc *core.Services, cfg map[string]any) *run.Executor {
	return run.New(svc, run.Config{CancelPoll: -1, Config: cfg})
}
