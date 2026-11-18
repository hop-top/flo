//go:build !windows

package serve_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/runrec"
)

// tree lists every file of dir with its content.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(p) //nolint:gosec // a temp bundle
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *rig) fromRun(flow string, run core.RunID, mut func(*serve.FromRunRequest)) (serve.FromRunResult, error) {
	req := serve.FromRunRequest{Flow: flow, Run: run, Caller: "jad"}
	if mut != nil {
		mut(&req)
	}
	return r.e.FromRun(bg, core.DefaultTenant, req)
}

func wantUsage(t *testing.T, err error, sub string) {
	t.Helper()
	if !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), sub) {
		t.Errorf("want a usage error with %q, got %v", sub, err)
	}
}

func TestFromRunMakesATestOfARecordedRun(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	run := r.startRecorded(t, "dependency-watch", v.Hash)
	live := r.live.count()
	before, _ := flowtest.Digest(v.Bundle)

	res, err := r.fromRun("dependency-watch", run.ID, func(q *serve.FromRunRequest) { q.WriteExpect = true })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Test.Passed || res.Test.Mode != flowtest.ModeReplay || res.Test.Calls != 2 || res.Test.Replayed != 2 {
		t.Fatalf("test = %+v", res.Test)
	}
	if res.Test.StateSource != flowtest.StateRecorded {
		t.Errorf("the replay did not start from the copied state: %s", res.Test.StateSource)
	}
	if r.live.count() != live {
		t.Errorf("the replay reached the real world: %d -> %d live calls", live, r.live.count())
	}
	for _, f := range res.Files {
		if f.Action != "create" {
			t.Errorf("%s: %s", f.Path, f.Action)
		}
		if _, err := os.Stat(filepath.Join(v.Bundle, filepath.FromSlash(f.Path))); err != nil {
			t.Errorf("planned %s, not written: %v", f.Path, err)
		}
	}
	for _, want := range []string{"record/trigger.json", "record/state.json", "tests/from-" + string(run.ID) + ".yaml"} {
		if !slices.ContainsFunc(res.Files, func(f serve.FromRunFile) bool { return f.Path == want }) {
			t.Errorf("files lack %s: %+v", want, res.Files)
		}
	}
	cass, _ := os.ReadDir(filepath.Join(v.Bundle, "record", "cassettes"))
	if len(cass) == 0 {
		t.Error("no cassettes were copied")
	}
	// The digest moved, and the stored result is bound to the new one.
	after, _ := flowtest.Digest(v.Bundle)
	if after == before || res.Test.Digest != after {
		t.Errorf("digest %s -> %s, result bound to %s", before, after, res.Test.Digest)
	}
	// What the run started from is what the bundle's test starts from.
	var seed map[string]any
	b, _ := os.ReadFile(filepath.Join(v.Bundle, "record", "state.json")) //nolint:gosec // a temp bundle
	if seed, err = flowtest.DecodeSeed(b, "state.json"); err != nil || len(seed) != 2 {
		t.Errorf("state.json = %v, %v", seed, err)
	}
	// A plain replay of the bundle holds, and so do the written expectations.
	again := r.test(t, "dependency-watch", flowtest.ModeReplay)
	if !again.Passed || again.Expected != 5 || again.Held != 5 {
		t.Errorf("replay of the bundle: passed=%v expected=%d held=%d problems=%v", again.Passed, again.Expected, again.Held, again.Problems)
	}
}

func TestFromRunWritesAnExpectSkeletonOfTheRunsSteps(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	run := r.startRecorded(t, "dependency-watch", v.Hash)
	steps, _ := r.e.Services.Runs.ListSteps(bg, core.DefaultTenant, run.ID)

	// Without --write-expect there is none.
	if _, err := r.fromRun("dependency-watch", run.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(v.Bundle, "tests")); err == nil {
		t.Error("a tests/ document was written without being asked for")
	}
	if _, err := r.fromRun("dependency-watch", run.ID, func(q *serve.FromRunRequest) { q.WriteExpect = true }); err != nil {
		t.Fatal(err)
	}
	cases, err := flowtest.LoadCases(v.Bundle)
	if err != nil || len(cases) != 1 || cases[0].State != nil {
		t.Fatalf("cases = %+v, %v", cases, err)
	}
	got := map[string]string{}
	for _, ex := range cases[0].Expects {
		got[ex.Step] = ex.Status
		if ex.HasOutput {
			t.Errorf("a skeleton pins no output: %+v", ex)
		}
	}
	for _, s := range steps {
		if s.Element == nil && got[s.StepID] != string(s.Status) {
			t.Errorf("step %s: expect %q, run %s", s.StepID, got[s.StepID], s.Status)
		}
	}
	if len(got) == 0 {
		t.Error("no expectations")
	}
	// Run again: the same file is overwritten, not duplicated.
	res, err := r.fromRun("dependency-watch", run.ID, func(q *serve.FromRunRequest) { q.WriteExpect = true })
	if err != nil || !res.Test.Passed {
		t.Fatalf("a second run: %v %+v", err, res.Test.Problems)
	}
	if again, _ := flowtest.LoadCases(v.Bundle); len(again) != 1 {
		t.Errorf("tests/ holds %d documents", len(again))
	}
	if !slices.ContainsFunc(res.Files, func(f serve.FromRunFile) bool { return f.Action == "overwrite" }) {
		t.Errorf("the second run overwrites: %+v", res.Files)
	}
}

func TestFromRunDryRunWritesNothing(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	run := r.startRecorded(t, "dependency-watch", v.Hash)
	snap := tree(t, v.Bundle)
	live := r.live.count()

	plan, err := r.e.PlanFromRun(bg, core.DefaultTenant, serve.FromRunRequest{Flow: "dependency-watch", Run: run.ID, Caller: "jad", WriteExpect: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Cassettes != 4 || plan.Hash != v.Hash || len(plan.Files) != 7 || plan.Bundle != v.Bundle {
		t.Errorf("plan = %+v", plan)
	}
	if after := tree(t, v.Bundle); len(after) != len(snap) {
		t.Errorf("a dry run wrote: %d -> %d files", len(snap), len(after))
	}
	if r.live.count() != live {
		t.Error("a dry run made a call")
	}
	if _, err := os.Stat(filepath.Join(v.Bundle, "record")); err == nil {
		t.Error("a dry run made record/")
	}
	// A plan refuses what the run refuses.
	if _, err := r.e.PlanFromRun(bg, core.DefaultTenant, serve.FromRunRequest{Flow: "dependency-watch", Run: run.ID, Caller: "noor"}); !core.IsCode(err, core.CodeGrantDenied) {
		t.Errorf("a caller who may not use run_as: %v", err)
	}
}

func TestFromRunRefusesWhatIsNotWhole(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	snap := tree(t, v.Bundle)

	plain, err := r.e.Services.Starter.Start(bg, core.StartRequest{Tenant: core.DefaultTenant, Flow: "dependency-watch", Version: v.Hash, StartedBy: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.Advance(bg, core.DefaultTenant, plain.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fromRun("dependency-watch", plain.Run.ID, nil); !core.IsCode(err, core.CodeNotFound) || !strings.Contains(err.Error(), "no recording") {
		t.Errorf("a run without a recording: %v", err)
	}
	if _, err := r.fromRun("dependency-watch", "run_01NOSUCHRUN0000000000000", nil); err == nil {
		t.Error("a run that does not exist")
	}

	run := r.startRecorded(t, "dependency-watch", v.Hash)
	meta, _, _ := r.e.Recordings.Load(core.DefaultTenant, run.ID)
	edit := func(f func(*runrec.Meta)) {
		m := meta
		f(&m)
		if err := r.e.Recordings.Save(core.DefaultTenant, m); err != nil {
			t.Fatal(err)
		}
	}
	edit(func(m *runrec.Meta) { m.Status = runrec.StatusRecording })
	if _, err := r.fromRun("dependency-watch", run.ID, nil); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "not finished") {
		t.Errorf("an unfinished recording: %v", err)
	}
	edit(func(m *runrec.Meta) { m.Partial = []string{"a call"} })
	if _, err := r.fromRun("dependency-watch", run.ID, nil); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "partial") {
		t.Errorf("a partial recording: %v", err)
	}
	edit(func(m *runrec.Meta) { m.RunStatus = "failed" })
	_, err = r.fromRun("dependency-watch", run.ID, nil)
	wantUsage(t, err, "could not pass")
	edit(func(*runrec.Meta) {})
	cass, _ := r.e.Recordings.Cassettes(core.DefaultTenant, run.ID)
	_ = os.RemoveAll(cass)
	if _, err := r.fromRun("dependency-watch", run.ID, nil); !core.IsCode(err, core.CodeNotFound) || !strings.Contains(err.Error(), "cassettes") {
		t.Errorf("missing cassettes: %v", err)
	}

	other := r.applyRenamed(t, "dw-other", "")
	_ = other
	good := r.startRecorded(t, "dependency-watch", v.Hash)
	_, err = r.fromRun("dw-other", good.ID, nil)
	wantUsage(t, err, "not dw-other")

	if after := tree(t, v.Bundle); len(after) != len(snap) {
		t.Errorf("a refusal wrote to the bundle: %d -> %d files", len(snap), len(after))
	}
	if r.live.count() != 6 { // three recorded runs of two calls: none from a refusal
		t.Errorf("a refusal reached the real world: %d live calls", r.live.count())
	}
}

func TestFromRunRefusesAnotherVersionOfTheFlow(t *testing.T) {
	r := newRig(t)
	v1 := r.apply(t, "dependency-watch", "vcs.read")
	run := r.startRecorded(t, "dependency-watch", v1.Hash)

	// The flow changes in place and is applied again: the bundle moves on.
	p := filepath.Join(v1.Bundle, "flow.yaml")
	b, _ := os.ReadFile(p) //nolint:gosec // a temp bundle
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "New advisories and releases", "Fresh advisories and releases", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := serve.ReadSource(v1.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil || a.Version.Hash == v1.Hash {
		t.Fatalf("apply v2: %v", err)
	}
	snap := tree(t, v1.Bundle)

	_, err = r.fromRun("dependency-watch", run.ID, nil)
	wantUsage(t, err, "the version to write is")
	var ce *core.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Suggestion, "--version "+v1.Hash) {
		t.Errorf("the refusal says which version fits: %v", err)
	}
	if len(tree(t, v1.Bundle)) != len(snap) {
		t.Error("the refusal wrote to the bundle")
	}

	// --version naming the recorded version matches it.
	res, err := r.fromRun("dependency-watch", run.ID, func(q *serve.FromRunRequest) { q.Ref = v1.Hash })
	if err != nil || !res.Test.Passed || res.Test.Hash != v1.Hash {
		t.Fatalf("--version %s: %v %+v", v1.Hash, err, res.Test.Problems)
	}
	if res, err = r.fromRun("dependency-watch", run.ID, func(q *serve.FromRunRequest) { q.Ref = v1.Version }); err == nil && res.Test.Hash == a.Version.Hash {
		t.Error("a document version that names the newer hash matched")
	}
}

// ---- local-only and redacted ----

func (r *lookupRig) fromRun(run core.RunID, mut func(*serve.FromRunRequest)) (serve.FromRunResult, error) {
	req := serve.FromRunRequest{Flow: "lookup", Run: run, Caller: "jad"}
	if mut != nil {
		mut(&req)
	}
	return r.e.FromRun(bg, core.DefaultTenant, req)
}

type lookupRig struct {
	e      *serve.Engine
	live   *echoExec
	bundle string
	run    core.Run
}

// recordLookup records one run of the local-only lookup flow, which has a
// secret trigger field, var and state key.
func recordLookup(t *testing.T) *lookupRig {
	t.Helper()
	live := &echoExec{}
	e, err := serve.Open(bg, serve.Config{Root: t.TempDir(), Owner: "jad", NoSync: true, Exec: live})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	bundle := filepath.Join(t.TempDir(), "lookup")
	for name, body := range map[string]string{"flow.yaml": lookupFlow, "nodes/lookup.yaml": lookupNode} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(bundle, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := serve.ReadSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Services.Starter.Start(bg, core.StartRequest{
		Tenant: core.DefaultTenant, Flow: "lookup", Version: a.Version.Hash, StartedBy: "jad", Record: true,
		Vars:    map[string]any{"query": "alpha", "passphrase": varSecret},
		Trigger: core.TriggerInfo{Payload: map[string]any{"token": secretToken, "note": "kept"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(bg, core.DefaultTenant, res.Run.ID); err != nil {
		t.Fatal(err)
	}
	run, _ := e.Services.Runs.GetRun(bg, core.DefaultTenant, res.Run.ID)
	if run.Status != core.RunSucceeded {
		t.Fatalf("the run: %s %+v", run.Status, run.Failure)
	}
	return &lookupRig{e: e, live: live, bundle: bundle, run: run}
}

func TestFromRunKeepsALocalOnlyRecordingOutOfTheBundleUnlessAllowed(t *testing.T) {
	r := recordLookup(t)
	snap := tree(t, r.bundle)

	_, err := r.fromRun(r.run.ID, nil)
	wantUsage(t, err, "local-only")
	if !strings.Contains(err.Error(), "committed") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if _, err := r.e.PlanFromRun(bg, core.DefaultTenant, serve.FromRunRequest{Flow: "lookup", Run: r.run.ID, Caller: "jad"}); err == nil {
		t.Error("a dry run of a local-only recording is not refused")
	}
	if len(tree(t, r.bundle)) != len(snap) {
		t.Error("the refusal wrote to the bundle")
	}

	// Allowed, the redacted inputs still have to be supplied again.
	_, err = r.fromRun(r.run.ID, func(q *serve.FromRunRequest) { q.AllowLocalData = true })
	wantUsage(t, err, "recorded input redacted: ")
	if len(tree(t, r.bundle)) != len(snap) {
		t.Error("a refused input wrote to the bundle")
	}
}

func TestFromRunNeverCopiesRedactedInputsRaw(t *testing.T) {
	r := recordLookup(t)
	for _, tc := range []struct {
		name string
		mut  func(*serve.FromRunRequest)
	}{
		{"nothing supplied", func(q *serve.FromRunRequest) {}},
		{"trigger only", func(q *serve.FromRunRequest) { q.Trigger = map[string]any{"token": secretToken} }},
		{"trigger and vars", func(q *serve.FromRunRequest) {
			q.Trigger, q.Vars = map[string]any{"token": secretToken}, map[string]string{"passphrase": "x"}
		}},
	} {
		_, err := r.fromRun(r.run.ID, func(q *serve.FromRunRequest) { q.AllowLocalData = true; tc.mut(q) })
		if !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "recorded input redacted: ") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	res, err := r.fromRun(r.run.ID, func(q *serve.FromRunRequest) {
		q.AllowLocalData = true
		q.Trigger = map[string]any{"token": secretToken}
		q.Vars = map[string]string{"query": "alpha", "passphrase": "x"}
		q.State = map[string]any{"cursor": "", "password": "y"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Test.Passed || res.Test.Calls != 1 || res.Test.Replayed != 1 || len(r.live.stdins) != 1 {
		t.Fatalf("test = %+v, live calls %d", res.Test, len(r.live.stdins))
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "local-only") }) {
		t.Errorf("no warning that local data was copied: %v", res.Warnings)
	}
	// What was supplied again served this replay; the bundle keeps none of it.
	for p, body := range tree(t, r.bundle) {
		if strings.Contains(body, secretToken) {
			t.Errorf("%s holds the supplied token", p)
		}
	}
	// Nothing of the run's own recording that was scrubbed shows up in the bundle.
	for p, body := range tree(t, r.bundle) {
		for _, secret := range []string{varSecret, secretOut, stateSecret} {
			if strings.Contains(body, secret) && !strings.HasSuffix(p, "flow.yaml") {
				t.Errorf("%s holds %q", p, secret)
			}
		}
	}
}
