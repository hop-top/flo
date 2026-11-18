package run_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/spec/node"
)

// drainAsync runs the queue in the background, for tests that move the clock.
func (h *h) drainAsync() <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); h.drain() }()
	return done
}

func (h *h) waitCalls(s *coretest.Script, n int) {
	h.t.Helper()
	waitFor(h.t, "calls", func() bool { return len(s.Calls()) >= n })
}

func finished(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish")
	}
}

// timeline records the clock at every call of a step.
type timeline struct {
	mu sync.Mutex
	at []time.Time
}

func (tl *timeline) mark(c core.Clock) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.at = append(tl.at, c.Now())
}

func (tl *timeline) get() []time.Time {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([]time.Time(nil), tl.at...)
}

func TestRetriesRunAtBackoffInstants(t *testing.T) {
	h := newH(t)
	h.reads("t/flaky@1")
	var tl timeline
	n := 0
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "r" },
		func(core.Invocation) (core.Result, error) {
			tl.mark(h.fx.Clock)
			n++
			if n < 3 {
				return coretest.Fail(core.CodeProviderRateLimited, "slow down", core.EffectUnknown), nil
			}
			return coretest.OK("done"), nil
		})
	start := h.fx.Clock.Now()
	id := h.start(h.flow(`flow: retry
version: 1.0.0
steps:
  - id: r
    uses: t/flaky@1
    retry: { max_attempts: 3, backoff: 30s }
`), nil)
	done := h.drainAsync()

	h.waitCalls(&h.fx.Runner.Script, 1)
	h.fx.Clock.BlockUntil(1)
	h.fx.Clock.Advance(29 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if c := len(h.fx.Runner.Calls()); c != 1 {
		t.Fatalf("calls = %d before the backoff elapsed", c)
	}
	h.fx.Clock.Advance(time.Second)
	h.waitCalls(&h.fx.Runner.Script, 2)
	h.fx.Clock.BlockUntil(1)
	h.fx.Clock.Advance(30 * time.Second)
	finished(t, done)

	h.wantRun(id, core.RunSucceeded, "")
	got := tl.get()
	want := []time.Time{start, start.Add(30 * time.Second), start.Add(60 * time.Second)}
	for i := range want {
		if i >= len(got) || !got[i].Equal(want[i]) {
			t.Fatalf("attempt instants = %v, want %v", got, want)
		}
	}
	s := h.steps(id)["r"]
	if len(s.Attempts) != 3 || s.Attempts[0].Outcome != core.OutcomeFailed || s.Attempts[2].Outcome != core.OutcomeOK {
		t.Errorf("attempts = %+v", s.Attempts)
	}
	if !contains(h.topics(id), "flo.run.step.retried") {
		t.Errorf("events = %v", h.topics(id))
	}
}

func contains(xs []string, x string) bool { return indexOf(xs, x) >= 0 }

func TestPermanentFailureIsNotRetried(t *testing.T) {
	h := newH(t)
	h.reads("t/flaky@1")
	h.fx.Runner.ForStep("r", coretest.Fail(core.CodeNodeReportedFailure, "no", core.EffectDone))
	id := h.do(`flow: noretry
version: 1.0.0
steps:
  - id: r
    uses: t/flaky@1
    retry: { max_attempts: 5, backoff: 1s }
`, nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	if n := len(h.fx.Runner.Calls()); n != 1 {
		t.Errorf("calls = %d", n)
	}
}

// A node that is not idempotent and has no key is never repeated after an
// outcome that is unknown; a failure known not to have executed is retried.
func TestNonIdempotentRetriesOnlyWhenNothingExecuted(t *testing.T) {
	h := newH(t)
	h.writes("t/send@1")
	n := 0
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "s" },
		func(core.Invocation) (core.Result, error) {
			n++
			if n == 1 {
				return coretest.Fail(core.CodeProviderRateLimited, "refused", core.EffectNone), nil
			}
			return coretest.OK(nil), nil
		})
	id := h.start(h.flow(`flow: safe
version: 1.0.0
steps:
  - id: s
    uses: t/send@1
    retry: { max_attempts: 3, backoff: 1s }
`), nil)
	done := h.drainAsync()
	h.fx.Clock.BlockUntil(1)
	h.fx.Clock.Advance(time.Second)
	finished(t, done)
	h.wantRun(id, core.RunSucceeded, "")
	if n != 2 {
		t.Errorf("calls = %d, want a retry after a failure that executed nothing", n)
	}
}

func TestUncertainOutcomeParksForADecision(t *testing.T) {
	for name, verdict := range map[string]core.Verdict{"approve reruns": core.VerdictApprove, "reject fails": core.VerdictReject} {
		t.Run(name, func(t *testing.T) {
			h := newH(t)
			h.writes("t/send@1")
			h.fx.Runner.ForStep("s",
				coretest.Fail(core.CodeTimeout, "no answer", core.EffectUnknown), coretest.OK("sent"))
			id := h.do(`flow: uncertain
version: 1.0.0
steps:
  - id: s
    uses: t/send@1
    retry: { max_attempts: 3, backoff: 1s }
`, nil)
			r := h.wantRun(id, core.RunAwaitingApproval, "")
			s := h.wantStep(id, "s", core.StepWaiting, core.ReasonUncertain)
			if n := len(h.fx.Runner.Calls()); n != 1 {
				t.Fatalf("calls = %d, nothing is repeated automatically", n)
			}
			if s.Approval == "" || r.WaitingFor != core.WaitingForApproval(s.Approval) {
				t.Fatalf("waiting_for = %q, step approval %q", r.WaitingFor, s.Approval)
			}
			a, _ := h.fx.Approvals.Get(h.ctx, core.DefaultTenant, s.Approval)
			if a.Kind != core.ApprovalHuman || len(a.Items) != 1 || a.Items[0].ID != core.HumanItemID {
				t.Fatalf("approval = %+v", a)
			}
			if _, err := h.fx.Approvals.Decide(h.ctx, core.DefaultTenant, core.DecideRequest{
				Approval: a.ID, Items: []core.ItemVerdict{{Item: core.HumanItemID, Verdict: verdict}}, By: "jad",
			}); err != nil {
				t.Fatal(err)
			}
			h.drain()
			if verdict == core.VerdictApprove {
				h.wantRun(id, core.RunSucceeded, "")
				if n := len(h.fx.Runner.Calls()); n != 2 {
					t.Errorf("calls = %d, want the accepted re-run", n)
				}
				return
			}
			h.wantRun(id, core.RunFailed, core.CodeRunFailed)
			if f := h.wantStep(id, "s", core.StepFailed, "").Failure; f.Code != core.CodeUncertain {
				t.Errorf("failure = %+v", f)
			}
			if n := len(h.fx.Runner.Calls()); n != 1 {
				t.Errorf("calls = %d, a rejected step is not repeated", n)
			}
		})
	}
}

func TestInterruptedAttemptIsUncertainUnlessRepeatable(t *testing.T) {
	for name, tc := range map[string]struct {
		node  func(*h) string
		state core.RunStatus
		step  core.StepStatus
	}{
		"idempotent reruns":     {func(h *h) string { return h.reads("t/read@1") }, core.RunSucceeded, core.StepSucceeded},
		"non-idempotent parked": {func(h *h) string { return h.writes("t/send@1") }, core.RunAwaitingApproval, core.StepWaiting},
	} {
		t.Run(name, func(t *testing.T) {
			h := newH(t)
			typ := tc.node(h)
			id := h.start(h.flow(`flow: crashed
version: 1.0.0
steps:
  - id: s
    uses: `+typ+`
    retry: { max_attempts: 2, backoff: 1s }
`), nil)
			// The engine stopped while attempt 1 was in flight.
			now := h.fx.Clock.Now()
			rec := core.StepRun{
				ID: core.NewStepRunID(h.fx.IDs), Tenant: core.DefaultTenant, Run: id, StepID: "s", Kind: core.StepKindNode,
				Node: typ, Status: core.StepRunning, StartedAt: &now,
			}
			if err := h.fx.DB.Runs.PutStep(h.ctx, core.DefaultTenant, rec); err != nil {
				t.Fatal(err)
			}
			done := h.drainAsync()
			if tc.state == core.RunSucceeded {
				h.fx.Clock.BlockUntil(1)
				h.fx.Clock.Advance(time.Second)
			}
			finished(t, done)
			h.wantRun(id, tc.state, "")
			if st := h.steps(id)["s"].Status; st != tc.step {
				t.Errorf("step = %s, want %s", st, tc.step)
			}
		})
	}
}

func TestAttemptTimeoutIsAnUnknownOutcome(t *testing.T) {
	h := newH(t)
	h.node(&node.Spec{Node: "t/slow@1", SideEffect: node.SideEffectRead, Timeout: "10s"})
	var calls atomic.Int32
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "s" },
		func(inv core.Invocation) (core.Result, error) {
			if calls.Add(1) == 1 {
				if inv.Timeout != 10*time.Second {
					t.Errorf("invocation timeout = %v, want the node's default", inv.Timeout)
				}
				<-h.release
				return core.Result{}, nil
			}
			return coretest.OK("late"), nil
		})
	id := h.start(h.flow(`flow: slow
version: 1.0.0
steps:
  - id: s
    uses: t/slow@1
    retry: { max_attempts: 2, backoff: 5s }
`), nil)
	done := h.drainAsync()
	h.waitCalls(&h.fx.Runner.Script, 1)
	h.fx.Clock.BlockUntil(1)
	h.fx.Clock.Advance(10 * time.Second) // the attempt's timeout fires
	waitFor(t, "backoff", func() bool { d, ok := h.fx.Clock.NextDeadline(); return ok && d.After(h.fx.Clock.Now()) })
	h.fx.Clock.Advance(5 * time.Second)
	finished(t, done)
	h.wantRun(id, core.RunSucceeded, "")
	a := h.steps(id)["s"].Attempts
	if len(a) != 2 || a[0].Failure == nil || a[0].Failure.Code != core.CodeTimeout || a[0].Effect != core.EffectUnknown {
		t.Errorf("attempts = %+v", a)
	}
}

func TestStepTimeoutOverridesNodeAndExecTimeoutOfCoreStepParks(t *testing.T) {
	h := newH(t)
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "x" },
		func(inv core.Invocation) (core.Result, error) {
			if inv.Timeout != 3*time.Second {
				t.Errorf("timeout = %v, want the step's", inv.Timeout)
			}
			<-h.release
			return core.Result{}, nil
		})
	id := h.start(h.flow(`flow: coretimeout
version: 1.0.0
steps:
  - id: x
    kind: exec
    timeout: 3s
    exec: { argv: [sleep, "9"] }
`), nil)
	done := h.drainAsync()
	h.waitCalls(&h.fx.Runner.Script, 1)
	h.fx.Clock.BlockUntil(1)
	h.fx.Clock.Advance(3 * time.Second)
	finished(t, done)
	// exec is not idempotent: a timeout may have run it
	h.wantRun(id, core.RunAwaitingApproval, "")
	h.wantStep(id, "x", core.StepWaiting, core.ReasonUncertain)
}
