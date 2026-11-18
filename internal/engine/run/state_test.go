package run_test

import (
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const stateFlow = `flow: stateful
version: 1.0.0
state:
  seen: { default: [] }
  cursor: { default: null }
steps:
  - id: a
    uses: t/read@1
    with: { before: "{{ state.seen }}" }
    check: { contract: seen, eva_url: "http://eva" }
    outputs: { state: { seen: "{{ result.seen }}", cursor: "{{ result.cursor }}" } }
  - id: b
    uses: t/read@1
    depends_on: [a]
    with: { before: "{{ state.seen }}" }
  - id: c
    uses: t/read@1
    depends_on: [b]
`

func (h *h) snapshot(flow string) map[string]any {
	h.t.Helper()
	s, err := h.fx.DB.State.Snapshot(h.ctx, core.DefaultTenant, flow)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestStateSnapshotAtStartStagedAfterCheckCommittedOnSuccess(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"seen": []any{"x"}, "cursor": int64(7)}))
	var stagedAtCheck map[string]any
	h.fx.Checker.Verdict = func(core.CheckRequest) (core.CheckResult, error) {
		stagedAtCheck, _ = h.fx.DB.State.Staged(h.ctx, core.DefaultTenant, h.lastRun())
		return core.CheckResult{Passed: true}, nil
	}
	id := h.start(h.flow(stateFlow), nil)
	h.setLast(id)
	h.drain()
	r := h.wantRun(id, core.RunSucceeded, "")

	if len(stagedAtCheck) != 0 {
		t.Errorf("state staged before the check passed: %v", stagedAtCheck)
	}
	// the snapshot taken at start: declared defaults, null included
	if seen, ok := r.StateSnapshot["seen"].([]any); !ok || len(seen) != 0 {
		t.Errorf("snapshot = %#v", r.StateSnapshot)
	}
	if v, ok := r.StateSnapshot["cursor"]; !ok || v != nil {
		t.Errorf("snapshot cursor = %#v, %v", v, ok)
	}
	// b runs after a staged its write and still reads the snapshot
	if before := h.fx.Runner.CallsFor("b")[0].In["before"].([]any); len(before) != 0 {
		t.Errorf("b saw its own run's staged write: %v", before)
	}
	got := h.snapshot("stateful")
	if s := got["seen"].([]any); len(s) != 1 || s[0] != "x" || got["cursor"] != int64(7) {
		t.Errorf("committed state = %v", got)
	}
	// the next run starts from the committed state
	id2 := h.start("stateful", nil)
	h.drain()
	if before := h.fx.Runner.CallsFor("a")[1].In["before"].([]any); len(before) != 1 {
		t.Errorf("second run read %v", before)
	}
	_ = id2
}

func TestFailedRunLeavesStateUntouched(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"seen": []any{"x"}, "cursor": 1}))
	h.fx.Runner.ForStep("b", coretest.Fail(core.CodeNodeReportedFailure, "boom", core.EffectDone))
	id := h.do(stateFlow, nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	if got := h.snapshot("stateful"); len(got) != 0 {
		t.Errorf("a failed run committed state: %v", got)
	}
	if staged, _ := h.fx.DB.State.Staged(h.ctx, core.DefaultTenant, id); len(staged) != 0 {
		t.Errorf("staged writes were kept: %v", staged)
	}
}

func TestStateTooLargeFailsRun(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	big := make([]byte, core.MaxStateValueBytes+10)
	for i := range big {
		big[i] = 'x'
	}
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"seen": string(big), "cursor": 1}))
	id := h.do(stateFlow, nil)
	h.wantRun(id, core.RunFailed, core.CodeStateTooLarge)
}

func TestCheckFailedIs422AndStopsDependents(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"seen": []any{"x"}, "cursor": 1}))
	h.fx.Checker.Verdict = func(core.CheckRequest) (core.CheckResult, error) {
		return core.CheckResult{Passed: false, Message: "missing field"}, nil
	}
	id := h.do(stateFlow, nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	s := h.wantStep(id, "a", core.StepFailed, "")
	if s.Failure.Code != core.CodeCheckFailed || s.Failure.Message != "check seen: missing field" {
		t.Errorf("failure = %+v", s.Failure)
	}
	h.wantStep(id, "b", core.StepSkipped, core.ReasonUpstreamFailed)
	if n := len(h.fx.Runner.CallsFor("a")); n != 1 {
		t.Errorf("a ran %d times, a failed check is not transient", n)
	}
	if c := h.fx.Checker.Calls()[0]; c.Contract != "seen" || c.URL != "http://eva" || c.Body.(map[string]any)["cursor"] != int64(1) {
		t.Errorf("check request = %+v", c)
	}
}

func TestCheckGatewayDownRetriesTheCheckNotTheStep(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("a", coretest.OK(map[string]any{"seen": []any{}, "cursor": 1}))
	n := 0
	h.fx.Checker.Verdict = func(core.CheckRequest) (core.CheckResult, error) {
		n++
		if n < 3 {
			return core.CheckResult{}, core.New(core.CodeCheckUnavailable, "503")
		}
		return core.CheckResult{Passed: true}, nil
	}
	id := h.start(h.flow(`flow: gateway
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
    retry: { max_attempts: 3, backoff: 10s }
    check: { contract: c, eva_url: "http://eva" }
`), nil)
	done := h.drainAsync()
	for i := 0; i < 2; i++ {
		h.fx.Clock.BlockUntil(1)
		h.fx.Clock.Advance(10 * time.Second)
	}
	finished(t, done)
	h.wantRun(id, core.RunSucceeded, "")
	if n != 3 || len(h.fx.Runner.CallsFor("a")) != 1 {
		t.Errorf("check calls = %d, step calls = %d", n, len(h.fx.Runner.CallsFor("a")))
	}
}

func TestCheckURLFallsBackToConfig(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	svc := h.fx.Services()
	svc.Runner = ctxRunner{h.fx.Runner}
	h.ex = newWithConfig(svc, map[string]any{"eva": map[string]any{"url": "http://cfg"}})
	h.ex.Register(h.fx.Worker)
	id := h.do(`flow: cfgurl
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
    check: { contract: c }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if got := h.fx.Checker.Calls()[0].URL; got != "http://cfg" {
		t.Errorf("url = %q", got)
	}
}
