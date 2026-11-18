//go:build !windows

package serve_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

// liveExec is the real world of a recording: gh and the inbox tool, as shell
// fakes would answer. A write that reaches it fails the test.
type liveExec struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
}

func (l *liveExec) Run(cmd *exec.Cmd) error {
	args := cmd.Args
	if i := slices.Index(args, "--"); i >= 0 && args[0] == "aps" {
		args = args[i+1:]
	}
	l.mu.Lock()
	l.calls = append(l.calls, strings.Join(args, " "))
	l.mu.Unlock()
	write := func(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) } //nolint:gosec // a temp step directory
	switch args[0] {
	case "inbox":
		if args[1] == "apply" {
			l.t.Errorf("a write reached the real world: %v", args)
			return nil
		}
		return write(args[3], `{"messages":[{"id":"m1","subject":"Invoice","from":"a@example.org"},{"id":"m2","subject":"Hello","from":"b@example.org"}]}`)
	case "sh": // gh api <path> > <out>
		path, out := args[len(args)-2], args[len(args)-1]
		src := "advisories_go_x_net.json"
		if strings.Contains(path, "/releases") {
			src = "releases_cli_cli.json"
		}
		b, err := os.ReadFile(testdata("github", src))
		if err != nil {
			return err //nolint:wrapcheck // test
		}
		return write(out, string(b))
	}
	l.t.Errorf("unexpected process %v", args)
	return errors.New("unexpected process")
}

func (l *liveExec) Output(cmd *exec.Cmd) ([]byte, error) { return nil, l.Run(cmd) }

func (l *liveExec) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

func (l *liveExec) called(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.calls, func(c string) bool { return strings.Contains(c, sub) })
}

// eva is the contract gateway: it answers every check with status.
type eva struct {
	mu     sync.Mutex
	status int
	calls  int
}

func (e *eva) Do(req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.calls++
	status := e.status
	e.mu.Unlock()
	if status == 0 {
		return nil, errors.New("connection refused")
	}
	rec := httptest.NewRecorder()
	rec.WriteHeader(status)
	_, _ = io.WriteString(rec, `{"message":"label not in set"}`)
	r := rec.Result()
	r.Request = req
	return r, nil
}

type rig struct {
	root string
	e    *serve.Engine
	live *liveExec
	eva  *eva
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	packs := filepath.Join(root, "packs")
	if err := os.MkdirAll(packs, 0o750); err != nil {
		t.Fatal(err)
	}
	copyTree(t, testdata("packs", "inbox"), filepath.Join(packs, "inbox"))
	r := &rig{root: root, live: &liveExec{t: t}, eva: &eva{status: 200}}
	e, err := serve.Open(bg, serve.Config{Root: root, Owner: "jad", NoSync: true, Exec: r.live, HTTP: r.eva})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	r.e = e
	return r
}

// apply stores a copy of a testdata bundle (so recording does not write into
// the repository) and grants what its nodes declare.
func (r *rig) apply(t *testing.T, name string, caps ...string) core.FlowVersion {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), name)
	copyTree(t, testdata("flows", name), bundle)
	return r.applyBundle(t, bundle, caps...)
}

func (r *rig) applyBundle(t *testing.T, bundle string, caps ...string) core.FlowVersion {
	t.Helper()
	for _, c := range caps {
		if _, err := r.e.Grants.Set(bg, core.DefaultTenant, "automation", c); err != nil {
			t.Fatal(err)
		}
	}
	src, err := serve.ReadSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil {
		t.Fatal(err)
	}
	return a.Version
}

func (r *rig) test(t *testing.T, flow string, mode flowtest.Mode) flowtest.Result {
	t.Helper()
	res, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: flow, Mode: mode, Caller: "jad"})
	if err != nil {
		t.Fatalf("TestFlow(%s): %v", mode, err)
	}
	return res
}

func problemKinds(res flowtest.Result) []string {
	var out []string
	for _, p := range res.Problems {
		out = append(out, p.Kind)
	}
	return out
}

func (r *rig) activate(flow string) error {
	_, err := r.e.Flows.SetState(bg, core.DefaultTenant, "jad", flow, "", core.FlowActive)
	return err //nolint:wrapcheck // the test reads the code
}

func TestRecordThenReplayGatesActivation(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")

	if err := r.activate("dependency-watch"); !core.IsCode(err, core.CodeFlowNotTested) || !strings.Contains(err.Error(), "has no test") {
		t.Fatalf("an untested version: %v", err)
	}

	rec := r.test(t, "dependency-watch", flowtest.ModeRecord)
	if !rec.Passed || rec.Mode != flowtest.ModeRecord || rec.Hash != v.Hash || r.live.count() != 2 {
		t.Fatalf("record: %+v, live calls %d", rec, r.live.count())
	}
	for _, want := range []string{"record/cassettes/", "record/plans.json"} {
		if !slices.Contains(rec.Recorded, want) {
			t.Errorf("recorded %v lacks %s", rec.Recorded, want)
		}
	}
	if _, err := os.Stat(filepath.Join(v.Bundle, "record", "cassettes")); err != nil {
		t.Error(err)
	}

	before := r.live.count()
	rep := r.test(t, "dependency-watch", flowtest.ModeReplay)
	if !rep.Passed || rep.Calls != 2 || rep.Replayed != 2 || r.live.count() != before {
		t.Fatalf("replay: %+v, live calls %d -> %d", rep, before, r.live.count())
	}
	if rep.Run == "" || len(rep.Steps) != 5 {
		t.Errorf("steps = %+v", rep.Steps)
	}

	// The sandbox kept nothing of the real engine's: no run, no state, no output.
	if runs, _ := r.e.Services.Runs.ListRuns(bg, core.DefaultTenant, core.RunFilter{}); len(runs) != 0 {
		t.Errorf("a test left %d run(s) in the real store", len(runs))
	}
	if st, _ := r.e.Services.State.Snapshot(bg, core.DefaultTenant, "dependency-watch"); len(st) != 0 {
		t.Errorf("a test committed state: %v", st)
	}
	if outs, _ := r.e.Services.Outputs.ListOutputs(bg, core.DefaultTenant, core.OutputFilter{}); len(outs) != 0 {
		t.Errorf("a test published %d output(s)", len(outs))
	}

	if err := r.activate("dependency-watch"); err != nil {
		t.Fatalf("a passing test of this version: %v", err)
	}
}

func TestAReplayWithoutARecordingFails(t *testing.T) {
	r := newRig(t)
	r.apply(t, "dependency-watch", "vcs.read")
	res := r.test(t, "dependency-watch", flowtest.ModeReplay)
	if res.Passed || !slices.Contains(problemKinds(res), flowtest.ProblemCassette) || r.live.count() != 0 {
		t.Fatalf("result %+v, live calls %d", res, r.live.count())
	}
	err := r.activate("dependency-watch")
	if !core.IsCode(err, core.CodeFlowNotTested) || !strings.Contains(err.Error(), "failed") {
		t.Errorf("activation after a failing test: %v", err)
	}
}

func TestAnEditAfterTheTestIsNotTheTestedVersion(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	if res := r.test(t, "dependency-watch", flowtest.ModeRecord); !res.Passed {
		t.Fatalf("%+v", res)
	}
	// A node spec of the bundle changes; the flow document does not.
	node := filepath.Join(v.Bundle, "nodes", "gh-releases.yaml")
	b, _ := os.ReadFile(node)                                                     //nolint:gosec // a temp copy
	if err := os.WriteFile(node, append(b, "# edited\n"...), 0o600); err != nil { //nolint:gosec // a temp copy
		t.Fatal(err)
	}
	err := r.activate("dependency-watch")
	if !core.IsCode(err, core.CodeFlowNotTested) || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("activation after the bundle changed: %v", err)
	}
	if res := r.test(t, "dependency-watch", flowtest.ModeRecord); !res.Passed {
		t.Fatalf("%+v", res)
	}
	if err := r.activate("dependency-watch"); err != nil {
		t.Errorf("after testing the edited bundle: %v", err)
	}
}

func TestAnotherVersionIsNotCoveredByTheTestOfThisOne(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	if res := r.test(t, "dependency-watch", flowtest.ModeRecord); !res.Passed {
		t.Fatalf("%+v", res)
	}
	flow := filepath.Join(v.Bundle, "flow.yaml")
	b, _ := os.ReadFile(flow)                                                                                                    //nolint:gosec // a temp copy
	if err := os.WriteFile(flow, []byte(strings.Replace(string(b), "version: 0.1.0", "version: 0.2.0", 1)), 0o600); err != nil { //nolint:gosec // a temp copy
		t.Fatal(err)
	}
	v2 := r.applyBundle(t, v.Bundle)
	if v2.Hash == v.Hash {
		t.Fatal("same version")
	}
	if err := r.activate("dependency-watch"); !core.IsCode(err, core.CodeFlowNotTested) {
		t.Errorf("the new version was not tested: %v", err)
	}
	// The old version names the same bundle directory, which now holds the new
	// document: what was tested is gone, so it is not the tested version either.
	if _, err := r.e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", v.Hash, core.FlowActive); !core.IsCode(err, core.CodeFlowNotTested) {
		t.Errorf("the old version, whose bundle was rewritten: %v", err)
	}
	if res := r.test(t, "dependency-watch", flowtest.ModeRecord); !res.Passed || res.Hash != v2.Hash {
		t.Fatalf("%+v", res)
	}
	if err := r.activate("dependency-watch"); err != nil {
		t.Errorf("the new version after its own test: %v", err)
	}
}

func TestExpectBlocksFailTheTest(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	tests := filepath.Join(v.Bundle, "tests")
	if err := os.MkdirAll(tests, 0o750); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(tests, "expect.yaml")
	if err := os.WriteFile(doc, []byte("expect:\n  - step: newadv\n    output: { items: [{ ghsa_id: GHSA-nope }] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v = r.applyBundle(t, v.Bundle) // same document: same version, new tests/
	res := r.test(t, "dependency-watch", flowtest.ModeRecord)
	if res.Passed || res.Expected != 1 || res.Held != 0 || !slices.Equal(problemKinds(res), []string{flowtest.ProblemExpect}) {
		t.Fatalf("%+v", res)
	}
	if err := os.WriteFile(doc, []byte("expect:\n  - step: findings\n    status: succeeded\n  - step: newadv\n    output: { items: [{ severity: medium }, { severity: medium }] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res = r.test(t, "dependency-watch", flowtest.ModeReplay); !res.Passed || res.Expected != 2 || res.Held != 2 {
		t.Fatalf("%+v", res)
	}
}

// An approval is auto-approved, so the write it guards is planned for every
// item; the write never reaches the real world.
func TestWritesAreRecordedAsPlansAndNeverRun(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "inbox-triage", "mail.read", "mail.write")
	rec := r.test(t, "inbox-triage", flowtest.ModeRecord)
	if !rec.Passed {
		t.Fatalf("%+v", rec)
	}
	if r.live.called("apply") || !r.live.called("inbox list") {
		t.Errorf("live calls = %v", r.live.calls)
	}
	var planned []string
	for _, s := range rec.Steps {
		if s.Planned && s.Index != nil {
			planned = append(planned, s.ID)
		}
	}
	if len(planned) != 2 {
		t.Fatalf("one plan per approved message, got %v in %+v", planned, rec.Steps)
	}
	plans, err := os.ReadFile(filepath.Join(v.Bundle, "record", "plans.json")) //nolint:gosec // a temp copy
	if err != nil || !strings.Contains(string(plans), "inbox/apply@1") || !strings.Contains(string(plans), "m1") {
		t.Errorf("plans.json = %s, %v", plans, err)
	}
	if res := r.test(t, "inbox-triage", flowtest.ModeReplay); !res.Passed {
		t.Fatalf("replay: %+v", res)
	}
	if err := r.activate("inbox-triage"); err != nil {
		t.Errorf("activation of the tested flow: %v", err)
	}
}

const checkedFlow = `flow: checked
version: 0.1.0
on:
  - manual: {}
tasks: none
steps:
  - id: dd
    uses: flo/dedupe@1
    with: { items: [1, 2], key: "{{ string(item) }}" }
    check: { contract: dep-judge-v1, eva_url: "http://eva.test" }
`

func (r *rig) checked(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "checked")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flow.yaml"), []byte(checkedFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	r.applyBundle(t, dir)
}

func TestChecksAreRecordedAndMustPass(t *testing.T) {
	r := newRig(t)
	r.checked(t)
	if res := r.test(t, "checked", flowtest.ModeRecord); !res.Passed || r.eva.calls != 1 {
		t.Fatalf("a passing check: %+v, eva calls %d", res, r.eva.calls)
	}
	if res := r.test(t, "checked", flowtest.ModeReplay); !res.Passed || r.eva.calls != 1 || res.Replayed != 1 {
		t.Fatalf("replay: %+v, eva calls %d", res, r.eva.calls)
	}

	r.eva.status = 422
	res := r.test(t, "checked", flowtest.ModeRecord)
	if res.Passed || !slices.Equal(problemKinds(res), []string{flowtest.ProblemCheck}) || res.Problems[0].Code != string(core.CodeCheckFailed) {
		t.Fatalf("a failing check: %+v", res)
	}
	if res = r.test(t, "checked", flowtest.ModeReplay); res.Passed {
		t.Errorf("the recorded 422 must fail the replay: %+v", res)
	}
}

func TestEvaUnreachableFailsTheTest(t *testing.T) {
	r := newRig(t)
	r.checked(t)
	r.eva.status = 0
	res := r.test(t, "checked", flowtest.ModeRecord)
	if res.Passed || !slices.Contains(problemKinds(res), flowtest.ProblemCheck) {
		t.Fatalf("%+v", res)
	}
	// Nothing recorded for eva: replaying finds no cassette either.
	r2 := newRig(t)
	r2.checked(t)
	res = r2.test(t, "checked", flowtest.ModeReplay)
	if res.Passed || !slices.Contains(problemKinds(res), flowtest.ProblemCassette) {
		t.Fatalf("%+v", res)
	}
}

func TestTestFlowRefusals(t *testing.T) {
	r := newRig(t)
	if _, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "nope", Caller: "jad"}); !core.IsCode(err, core.CodeFlowNotFound) {
		t.Errorf("no such flow: %v", err)
	}
	r.apply(t, "dependency-watch", "vcs.read")
	if _, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "dependency-watch", Caller: "noor"}); !core.IsCode(err, core.CodeGrantDenied) {
		t.Errorf("a caller who may not use run_as: %v", err)
	}
	if _, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "dependency-watch", Caller: "jad", Mode: "live"}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("a mode that is neither: %v", err)
	}
	if _, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "dependency-watch", Caller: "jad", Vars: map[string]string{"nope": "1"}}); !core.IsCode(err, core.CodeInvalidVar) {
		t.Errorf("an undeclared var: %v", err)
	}

	// A single file has no record/ to keep a recording in.
	file := filepath.Join(t.TempDir(), "solo.yaml")
	if err := os.WriteFile(file, []byte(strings.Replace(checkedFlow, "flow: checked", "flow: solo", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	r.applyBundle(t, file)
	if _, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "solo", Caller: "jad", Mode: flowtest.ModeRecord}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("recording a single-file flow: %v", err)
	}
	// A single file replays against no cassettes: its eva check has none.
	if res := r.test(t, "solo", flowtest.ModeReplay); res.Passed || !slices.Contains(problemKinds(res), flowtest.ProblemCassette) {
		t.Errorf("replay of a single file with a check: %+v", res)
	}
}

func TestTriggerPayloadIsKeptByARecording(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(t.TempDir(), "ev")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	doc := `flow: ev
version: 0.1.0
on:
  - manual: {}
tasks: none
steps:
  - id: dd
    uses: flo/dedupe@1
    with: { items: "{{ trigger.event.items }}", key: "{{ string(item) }}" }
`
	if err := os.WriteFile(filepath.Join(dir, "flow.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	r.applyBundle(t, dir)
	payload := map[string]any{"event": map[string]any{"items": []any{"a", "b"}}}
	rec, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "ev", Caller: "jad", Mode: flowtest.ModeRecord, Trigger: payload})
	if err != nil || !rec.Passed || !slices.Contains(rec.Recorded, "record/trigger.json") {
		t.Fatalf("%+v %v", rec, err)
	}
	// Replaying without a payload uses the recorded one.
	rep := r.test(t, "ev", flowtest.ModeReplay)
	if !rep.Passed {
		t.Fatalf("%+v", rep)
	}
	got, _ := rep.Steps[0].Output.(map[string]any)
	if items, _ := got["items"].([]any); len(items) != 2 {
		t.Errorf("the recorded trigger was not used: %+v", rep.Steps[0])
	}
}
