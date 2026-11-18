//go:build !windows

package serve_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

const firstAdv = "GHSA-5cv4-jp36-h3mw" // one of the two advisories of the fixture

func items(t *testing.T, res flowtest.Result, step string) int {
	t.Helper()
	for _, s := range res.Steps {
		if s.ID == step && s.Index == nil {
			m, _ := s.Output.(map[string]any)
			l, _ := m["items"].([]any)
			return len(l)
		}
	}
	t.Fatalf("no step %s in %+v", step, res.Steps)
	return 0
}

func (r *rig) testWith(t *testing.T, req serve.TestRequest) (flowtest.Result, error) {
	t.Helper()
	req.Flow, req.Caller = "dependency-watch", "jad"
	return r.e.TestFlow(bg, core.DefaultTenant, req)
}

func (r *rig) mustTest(t *testing.T, req serve.TestRequest) flowtest.Result {
	t.Helper()
	res, err := r.testWith(t, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func recorded(t *testing.T) (*rig, core.FlowVersion) {
	t.Helper()
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	if res := r.test(t, "dependency-watch", flowtest.ModeRecord); !res.Passed {
		t.Fatalf("%+v", res)
	}
	return r, v
}

func TestStateSources(t *testing.T) {
	r, _ := recorded(t)
	dir := t.TempDir()
	if res := r.mustTest(t, serve.TestRequest{}); res.StateSource != flowtest.StateRecorded || items(t, res, "newadv") != 2 {
		t.Fatalf("recorded defaults: %s %+v", res.StateSource, res.Steps[1])
	}
	if res := r.mustTest(t, serve.TestRequest{State: "default"}); res.StateSource != flowtest.StateDefault || items(t, res, "newadv") != 2 {
		t.Fatalf("default: %s", res.StateSource)
	}
	for name, body := range map[string]string{
		"seed.json": `{"seen_adv": ["` + firstAdv + `"]}`,
		"seed.yaml": "seen_adv: [" + firstAdv + "]\n",
	} {
		p := writeFile(t, filepath.Join(dir, name), body)
		res := r.mustTest(t, serve.TestRequest{State: p})
		if res.StateSource != flowtest.StateFile+p || !res.Passed || items(t, res, "newadv") != 1 {
			t.Errorf("%s: %s, newadv items %d", name, res.StateSource, items(t, res, "newadv"))
		}
	}
	// refusals: a key the flow does not declare, a value of another kind
	for name, body := range map[string]string{
		"unknown": `{"nope": 1}`, "type": `{"seen_adv": "x"}`, "not a map": `[1]`,
	} {
		p := writeFile(t, filepath.Join(dir, "bad.json"), body)
		if _, err := r.testWith(t, serve.TestRequest{State: p}); !core.IsCode(err, core.CodeUsage) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.testWith(t, serve.TestRequest{State: filepath.Join(dir, "missing.json")}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("a missing seed file: %v", err)
	}
}

// liveRun makes the real engine run the active flow once, so it has committed
// state.
func liveRun(t *testing.T, r *rig) {
	t.Helper()
	if err := r.activate("dependency-watch"); err != nil {
		t.Fatal(err)
	}
	res, err := r.e.Services.Starter.Start(bg, core.StartRequest{Tenant: core.DefaultTenant, Flow: "dependency-watch", StartedBy: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.Executor.Advance(bg, core.DefaultTenant, res.Run.ID); err != nil {
		t.Fatal(err)
	}
}

func liveState(t *testing.T, r *rig) map[string]any {
	t.Helper()
	s, err := r.e.Services.State.Snapshot(bg, core.DefaultTenant, "dependency-watch")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCurrentStateIsACopyAndNeverWrittenBack(t *testing.T) {
	r, _ := recorded(t)
	liveRun(t, r)
	before := liveState(t, r)
	if len(before["seen_adv"].([]any)) != 2 {
		t.Fatalf("the live run committed no state: %v", before)
	}
	res := r.mustTest(t, serve.TestRequest{State: "current"})
	if res.StateSource != flowtest.StateCurrent || !res.Passed || items(t, res, "newadv") != 0 {
		t.Fatalf("current: %s, newadv items %d", res.StateSource, items(t, res, "newadv"))
	}
	// a run on defaults commits seen keys in the sandbox: none of it is live
	res = r.mustTest(t, serve.TestRequest{State: "default"})
	if items(t, res, "newadv") != 2 || !reflect.DeepEqual(liveState(t, r), before) {
		t.Fatalf("the live state moved: %v -> %v", before, liveState(t, r))
	}
	r.mustTest(t, serve.TestRequest{State: "current"})
	if !reflect.DeepEqual(liveState(t, r), before) {
		t.Fatalf("the live state moved after a current test: %v -> %v", before, liveState(t, r))
	}
}

func TestCaseStateWinsOverTheFlag(t *testing.T) {
	r, v := recorded(t)
	dir := t.TempDir()
	flagSeed := writeFile(t, filepath.Join(dir, "seed.json"), `{"seen_adv": ["`+firstAdv+`"]}`)
	writeFile(t, filepath.Join(v.Bundle, "tests", "fresh.yaml"), "state: { seen_adv: [] }\nexpect:\n  - step: newadv\n    output: { keys: [GHSA-5cv4-jp36-h3mw, GHSA-vvgc-356p-c3xw] }\n")
	writeFile(t, filepath.Join(v.Bundle, "tests", "base.yaml"), "expect:\n  - step: newadv\n    output: { keys: [GHSA-vvgc-356p-c3xw] }\n")
	res := r.mustTest(t, serve.TestRequest{State: flagSeed})
	if !res.Passed || res.Expected != 2 || res.Held != 2 {
		t.Fatalf("%+v", res.Problems)
	}
	if len(res.Cases) != 1 || res.Cases[0].Name != "fresh.yaml" || res.Cases[0].StateSource != flowtest.StateCase+"fresh.yaml" || !res.Cases[0].Passed {
		t.Fatalf("cases = %+v", res.Cases)
	}
	if res.StateSource != flowtest.StateFile+flagSeed {
		t.Errorf("the base run keeps the flag's seed: %s", res.StateSource)
	}
	// a case seed the flow does not declare is refused
	writeFile(t, filepath.Join(v.Bundle, "tests", "fresh.yaml"), "state: { nope: 1 }\n")
	if _, err := r.testWith(t, serve.TestRequest{}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("an undeclared case key: %v", err)
	}
}

func TestRecordKeepsTheSeedAndReplayReusesIt(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	seed := writeFile(t, filepath.Join(t.TempDir(), "seed.json"), `{"seen_adv": ["`+firstAdv+`"]}`)
	rec := r.mustTest(t, serve.TestRequest{Mode: flowtest.ModeRecord, State: seed})
	if !rec.Passed || rec.StateSource != flowtest.StateFile+seed || !contains(rec.Recorded, "record/state.json") {
		t.Fatalf("%+v", rec)
	}
	b, err := os.ReadFile(filepath.Join(v.Bundle, "record", "state.json")) //nolint:gosec // a temp copy
	if err != nil || !strings.Contains(string(b), firstAdv) || !strings.Contains(string(b), "seen_rel") {
		t.Fatalf("state.json = %s, %v", b, err)
	}
	// replay: the recorded seed, not the defaults; an explicit flag wins
	res := r.mustTest(t, serve.TestRequest{})
	if res.StateSource != flowtest.StateRecorded || !res.Passed || items(t, res, "newadv") != 1 {
		t.Fatalf("replay: %s, newadv items %d", res.StateSource, items(t, res, "newadv"))
	}
	res = r.mustTest(t, serve.TestRequest{State: "default"})
	if res.StateSource != flowtest.StateDefault || items(t, res, "newadv") != 2 {
		t.Fatalf("explicit default: %s, newadv items %d", res.StateSource, items(t, res, "newadv"))
	}
	// state.json is part of the bundle the test binds to
	if err := r.activate("dependency-watch"); err == nil {
		// the last test passed (default seed): activation is allowed; now edit the seed
		writeFile(t, filepath.Join(v.Bundle, "record", "state.json"), `{"seen_adv": [], "seen_rel": []}`)
		if _, err := r.e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowPaused); err != nil {
			t.Fatal(err)
		}
		if err := r.activate("dependency-watch"); !core.IsCode(err, core.CodeFlowNotTested) || !strings.Contains(err.Error(), "changed since") {
			t.Errorf("an edited seed: %v", err)
		}
	} else {
		t.Fatalf("activation: %v", err)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestPlanTestWritesAndRunsNothing(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "inbox-triage", "mail.read", "mail.write")
	snapshot := func() string {
		var names []string
		_ = filepath.WalkDir(v.Bundle, func(p string, _ os.DirEntry, _ error) error { names = append(names, p); return nil })
		return strings.Join(names, "\n")
	}
	before := snapshot()
	plan, err := r.e.PlanTest(bg, core.DefaultTenant, serve.TestRequest{Flow: "inbox-triage", Caller: "jad", Mode: flowtest.ModeRecord})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot() != before || r.live.count() != 0 {
		t.Fatalf("a plan wrote or ran something: live calls %d", r.live.count())
	}
	if plan.Hash != v.Hash || plan.Mode != "record" || plan.StateSource != flowtest.StateDefault || plan.CassetteAction != "record" || len(plan.Writes) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	steps := []string{plan.Writes[0].Step, plan.Writes[1].Step}
	if !contains(steps, "review") || !contains(steps, "apply") {
		t.Errorf("planned writes = %+v", plan.Writes)
	}
	if _, err := r.e.PlanTest(bg, core.DefaultTenant, serve.TestRequest{Flow: "inbox-triage", Caller: "noor"}); !core.IsCode(err, core.CodeGrantDenied) {
		t.Errorf("a plan refuses what the test refuses: %v", err)
	}
}
