package run_test

import (
	"fmt"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const eachFlow = `flow: each
version: 1.0.0
steps:
  - id: list
    uses: t/read@1
  - id: each
    uses: t/read@1
    for_each: "{{ results.list.items }}"
    concurrency: %d
    on_item_error: %s
    with: { name: "{{ item.name }}", at: "{{ index }}" }
`

func items(names ...string) map[string]any {
	out := []any{}
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return map[string]any{"items": out}
}

func TestForEachBoundsInFlightAndKeepsOrder(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "b", "c", "d", "e", "f")))
	meet := barrier(t, 3) // the first three elements are in flight together
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "each" },
		func(inv core.Invocation) (core.Result, error) {
			if *inv.Index < 3 {
				meet()
			}
			return coretest.OK(map[string]any{"got": inv.In["name"], "i": *inv.Index}), nil
		})
	id := h.do(fmt.Sprintf(eachFlow, 3, "fail"), nil)
	h.wantRun(id, core.RunSucceeded, "")
	if m := h.fx.Runner.MaxInFlight(); m != 3 {
		t.Errorf("max in flight = %d, want exactly the concurrency of 3", m)
	}
	out := h.steps(id)["each"].Output.([]any)
	if len(out) != 6 {
		t.Fatalf("output = %v", out)
	}
	for i, o := range out {
		m := o.(map[string]any)
		if m["i"] != int64(i) || m["got"] != string(rune('a'+i)) {
			t.Errorf("output[%d] = %v, want input order", i, m)
		}
	}
	els := h.elements(id, "each")
	if len(els) != 6 || els[2].Element.Index != 2 || els[2].Element.Item.(map[string]any)["name"] != "c" {
		t.Errorf("each element is its own record with its item: %+v", els)
	}
	if els[4].Inputs.(map[string]any)["at"] != int64(4) {
		t.Errorf("index is bound in with: %v", els[4].Inputs)
	}
}

func TestForEachDefaultConcurrencyIsFour(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "b", "c", "d", "e", "f", "g", "h")))
	meet := barrier(t, 4)
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "each" },
		func(inv core.Invocation) (core.Result, error) {
			if *inv.Index < 4 {
				meet()
			}
			return coretest.OK(nil), nil
		})
	id := h.do(`flow: def4
version: 1.0.0
steps:
  - id: list
    uses: t/read@1
  - id: each
    uses: t/read@1
    for_each: "{{ results.list.items }}"
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if m := h.fx.Runner.MaxInFlight(); m != 4 {
		t.Errorf("max in flight = %d, want the default of 4", m)
	}
}

func TestForEachEmptyListSucceedsWithEmptyList(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items()))
	id := h.do(fmt.Sprintf(eachFlow, 4, "fail"), nil)
	h.wantRun(id, core.RunSucceeded, "")
	got := h.steps(id)["each"].Output
	if l, ok := got.([]any); !ok || len(l) != 0 {
		t.Errorf("output = %#v, want []", got)
	}
	if n := len(h.fx.Runner.CallsFor("each")); n != 0 {
		t.Errorf("%d calls for an empty list", n)
	}
}

func TestForEachOnItemErrorSkipLeavesElementOut(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "b", "c")))
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "each" },
		func(inv core.Invocation) (core.Result, error) {
			if *inv.Index == 1 {
				return coretest.Fail(core.CodeNodeReportedFailure, "bad b", core.EffectDone), nil
			}
			return coretest.OK(inv.In["name"]), nil
		})
	id := h.do(fmt.Sprintf(eachFlow, 2, "skip"), nil)
	h.wantRun(id, core.RunSucceeded, "")
	out := h.steps(id)["each"].Output.([]any)
	if len(out) != 2 || out[0] != "a" || out[1] != "c" {
		t.Errorf("output = %v, the skipped element is left out", out)
	}
	el := h.elements(id, "each")[1]
	if el.Status != core.StepSkipped || el.Reason != core.ReasonItemSkipped || el.Failure == nil || el.Failure.Code != core.CodeNodeReportedFailure {
		t.Errorf("skipped element = %s/%s %+v", el.Status, el.Reason, el.Failure)
	}
}

func TestForEachOnItemErrorFailFailsStepAfterRestFinish(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "b", "c")))
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "each" },
		func(inv core.Invocation) (core.Result, error) {
			if *inv.Index == 0 {
				return coretest.Fail(core.CodeNodeReportedFailure, "bad a", core.EffectDone), nil
			}
			return coretest.OK(inv.In["name"]), nil
		})
	id := h.do(fmt.Sprintf(eachFlow, 4, "fail"), nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	s := h.wantStep(id, "each", core.StepFailed, "")
	if s.Failure.Code != core.CodeNodeReportedFailure {
		t.Errorf("step failure = %+v", s.Failure)
	}
	els := h.elements(id, "each")
	if len(els) != 3 || els[1].Status != core.StepSucceeded || els[2].Status != core.StepSucceeded {
		t.Errorf("the rest finish: %+v", els)
	}
}

func TestForEachWhenIsPerElementAndAbsentListSkips(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "skipme", "c")))
	id := h.do(`flow: eachwhen
version: 1.0.0
steps:
  - id: list
    uses: t/read@1
  - id: each
    uses: t/read@1
    for_each: "{{ results.list.items }}"
    when: "{{ item.name != 'skipme' }}"
    with: { name: "{{ item.name }}" }
  - id: none
    uses: t/read@1
    when: "{{ 1 == 2 }}"
  - id: over-none
    uses: t/read@1
    for_each: "{{ results.none.items }}"
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	els := h.elements(id, "each")
	if els[1].Status != core.StepSkipped || els[1].Reason != core.ReasonWhenFalse {
		t.Errorf("element 1 = %s/%s", els[1].Status, els[1].Reason)
	}
	if out := h.steps(id)["each"].Output.([]any); len(out) != 2 {
		t.Errorf("output = %v", out)
	}
	h.wantStep(id, "over-none", core.StepSkipped, core.ReasonUpstreamSkipped)
}

func TestForEachMapIsAnError(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(map[string]any{"items": map[string]any{"a": 1}}))
	id := h.do(fmt.Sprintf(eachFlow, 4, "fail"), nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	if s := h.steps(id)["each"]; s.Failure == nil || s.Failure.Code != core.CodeEvalError {
		t.Errorf("each = %+v", s)
	}
}

func TestForEachCheckRunsPerElement(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "b")))
	h.fx.Checker.Verdict = func(r core.CheckRequest) (core.CheckResult, error) {
		return core.CheckResult{Passed: true}, nil
	}
	id := h.do(`flow: eachcheck
version: 1.0.0
steps:
  - id: list
    uses: t/read@1
  - id: each
    uses: t/read@1
    for_each: "{{ results.list.items }}"
    check: { contract: "c-{{ item.name }}", eva_url: "http://eva" }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	calls := h.fx.Checker.Calls()
	if len(calls) != 2 {
		t.Fatalf("checks = %+v, want one per element", calls)
	}
	seen := map[string]bool{calls[0].Contract: true, calls[1].Contract: true}
	if !seen["c-a"] || !seen["c-b"] || calls[0].URL != "http://eva" {
		t.Errorf("checks = %+v", calls)
	}
}
