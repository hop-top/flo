//go:build !windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
)

func (e *env) flowTest(args ...string) (flowtest.Result, result) {
	e.t.Helper()
	var res flowtest.Result
	r := e.jsonOf(&res, append([]string{"flow", "test"}, args...)...)
	return res, r
}

func hasProblem(res flowtest.Result, kind string) bool {
	return slices.ContainsFunc(res.Problems, func(p flowtest.Problem) bool { return p.Kind == kind })
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestFlowTestGatesActivation is the testing story end to end on the binary:
// no activation without a passing test of that exact version; a replay answers
// from the recording and never reaches gh; a recording runs the reads once for
// real and keeps the real stores untouched.
func TestFlowTestGatesActivation(t *testing.T) {
	e := newEnv(t)
	bundle := copyBundle(t, "dependency-watch")
	gh := filepath.Join(e.scratch, "gh.log")
	e.ok("grant", "set", "automation", "vcs.read", "--yes")

	// F6: apply is a write; without --yes and a terminal it asks, and fails.
	if r := e.run("flow", "apply", "-f", bundle); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "confirmation_required") {
		t.Fatalf("apply without --yes: exit %d\n%s", r.Exit, r.Stderr)
	}
	if r := e.run("flow", "show", "dependency-watch"); r.Exit != core.ExitNotFound {
		t.Fatalf("an unconfirmed apply stored a version: exit %d", r.Exit)
	}
	e.ok("flow", "apply", "-f", bundle, "--yes")

	set := func(state string) result { return e.run("flow", "set", "dependency-watch", "--state", state, "--yes") }
	if r := set("active"); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "flow_not_tested") || !strings.Contains(r.Stderr, "flo flow test dependency-watch") {
		t.Fatalf("activation without a test: exit %d\n%s", r.Exit, r.Stderr)
	}

	// A replay without recordings fails and names what is missing; gh is not reached.
	res, r := e.flowTest("dependency-watch")
	if r.Exit != core.ExitGeneral || res.Passed || res.Mode != flowtest.ModeReplay || !hasProblem(res, flowtest.ProblemCassette) || exists(gh) {
		t.Fatalf("replay without a recording: exit %d, %+v, gh log %v\n%s", r.Exit, res, exists(gh), r.Stderr)
	}
	if r := set("active"); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "failed") {
		t.Fatalf("activation after a failing test: exit %d\n%s", r.Exit, r.Stderr)
	}

	// Recording is a write: it asks first, and --dry-run shows the plan.
	if r := e.run("flow", "test", "dependency-watch", "--record"); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "confirmation_required") || exists(gh) {
		t.Fatalf("record without --yes: exit %d\n%s", r.Exit, r.Stderr)
	}
	if r := e.run("flow", "test", "dependency-watch", "--record", "--dry-run"); r.Exit != 0 || exists(gh) || exists(filepath.Join(bundle, "record")) {
		t.Fatalf("record --dry-run: exit %d\n%s", r.Exit, r.Stderr)
	}
	if r := e.run("flow", "test", "dependency-watch", "--record", "--replay", "--yes"); r.Exit == 0 {
		t.Error("--record and --replay together")
	}

	res, r = e.flowTest("dependency-watch", "--record", "--yes")
	if r.Exit != 0 || !res.Passed || res.Mode != flowtest.ModeRecord || len(res.Steps) != 5 {
		t.Fatalf("record: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	if lines := readLines(t, gh); len(lines) != 2 {
		t.Fatalf("the recording read through gh once per node: %v", lines)
	}
	for _, p := range []string{"record/cassettes", "record/plans.json"} {
		if !exists(filepath.Join(bundle, p)) {
			t.Errorf("the recording left no %s", p)
		}
	}
	// Nothing of the sandbox run reached the real stores.
	var runs []runRowView
	e.okJSON(&runs, "run", "list", "--flow", "dependency-watch")
	if len(runs) != 0 {
		t.Errorf("a test left runs behind: %+v", runs)
	}
	if state, _ := e.store().State.Snapshot(t.Context(), tenant, "dependency-watch"); len(state) != 0 {
		t.Errorf("a test committed state: %v", state)
	}

	// The replay does not reach gh, even where the real call would fail.
	if err := os.WriteFile(filepath.Join(e.scratch, "fail-releases"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, r = e.flowTest("dependency-watch", "--replay")
	if r.Exit != 0 || !res.Passed || res.Mode != flowtest.ModeReplay || res.Calls != 2 || res.Replayed != 2 || len(readLines(t, gh)) != 2 {
		t.Fatalf("replay: exit %d, %+v, gh log %v\n%s", r.Exit, res, readLines(t, gh), r.Stderr)
	}

	// expect: blocks of tests/ compare chosen outputs.
	tests := filepath.Join(bundle, "tests")
	if err := os.MkdirAll(tests, 0o750); err != nil {
		t.Fatal(err)
	}
	expect := filepath.Join(tests, "expect.yaml")
	if err := os.WriteFile(expect, []byte("expect:\n  - step: findings\n    status: skipped\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, r = e.flowTest("dependency-watch")
	if r.Exit != core.ExitGeneral || res.Passed || !hasProblem(res, flowtest.ProblemExpect) || res.Expected != 1 || res.Held != 0 {
		t.Fatalf("a failing expect: exit %d, %+v", r.Exit, res)
	}
	if err := os.WriteFile(expect, []byte("expect:\n  - step: findings\n    status: succeeded\n  - step: newadv\n    output: { seen: [GHSA-5cv4-jp36-h3mw, GHSA-vvgc-356p-c3xw] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, r = e.flowTest("dependency-watch"); r.Exit != 0 || !res.Passed || res.Held != 2 {
		t.Fatalf("a holding expect: exit %d, %+v", r.Exit, res)
	}
	if err := os.Remove(filepath.Join(e.scratch, "fail-releases")); err != nil {
		t.Fatal(err)
	}

	var a applied
	e.okJSON(&a, "flow", "set", "dependency-watch", "--state", "active", "--yes")
	if a.State != "active" {
		t.Fatalf("activation after a passing test: %+v", a)
	}

	// A node spec edited after the test is not the tested version.
	if r := set("paused"); r.Exit != 0 {
		t.Fatalf("pause: exit %d\n%s", r.Exit, r.Stderr)
	}
	rewrite(t, filepath.Join(bundle, "nodes", "gh-releases.yaml"), "timeout: 30s", "timeout: 31s")
	if r := set("active"); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "changed since") {
		t.Fatalf("activation after the bundle changed: exit %d\n%s", r.Exit, r.Stderr)
	}
	// Testing the edited bundle on the old recording passes (the argv did not
	// change), and the version can be activated again.
	if res, r = e.flowTest("dependency-watch"); r.Exit != 0 || !res.Passed {
		t.Fatalf("replay of the edited bundle: exit %d, %+v", r.Exit, res)
	}
	if r := set("active"); r.Exit != 0 {
		t.Fatalf("activation after re-testing: exit %d\n%s", r.Exit, r.Stderr)
	}
}

// TestFlowTestPlansWritesAndNeverRunsThem: the inbox flow archives mail
// (irreversible) behind an approval. A test approves, plans every archive and
// runs none; the real tool is only asked to list.
func TestFlowTestPlansWritesAndNeverRunsThem(t *testing.T) {
	e := newEnv(t)
	e.installPack("inbox")
	e.ok("grant", "set", "automation", "mail.read", "--yes")
	e.ok("grant", "set", "automation", "mail.write", "--yes")
	bundle := copyBundle(t, "inbox-triage")
	e.ok("flow", "apply", "-f", bundle, "--yes")

	res, r := e.flowTest("inbox-triage", "--record", "--yes")
	if r.Exit != 0 || !res.Passed {
		t.Fatalf("record: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	if exists(filepath.Join(e.scratch, "applied.log")) {
		t.Fatal("a test applied an irreversible node for real")
	}
	var planned []string
	for _, s := range res.Steps {
		if s.Planned && s.ID == "apply" {
			planned = append(planned, s.Node)
		}
	}
	if len(planned) != 1 {
		t.Fatalf("one message, one plan: %+v", res.Steps)
	}
	plans, err := os.ReadFile(filepath.Join(bundle, "record", "plans.json")) //nolint:gosec // a temp copy
	if err != nil || !strings.Contains(string(plans), "inbox/apply@1") || !strings.Contains(string(plans), "m1") {
		t.Fatalf("plans.json: %s, %v", plans, err)
	}
	if res, r = e.flowTest("inbox-triage"); r.Exit != 0 || !res.Passed || res.Replayed != 1 {
		t.Fatalf("replay: exit %d, %+v", r.Exit, res)
	}
	e.ok("flow", "set", "inbox-triage", "--state", "active", "--yes")
}

const triggerFlow = `flow: on-event
version: 0.1.0
on:
  - manual: {}
tasks: none
steps:
  - id: new
    uses: flo/dedupe@1
    with: { items: "{{ trigger.event.items }}", key: "{{ string(item) }}" }
`

// TestTriggerFileFeedsTestsAndRuns: a flow that reads the trigger payload is
// tested with --trigger-file, the recording keeps the payload, and `run create
// --trigger-file` gives a real run the same.
func TestTriggerFileFeedsTestsAndRuns(t *testing.T) {
	e := newEnv(t)
	bundle := filepath.Join(t.TempDir(), "on-event")
	if err := os.MkdirAll(bundle, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "flow.yaml"), []byte(triggerFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(payload, []byte(`{"event":{"items":["a","b","c"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ok("flow", "apply", "-f", bundle, "--yes")

	// No payload: the flow cannot evaluate trigger.event.items.
	if res, r := e.flowTest("on-event"); r.Exit == 0 || res.Passed {
		t.Fatalf("a test without the payload: exit %d, %+v", r.Exit, res)
	}
	res, r := e.flowTest("on-event", "--record", "--trigger-file", payload, "--yes")
	if r.Exit != 0 || !res.Passed || !slices.Contains(res.Recorded, "record/trigger.json") {
		t.Fatalf("record with a payload: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	if res, r = e.flowTest("on-event"); r.Exit != 0 || !res.Passed {
		t.Fatalf("replay takes the recorded payload: exit %d, %+v", r.Exit, res)
	}
	e.ok("flow", "set", "on-event", "--state", "active", "--yes")

	eng := e.startEngine()
	defer eng.stop()
	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "on-event", "--wait"); r.Exit == 0 || c.Status != "failed" {
		t.Fatalf("a run without the payload: exit %d, %+v", r.Exit, c)
	}
	if r := e.jsonOf(&c, "run", "create", "--flow", "on-event", "--trigger-file", payload, "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("a run with the payload: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
	}
	if r := e.run("run", "create", "--flow", "on-event", "--trigger-file", filepath.Join(t.TempDir(), "nope.json")); r.Exit != core.ExitUsage {
		t.Errorf("a missing trigger file: exit %d", r.Exit)
	}
}

func newadvItems(res flowtest.Result) int {
	for _, s := range res.Steps {
		if s.ID == "newadv" {
			m, _ := s.Output.(map[string]any)
			l, _ := m["items"].([]any)
			return len(l)
		}
	}
	return -1
}

// TestFlowTestStateSeeds: where a test's flow state comes from, how a recording
// keeps it, and that the live state is never touched.
func TestFlowTestStateSeeds(t *testing.T) {
	e := newEnv(t)
	bundle := copyBundle(t, "dependency-watch")
	gh := filepath.Join(e.scratch, "gh.log")
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	e.ok("flow", "apply", "-f", bundle, "--yes")
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(seed, []byte("seen_adv: [GHSA-5cv4-jp36-h3mw]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// --dry-run: the plan names the seed and writes nothing, runs nothing.
	r := e.run("flow", "test", "dependency-watch", "--record", "--state", seed, "--dry-run", "--format", "json")
	if r.Exit != 0 || !strings.Contains(r.Stdout, "state_source") || !strings.Contains(r.Stdout, "GHSA-5cv4-jp36-h3mw") || exists(gh) || exists(filepath.Join(bundle, "record")) {
		t.Fatalf("dry-run: exit %d\n%s%s", r.Exit, r.Stdout, r.Stderr)
	}

	// a seed with an undeclared key is a usage error
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"nope": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.run("flow", "test", "dependency-watch", "--state", bad); r.Exit != core.ExitUsage {
		t.Fatalf("an undeclared key: exit %d\n%s", r.Exit, r.Stderr)
	}

	res, r := e.flowTest("dependency-watch", "--record", "--state", seed, "--yes")
	if r.Exit != 0 || !res.Passed || res.StateSource != "file:"+seed || newadvItems(res) != 1 {
		t.Fatalf("record on a seed: exit %d, %s, items %d\n%s", r.Exit, res.StateSource, newadvItems(res), r.Stderr)
	}
	if b, err := os.ReadFile(filepath.Join(bundle, "record", "state.json")); err != nil || !strings.Contains(string(b), "GHSA-5cv4-jp36-h3mw") { //nolint:gosec // a temp copy
		t.Fatalf("state.json: %s %v", b, err)
	}
	// replay reuses the recorded seed; an explicit --state wins
	res, r = e.flowTest("dependency-watch")
	if r.Exit != 0 || res.StateSource != "recorded" || newadvItems(res) != 1 {
		t.Fatalf("replay: exit %d, %s, items %d", r.Exit, res.StateSource, newadvItems(res))
	}
	res, r = e.flowTest("dependency-watch", "--state", "default")
	if r.Exit != 0 || res.StateSource != "default" || newadvItems(res) != 2 {
		t.Fatalf("explicit default: exit %d, %s, items %d", r.Exit, res.StateSource, newadvItems(res))
	}

	// current: a copy of what a live run committed, and nothing written back
	e.ok("flow", "set", "dependency-watch", "--state", "active", "--yes")
	eng := e.startEngine()
	defer eng.stop()
	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 {
		t.Fatalf("live run: exit %d\n%s", r.Exit, r.Stderr)
	}
	live := func() string {
		s, err := e.store().State.Snapshot(t.Context(), tenant, "dependency-watch")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		return string(b)
	}
	before := live()
	if !strings.Contains(before, "GHSA-vvgc-356p-c3xw") {
		t.Fatalf("the live run committed no state: %s", before)
	}
	res, r = e.flowTest("dependency-watch", "--state", "current")
	if r.Exit != 0 || res.StateSource != "current" || newadvItems(res) != 0 {
		t.Fatalf("current: exit %d, %s, items %d\n%s", r.Exit, res.StateSource, newadvItems(res), r.Stderr)
	}
	if res, r = e.flowTest("dependency-watch", "--state", "default"); r.Exit != 0 || newadvItems(res) != 2 {
		t.Fatalf("default after a live run: exit %d, items %d", r.Exit, newadvItems(res))
	}
	if after := live(); after != before {
		t.Fatalf("a test changed the live state:\n%s\n%s", before, after)
	}
}
