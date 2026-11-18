package run_test

import (
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

func TestLinearRunSucceedsAndLogsInOrder(t *testing.T) {
	h := newH(t)
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"n": 1}))
	id := h.do(`flow: lin
version: 1.0.0
steps:
  - id: a
    kind: exec
    exec: { argv: [echo, hi] }
  - id: b
    kind: exec
    depends_on: [a]
    exec: { argv: [echo, "{{ results.a.n }}"] }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	h.wantStep(id, "a", core.StepSucceeded, "")
	b := h.wantStep(id, "b", core.StepSucceeded, "")
	if argv := h.fx.Runner.CallsFor("b")[0].Exec.Argv; len(argv) != 2 || argv[1] != "1" {
		t.Errorf("b argv = %v, want the rendered result", argv)
	}
	if b.Inputs == nil || b.Attempts[0].N != 1 {
		t.Errorf("record keeps inputs and attempts: %+v", b)
	}
	want := []string{
		"flo.run.run.created", "flo.run.run.started",
		"flo.run.admission.decided", "flo.run.step.started", "flo.run.step.succeeded",
		"flo.run.admission.decided", "flo.run.step.started", "flo.run.step.succeeded",
		"flo.run.run.succeeded",
	}
	if got := h.topics(id); !equal(got, want) {
		t.Errorf("events = %v\nwant     %v", got, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Readiness follows results references without depends_on, and independent
// ready steps run at the same time.
func TestImplicitEdgesAndObservedOverlap(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	meet := barrier(t, 2)
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"v": "x"}))
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "c" || inv.StepID == "d" },
		func(core.Invocation) (core.Result, error) { meet(); return coretest.OK(nil), nil })
	id := h.do(`flow: edges
version: 1.0.0
steps:
  - id: b
    uses: t/read@1
    with: { q: "{{ results.a.v }}" }
  - id: a
    uses: t/read@1
  - id: c
    uses: t/read@1
  - id: d
    uses: t/read@1
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	calls := calledSteps(&h.fx.Runner.Script)
	if indexOf(calls, "a#1") > indexOf(calls, "b#1") {
		t.Errorf("b reads results.a and must start after it: %v", calls)
	}
	if got := h.fx.Runner.CallsFor("b")[0].In["q"]; got != "x" {
		t.Errorf("b q = %v", got)
	}
	if h.fx.Runner.MaxInFlight() < 2 {
		t.Errorf("max in flight = %d, ready steps must overlap", h.fx.Runner.MaxInFlight())
	}
}

func TestWhenFalseSkipsAndAbsentResultsSkipReaders(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("a", coretest.OK(1))
	id := h.do(`flow: whens
version: 1.0.0
vars:
  go: { type: boolean, default: false }
steps:
  - id: gate
    uses: t/read@1
    when: "{{ vars.go }}"
  - id: after
    uses: t/read@1
    depends_on: [gate]
  - id: reader
    uses: t/read@1
    with: { x: "{{ results.gate.out }}" }
  - id: guarded
    uses: t/read@1
    when: "{{ has(results.gate) }}"
  - id: guard-true
    uses: t/read@1
    with: { present: "{{ has(results.gate) }}" }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	h.wantStep(id, "gate", core.StepSkipped, core.ReasonWhenFalse)
	h.wantStep(id, "after", core.StepSucceeded, "") // a skipped dependency does not stop depends_on
	h.wantStep(id, "reader", core.StepSkipped, core.ReasonUpstreamSkipped)
	h.wantStep(id, "guarded", core.StepSkipped, core.ReasonWhenFalse)
	h.wantStep(id, "guard-true", core.StepSucceeded, "")
	if got := h.fx.Runner.CallsFor("guard-true")[0].In["present"]; got != false {
		t.Errorf("has() sees absence as data, got %v", got)
	}
}

func TestFailedDependencySkipsDependentsAndFailsRun(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("a", coretest.Fail(core.CodeNodeReportedFailure, "no", core.EffectDone))
	id := h.do(`flow: fails
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
  - id: b
    uses: t/read@1
    depends_on: [a]
  - id: c
    uses: t/read@1
    with: { x: "{{ results.b }}" }
  - id: free
    uses: t/read@1
`, nil)
	r := h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	if r.Failure.Detail["step"] != "a" {
		t.Errorf("failure detail = %v", r.Failure.Detail)
	}
	h.wantStep(id, "a", core.StepFailed, "")
	h.wantStep(id, "b", core.StepSkipped, core.ReasonUpstreamFailed)
	h.wantStep(id, "c", core.StepSkipped, core.ReasonUpstreamFailed)
	h.wantStep(id, "free", core.StepSucceeded, "")
	if got := h.topics(id); got[len(got)-1] != "flo.run.run.failed" {
		t.Errorf("last event = %s", got[len(got)-1])
	}
}

func TestEngineFeaturesRefusedBeforeAnythingRuns(t *testing.T) {
	for name, step := range map[string]string{
		"sub-flow": "uses: other@1.0.0",
		"agent":    "agent: helper",
	} {
		t.Run(name, func(t *testing.T) {
			h := newH(t)
			h.reads("t/read@1")
			id := h.do(`flow: feat
version: 1.0.0
steps:
  - id: ok
    uses: t/read@1
  - id: x
    `+step+`
`, nil)
			h.wantRun(id, core.RunFailed, core.CodeEngineFeature)
			if n := len(h.fx.Runner.Calls()); n != 0 {
				t.Errorf("%d steps ran, the run is refused up front", n)
			}
		})
	}
}

func TestUnknownNodeFailsRun(t *testing.T) {
	h := newH(t)
	id := h.do(`flow: nonode
version: 1.0.0
steps:
  - id: x
    uses: no/such@1
`, nil)
	h.wantRun(id, core.RunFailed, core.CodeNodeNotFound)
}

func TestStaleAdvanceJobOfTerminalRunIsNoop(t *testing.T) {
	h := newH(t)
	id := h.do(`flow: once
version: 1.0.0
steps:
  - id: a
    kind: exec
    exec: { argv: [true] }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if err := h.ex.Advance(h.ctx, core.DefaultTenant, id); err != nil {
		t.Fatal(err)
	}
	if n := len(h.fx.Runner.Calls()); n != 1 {
		t.Errorf("calls = %d, a terminal run is not advanced again", n)
	}
}
