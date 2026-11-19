package bos_test

import (
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
	x := &command.Executor{Commands: &coretest.Commands{}}
	p, perr := x.Plan(core.Invocation{
		Node: s, In: in, RunAs: "noor",
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
	if argv[0] != "sh" || !strings.Contains(argv[2], "workflow request-transition") ||
		!strings.Contains(argv[2], "--proposal") || !strings.Contains(argv[2], "FLO_IDEMPOTENCY_KEY") {
		t.Errorf("shell line = %v", argv[:3])
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
