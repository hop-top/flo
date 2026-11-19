package bos_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/command"
	"hop.top/flo/spec/node"
)

// The pack's nodes load and validate, name bos operations, and render the bos
// CLI call.
func loadPack(t *testing.T) map[string]*node.Spec {
	t.Helper()
	lp, err := node.LoadPack(os.DirFS("../../packs"), "bos/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*node.Spec{}
	for _, n := range lp.Nodes {
		out[n.Node] = n
	}
	return out
}

func plan(t *testing.T, s *node.Spec, in map[string]any) []string {
	t.Helper()
	return planBound(t, s, in, nil)
}

func planBound(t *testing.T, s *node.Spec, in map[string]any, adm *core.AdmissionBinding) []string {
	t.Helper()
	x := &command.Executor{Commands: &coretest.Commands{}}
	p, perr := x.Plan(core.Invocation{
		Node: s, In: in, RunAs: "noor", Admission: adm,
		Config: map[string]any{"bos": map[string]any{"scope": scope}},
	})
	if perr != nil {
		t.Fatal(perr)
	}
	argv, _ := p["argv"].([]string)
	return argv
}

func TestStatusNodeReadsTheInstance(t *testing.T) {
	s := loadPack(t)["bos/workflow.status@1"]
	if s == nil || s.SideEffect != node.SideEffectRead {
		t.Fatalf("node = %+v", s)
	}
	argv := plan(t, s, map[string]any{"instance": "workflow_instances_1"})
	if strings.Join(argv, " ") != "bos workflow status workflow_instances_1 --scope "+scope+" --format json" {
		t.Errorf("argv = %v", argv)
	}
}

// The request node is a write of the operation workflow.request-transition,
// its input the request as bos digests it, and its shell line turns what the
// engine hands a step into --proposal and --idempotency-key.
func TestRequestTransitionNodeIsARequestOnly(t *testing.T) {
	s := loadPack(t)["bos/workflow.request-transition@1"]
	if s == nil || s.SideEffect != node.SideEffectWriteShared || len(s.Capabilities) != 1 || s.Capabilities[0] != "workflow.request-transition" {
		t.Fatalf("node = %+v", s)
	}
	in := map[string]any{
		"args":  []any{"workflow_instances_1"},
		"flags": map[string]any{"reason": "checks passed", "state-version": "3", "action": "approve"},
	}
	argv := plan(t, s, in)
	line := strings.Join(argv, " ")
	for _, want := range []string{"workflow_instances_1", "--scope " + scope, "--reason checks passed", "--state-version 3", "--action approve"} {
		if !strings.Contains(line, want) {
			t.Errorf("argv lacks %q: %v", want, argv)
		}
	}
	if strings.Contains(line, "--proposal") || strings.Contains(line, "--idempotency-key") {
		t.Errorf("a request with no approval carries approval flags: %v", argv)
	}
	// it names no decision: nothing in the call votes or decides
	if strings.Contains(line, "decide") {
		t.Errorf("argv = %v", argv)
	}
	// without the optional flags the call has only what was given
	argv = plan(t, s, map[string]any{"args": []any{"i"}, "flags": map[string]any{"reason": "r"}})
	if strings.Contains(strings.Join(argv, " "), "--action") || strings.Contains(strings.Join(argv, " "), "--state-version") {
		t.Errorf("argv = %v", argv)
	}
}

// An approved request carries the proposal and the execution key, taken from
// the admission namespace; no shell line is needed.
func TestRequestTransitionNodeCarriesTheApproval(t *testing.T) {
	s := loadPack(t)["bos/workflow.request-transition@1"]
	in := map[string]any{"args": []any{"i"}, "flags": map[string]any{"reason": "r", "state-version": "1"}}
	argv := planBound(t, s, in, &core.AdmissionBinding{Proposal: "agent_proposals_1", ExecutionKey: "exec_1"})
	if argv[0] != "bos" || !strings.Contains(strings.Join(argv, " "), "--proposal agent_proposals_1 --idempotency-key exec_1") {
		t.Errorf("argv = %v", argv)
	}
}

// The status node reshapes the stage document bos prints (one object).
func TestStatusNodeReshapesTheStageDocument(t *testing.T) {
	s := loadPack(t)["bos/workflow.status@1"]
	raw, err := os.ReadFile("testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	f.fx.Commands.On([]string{"bos", "workflow", "status"}, func(core.CommandSpec) (core.CommandResult, error) {
		return core.CommandResult{Stdout: raw}, nil
	})
	x := &command.Executor{Commands: f.fx.Commands}
	res, xerr := x.Execute(bg, core.Invocation{Node: s, In: map[string]any{"instance": "i"}, RunAs: "noor",
		Config: map[string]any{"bos": map[string]any{"scope": scope}}})
	if xerr != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("result = %+v %v", res, xerr)
	}
	out, _ := res.Output.(map[string]any)
	ts, _ := out["transitions"].([]any)
	if out["state"] != "pending" || fmt.Sprint(out["state_version"]) != "1" || len(ts) != 1 {
		t.Fatalf("output = %v", out)
	}
}
