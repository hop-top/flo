//go:build !windows

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
)

type applied struct {
	Flow    string `json:"flow"`
	Hash    string `json:"hash"`
	Seq     int    `json:"seq"`
	State   string `json:"state"`
	Changed bool   `json:"changed"`
}

// copyBundle copies a testdata flow bundle and returns the copy's path.
func copyBundle(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if out, err := cpR(testdata("flows", name), dst); err != nil {
		t.Fatalf("copy bundle: %v\n%s", err, out)
	}
	return dst
}

// TestWatcher runs a dependency watcher: a schedule trigger, declared command
// nodes that call gh through aps, dedupe against state, findings in the
// output store. T-0040: watcher-shaped fixture succeeds, seen is committed,
// the second run dedupes, and a failing run leaves state alone.
func TestWatcher(t *testing.T) {
	e := newEnv(t)
	bundle := testdata("flows", "dependency-watch")

	// Apply is checked against grants: automation holds nothing yet.
	if r := e.run("flow", "apply", "-f", bundle); r.Exit != core.ExitPermission || !strings.Contains(r.Stderr, "grant_denied") {
		t.Fatalf("apply without the grant: exit %d, stderr %s", r.Exit, r.Stderr)
	}
	e.ok("grant", "set", "automation", "vcs.read", "--yes")

	var a applied
	e.okJSON(&a, "flow", "apply", "-f", bundle)
	if !a.Changed || a.Seq != 1 || a.State != "draft" {
		t.Fatalf("first apply = %+v", a)
	}
	first := a.Hash
	e.okJSON(&a, "flow", "apply", "-f", bundle)
	if a.Changed || a.Hash != first {
		t.Errorf("the same content must be the same version: %+v", a)
	}
	// New content is a new version; the old content is not stored again.
	changed := copyBundle(t, "dependency-watch")
	rewrite(t, filepath.Join(changed, "flow.yaml"), "version: 0.1.0", "version: 0.2.0")
	e.okJSON(&a, "flow", "apply", "-f", changed)
	if !a.Changed || a.Seq != 2 || a.Hash == first {
		t.Fatalf("changed content = %+v", a)
	}
	e.okJSON(&a, "flow", "apply", "-f", bundle)
	if a.Changed || a.Hash != first || a.Seq != 1 {
		t.Errorf("re-applying the first content = %+v", a)
	}

	// A flow that does not validate is refused with its findings.
	bad := copyBundle(t, "dependency-watch")
	rewrite(t, filepath.Join(bad, "flow.yaml"), "gh/releases@1", "gh/nothing@1")
	if r := e.run("flow", "validate", "-f", bad); r.Exit != core.ExitUsage && r.Exit != core.ExitNotFound {
		t.Errorf("validate of a flow naming a missing node: exit %d\n%s", r.Exit, r.Stderr)
	}

	e.okJSON(&a, "flow", "set", "dependency-watch", "--version", first, "--state", "active", "--yes")
	if a.State != "active" || !a.Changed {
		t.Fatalf("activate = %+v", a)
	}
	var trig []triggerRow
	e.okJSON(&trig, "trigger", "list", "--flow", "dependency-watch")
	if len(trig) != 2 {
		t.Fatalf("triggers = %+v", trig)
	}
	schedule := slices.IndexFunc(trig, func(x triggerRow) bool { return x.Kind == "schedule" })
	if schedule < 0 || trig[schedule].Next == "" || trig[schedule].State != "active" {
		t.Errorf("the schedule has a next fire: %+v", trig)
	}

	eng := e.startEngine()
	defer eng.stop()

	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("first run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	st := e.store()
	ctx := context.Background()
	state, err := st.State.Snapshot(ctx, tenant, "dependency-watch")
	if err != nil {
		t.Fatal(err)
	}
	if adv, rel := asStrings(state["seen_adv"]), asStrings(state["seen_rel"]); len(adv) != 2 || len(rel) != 3 {
		t.Fatalf("seen was not committed: %v", state)
	}
	outs, _ := st.Outputs.ListOutputs(ctx, tenant, core.OutputFilter{Run: core.RunID(c.Run)})
	if len(outs) != 1 || outs[0].Kind != core.OutputFindings || len(outs[0].Items) != 5 {
		t.Fatalf("findings of the first run: %+v", outs)
	}
	if lines := readLines(t, filepath.Join(e.scratch, "aps.log")); len(lines) != 2 || !strings.HasPrefix(lines[0], "automation ") {
		t.Errorf("the nodes ran as their profile through aps: %v", lines)
	}

	// Second run: everything was seen, nothing is published again.
	var c2 created
	if r := e.jsonOf(&c2, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 || c2.Status != "succeeded" || c2.Run == c.Run {
		t.Fatalf("second run: exit %d %+v", r.Exit, c2)
	}
	v := e.show(c2.Run)
	if status, reason := v.step("findings"); status != "skipped" || reason != "when_false" {
		t.Errorf("findings of the deduped run: %s %s", status, reason)
	}
	if outs, _ := st.Outputs.ListOutputs(ctx, tenant, core.OutputFilter{}); len(outs) != 1 {
		t.Errorf("a deduped run published %d outputs in all", len(outs))
	}

	// A failing run leaves state as it was: new candidates, a releases call
	// that fails. The next run sees the same candidates.
	fail := filepath.Join(e.scratch, "fail-releases")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var c3 created
	r := e.jsonOf(&c3, "run", "create", "--flow", "dependency-watch", "--var", "package=golang.org/x/crypto@0.1.0", "--wait")
	if r.Exit != core.ExitGeneral || c3.Status != "failed" {
		t.Fatalf("failing run: exit %d %+v\n%s", r.Exit, c3, r.Stderr)
	}
	if status, _ := e.show(c3.Run).step("newadv"); status != "succeeded" {
		t.Errorf("newadv, which staged state, status %q", status)
	}
	after, _ := st.State.Snapshot(ctx, tenant, "dependency-watch")
	if len(asStrings(after["seen_adv"])) != 2 {
		t.Fatalf("a failed run changed state: %v", after)
	}
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	var c4 created
	if r := e.jsonOf(&c4, "run", "create", "--flow", "dependency-watch", "--var", "package=golang.org/x/crypto@0.1.0", "--wait"); r.Exit != 0 {
		t.Fatalf("rerun: exit %d\n%s", r.Exit, r.Stderr)
	}
	after, _ = st.State.Snapshot(ctx, tenant, "dependency-watch")
	if len(asStrings(after["seen_adv"])) != 4 {
		t.Errorf("the rerun saw the same candidates and committed them: %v", after)
	}
}

type triggerRow struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
	Next  string `json:"next_at"`
}

func asStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
