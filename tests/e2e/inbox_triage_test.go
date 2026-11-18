//go:build !windows

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// The inbox-triage flow of flows/ over the inbox pack of packs/: the wrappers
// run for real, the inbox script is a fake (testdata/inbox/shims/inbox) over
// fixtures, aps and tlc are the harness shims. The real script is never run:
// nothing here names a real inbox home or run directory.
//
// The fake's default scenario: four messages. <m1> is automatic (read and
// archive are trusted), <m2> and <m3> are pending (label:Finance and
// reply:draft are still in training), <m5> is pending because a rule always
// asks, although its archive is trusted.

// triageEnv is an env with the inbox pack installed, its wrappers and the fake
// inbox on the PATH of the fakes, and the grants of the flow's profile.
func triageEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	needJQ(t)
	e := newEnv(t, extra...)
	e.installRepoPack("inbox")
	for _, dir := range []string{filepath.Join(repoRoot, "packs", "inbox", "wrappers"), testdata("inbox", "shims")} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range ents {
			b, err := os.ReadFile(filepath.Join(dir, f.Name())) //nolint:gosec // a file of this repository
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.shims, f.Name()), b, 0o755); err != nil { //nolint:gosec // an executable fake
				t.Fatal(err)
			}
		}
	}
	e.grantMail()
	return e
}

func (e *env) grantMail() {
	e.t.Helper()
	for _, c := range []string{"mail.read", "mail.write", "mail.send"} {
		e.ok("grant", "set", "automation", c, "--yes")
	}
}

// installRepoPack copies a pack of the repository's packs/ into the engine's
// packs directory.
func (e *env) installRepoPack(name string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.root(), "packs"), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if out, err := cpR(filepath.Join(repoRoot, "packs", name), filepath.Join(e.root(), "packs", name)); err != nil {
		e.t.Fatalf("install pack %s: %v\n%s", name, err, out)
	}
}

// triageBundle copies the shipped flow bundle, recording included.
func triageBundle(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "inbox-triage")
	if out, err := cpR(filepath.Join(repoRoot, "flows", "inbox-triage"), dst); err != nil {
		t.Fatalf("copy bundle: %v\n%s", err, out)
	}
	return dst
}

// triageActive applies the shipped bundle, replays its recording (the
// activation gate wants a passing test of that version) and activates it.
func (e *env) triageActive() {
	e.t.Helper()
	e.ok("flow", "apply", "-f", triageBundle(e.t), "--yes")
	if res, r := e.flowTest("inbox-triage"); r.Exit != 0 || !res.Passed {
		e.t.Fatalf("replay of the recording: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	// a replay starts no process: the scenario the test set up is still there
	e.ok("flow", "set", "inbox-triage", "--state", "active", "--yes")
}

// flag switches a scenario of the fake inbox (see its header) on or off.
func (e *env) inboxFlag(name string, on bool) {
	e.t.Helper()
	dir := filepath.Join(e.scratch, "inbox", "flags")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		e.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if !on {
		_ = os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// inboxLog is what the fake inbox was asked, one call per line.
func (e *env) inboxLog() []string {
	return readLines(e.t, filepath.Join(e.scratch, "inbox", "inbox.log"))
}

// inboxCalls are the calls of one subcommand.
func (e *env) inboxCalls(sub string) []string {
	var out []string
	for _, l := range e.inboxLog() {
		if l == sub || strings.HasPrefix(l, sub+" ") {
			out = append(out, l)
		}
	}
	return out
}

// applied is what the fake script executed on mail, in order.
func (e *env) applied() []string {
	return readLines(e.t, filepath.Join(e.scratch, "inbox", "applied.log"))
}

// trustOf reads the fake script's counters.
func (e *env) trustOf(key string) (streak, resets int) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "trust.json")) //nolint:gosec // the fake's state
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]struct {
		Streak int `json:"streak"`
		Resets int `json:"resets"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatal(err)
	}
	return m[key].Streak, m[key].Resets
}

// stepOf is the record of a step of a run, and its output as a map.
func (e *env) stepOf(run, step string) (core.StepRun, map[string]any) {
	e.t.Helper()
	recs, err := e.store().Runs.ListSteps(context.Background(), tenant, core.RunID(run))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range recs {
		if r.StepID == step && r.Element == nil {
			out, _ := r.Output.(map[string]any)
			return r, out
		}
	}
	e.t.Fatalf("run %s has no step %s", run, step)
	return core.StepRun{}, nil
}

// triageApproval is `flo approval show` of the triage approval.
type triageApproval struct {
	Status  string `json:"status"`
	Trust   string `json:"trust"`
	Waiting int    `json:"waiting"`
	Items   []struct {
		N       int      `json:"n"`
		ID      string   `json:"id"`
		Action  string   `json:"action"`
		Class   []string `json:"class"`
		Verdict string   `json:"verdict"`
		By      string   `json:"by"`
	} `json:"items"`
}

// pos is the position (the N of --item N=...) of the item for a message.
func (a triageApproval) pos(t *testing.T, id string) int {
	t.Helper()
	for _, it := range a.Items {
		if it.ID == id {
			return it.N
		}
	}
	t.Fatalf("approval has no item %s: %+v", id, a.Items)
	return 0
}

func (e *env) approvalOf(id string) triageApproval {
	e.t.Helper()
	var a triageApproval
	e.okJSON(&a, "approval", "show", id)
	return a
}

const (
	m1 = "<m1@example.org>" // automatic
	m2 = "<m2@example.org>" // pending: label:Finance, archive
	m3 = "<m3@example.org>" // pending: reply:draft
	m5 = "<m5@example.org>" // pending: held by a rule that always asks
)

// startTriage runs the flow and returns the parked run.
func (e *env) startTriage() (created, triageApproval) {
	e.t.Helper()
	var c created
	r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait")
	if r.Exit != core.ExitAwaiting || len(c.WaitingOn) != 1 {
		e.t.Fatalf("run create --wait: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
	}
	return c, e.approvalOf(c.WaitingOn[0])
}

// runShim runs one of the fakes or wrappers on the env's PATH, with stdin.
func (e *env) runShim(stdin, name string, args ...string) result {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(e.shims, name), args...) //nolint:gosec // a script of this repository
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	res := result{}
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatal(err)
		}
		res.Exit = ee.ExitCode()
	}
	res.Stdout, res.Stderr = out.String(), errOut.String()
	return res
}

// TestInboxPackDeclaresHonestEffects: what each node claims is what the
// script does, and the irreversible ones cannot run without an approval.
func TestInboxPackDeclaresHonestEffects(t *testing.T) {
	lp, err := node.LoadPack(os.DirFS(filepath.Join(repoRoot, "packs")), "inbox/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]*node.Spec{}
	for _, n := range lp.Nodes {
		byType[n.Node] = n
	}
	want := map[string]node.SideEffect{
		"inbox/fetch@1":  node.SideEffectWriteLocal,
		"inbox/decide@1": node.SideEffectWriteLocal,
		"inbox/plan@1":   node.SideEffectWriteLocal,
		"inbox/apply@1":  node.SideEffectIrreversible,
		"inbox/lanes@1":  node.SideEffectIrreversible,
	}
	for ref, se := range want {
		n := byType[ref]
		if n == nil {
			t.Fatalf("%s is not in the pack", ref)
		}
		if n.SideEffect != se {
			t.Errorf("%s side effect %s, want %s", ref, n.SideEffect, se)
		}
		if se == node.SideEffectIrreversible && n.Approval != "required" {
			t.Errorf("%s approval %q, want required", ref, n.Approval)
		}
		for _, a := range n.Argv {
			if strings.Contains(a, "/") && !strings.HasPrefix(a, "{{") {
				t.Errorf("%s names a path in its argv: %s", ref, a)
			}
		}
	}
	if len(lp.Trusts) != 1 || lp.Trusts[0].Trust != "inbox" {
		t.Errorf("trust providers: %+v", lp.Trusts)
	}
	// fetch never runs the lanes: they are lanes@1, which the flow does not use.
	if fetch := strings.Join(byType["inbox/fetch@1"].Argv, " "); strings.Contains(fetch, "lanes") {
		t.Errorf("fetch argv: %s", fetch)
	}
}

// TestInboxTriageReplaysOffline: the recording of the shipped bundle passes
// without the script, its wrappers or a process of any kind, and the version
// can be activated.
func TestInboxTriageReplaysOffline(t *testing.T) {
	e := newEnv(t)
	e.installRepoPack("inbox")
	e.grantMail()
	// the harness's toy inbox is the only inbox on the PATH: take it away too
	if err := os.Remove(filepath.Join(e.shims, "inbox")); err != nil {
		t.Fatal(err)
	}
	e.ok("flow", "apply", "-f", triageBundle(t), "--yes")

	res, r := e.flowTest("inbox-triage")
	if r.Exit != 0 || !res.Passed {
		t.Fatalf("replay: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	if res.Mode != "replay" || res.Calls != 3 || res.Replayed != 3 {
		t.Errorf("replay made %d calls, %d from cassettes (mode %s)", res.Calls, res.Replayed, res.Mode)
	}
	if res.Expected != 5 || res.Held != res.Expected {
		t.Errorf("expectations held %d of %d", res.Held, res.Expected)
	}
	planned := map[string]bool{}
	for _, s := range res.Steps {
		if s.Planned {
			planned[s.ID] = true
		}
	}
	if !planned["apply"] || !planned["approve"] || planned["fetch"] {
		t.Errorf("planned steps: %v", planned)
	}
	if exists(filepath.Join(e.scratch, "aps.log")) || exists(filepath.Join(e.scratch, "inbox")) {
		t.Error("the replay started a process")
	}
	e.ok("flow", "set", "inbox-triage", "--state", "active", "--yes")
	var trig []triggerRow
	e.okJSON(&trig, "trigger", "list", "--flow", "inbox-triage")
	i := slices.IndexFunc(trig, func(x triggerRow) bool { return x.Kind == "schedule" })
	if i < 0 || trig[i].State != "active" || trig[i].Next == "" {
		t.Errorf("the 15-minute schedule has a next fire: %+v", trig)
	}
}

// TestInboxTriageParksAndApplies: the whole story on the fake script. The run
// fetches and plans without touching mail, asks only about what is pending,
// and applies exactly what is approved, plus what trust approved.
func TestInboxTriageParksAndApplies(t *testing.T) {
	e := triageEnv(t)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()

	c, av := e.startTriage()
	// fetch, decide and plan ran; nothing was applied, no lane ran
	if got := []int{len(e.inboxCalls("fetch")), len(e.inboxCalls("decide")), len(e.inboxCalls("plan"))}; !slices.Equal(got, []int{1, 1, 1}) {
		t.Fatalf("fetch, decide, plan calls: %v\n%v", got, e.inboxLog())
	}
	if len(e.inboxCalls("apply"))+len(e.inboxCalls("lanes"))+len(e.inboxCalls("approve")) != 0 || len(e.applied()) != 0 {
		t.Fatalf("mail was touched before the approval: %v", e.inboxLog())
	}
	if f := e.inboxCalls("fetch")[0]; strings.Contains(f, "lanes") || !strings.Contains(f, "--json") {
		t.Errorf("fetch: %s", f)
	}
	if p := e.inboxCalls("plan")[0]; !strings.Contains(p, "--no-task") || !strings.Contains(p, "--unattended") {
		t.Errorf("plan makes a task or is not unattended: %s", p)
	}
	if a := e.inboxCalls("plan"); len(a) != 1 {
		t.Errorf("plan calls: %v", a)
	}

	// the approval holds every item apply would touch; trust decided the
	// automatic one, so only the three pending items wait for a person
	if av.Status != "open" || av.Trust != "inbox" || len(av.Items) != 4 || av.Waiting != 3 {
		t.Fatalf("approval = %+v", av)
	}
	for _, it := range av.Items {
		auto := it.ID == m1
		if (it.Verdict == "auto") != auto || (auto && it.By != "trust") {
			t.Errorf("item %s: verdict %q by %q", it.ID, it.Verdict, it.By)
		}
	}
	for _, it := range av.Items {
		if it.ID == m5 && len(it.Class) != 0 {
			t.Errorf("the item held by a rule has a trust class %v", it.Class)
		}
	}
	// the superseded copy of an earlier run is not an item
	if n := strings.Count(strings.Join(func() []string {
		var ids []string
		for _, it := range av.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}(), ","), m2); n != 1 {
		t.Errorf("%s appears %d times", m2, n)
	}

	// decide: m2 and m5 approved, m3 rejected
	var d struct {
		Status  string `json:"status"`
		Resumed bool   `json:"resumed"`
	}
	e.okJSON(&d, "approval", "set", c.WaitingOn[0],
		"--item", strconv.Itoa(av.pos(t, m2))+"=approve", "--item", strconv.Itoa(av.pos(t, m3))+"=reject", "--item", strconv.Itoa(av.pos(t, m5))+"=approve", "--yes")
	if d.Status != "decided" || !d.Resumed {
		t.Fatalf("decision = %+v", d)
	}
	eventually(t, "the run to succeed", func() bool { return e.show(c.Run).Status == "succeeded" })

	// the script was told, by the wrapper, with the numbers of its own plan
	ap := e.inboxCalls("approve")
	if len(ap) != 1 || !strings.HasSuffix(ap[0], "--only 2,4 --reject 3 --json") {
		t.Errorf("approve calls: %v", ap)
	}
	if s, r := e.trustOf("label:Finance"); s != 2 || r != 0 {
		t.Errorf("label:Finance streak %d, resets %d", s, r)
	}
	if s, r := e.trustOf("reply:draft"); s != 0 || r != 2 {
		t.Errorf("reply:draft streak %d, resets %d (a rejection resets)", s, r)
	}

	// apply ran once, on the automatic and the approved items, never the rejected one
	if got := e.applied(); !slices.Equal(got, []string{"applied " + m1, "applied " + m2, "applied " + m5}) {
		t.Errorf("applied = %v", got)
	}
	if n := len(e.inboxCalls("apply")); n != 1 {
		t.Errorf("apply calls: %d", n)
	}
	if a := e.inboxCalls("apply")[0]; !strings.Contains(a, "--no-task") {
		t.Errorf("apply makes a task: %s", a)
	}
	// the receipt is read from the files the summary names
	_, out := e.stepOf(c.Run, "apply")
	files, _ := out["files"].([]any)
	var receipt string
	for _, f := range files {
		if s, _ := f.(string); strings.HasSuffix(s, "/receipt.json") {
			receipt = s
		}
	}
	if receipt == "" || !strings.HasPrefix(receipt, filepath.Join(e.scratch, "inbox")) {
		t.Fatalf("apply summary files: %v", files)
	}
	var rc struct {
		Backend     string         `json:"backend"`
		Results     map[string]any `json:"results"`
		PendingLeft int            `json:"pending_left"`
	}
	b, err := os.ReadFile(receipt) //nolint:gosec // the fake's receipt
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rc); err != nil || len(rc.Results) != 3 || rc.PendingLeft != 0 {
		t.Errorf("receipt: %+v, %v", rc, err)
	}
	if got := out["pending_left"]; fmt.Sprint(got) != "0" {
		t.Errorf("pending_left = %v: every pending item has a verdict", got)
	}

	// the tlc task of the approval is the only task: the script made none
	if got := strings.TrimSpace(strings.Join(readLines(t, filepath.Join(e.scratch, "tlc", "T-0001")), "")); got == "" {
		t.Error("no task for the approval")
	}
	for _, l := range e.inboxLog() {
		if strings.Contains(l, "--task") && !strings.Contains(l, "--no-task") {
			t.Errorf("the script was asked to make a task: %s", l)
		}
	}
	// the same decision again changes nothing
	e.okJSON(&d, "approval", "set", c.WaitingOn[0], "--item", strconv.Itoa(av.pos(t, m2))+"=approve", "--yes")
	if d.Resumed || len(e.applied()) != 3 || len(e.inboxCalls("approve")) != 1 {
		t.Errorf("a repeated decision: %+v, applied %v", d, e.applied())
	}
}

// TestInboxTriageRejectNeverApplies: when every pending item is rejected and
// nothing is automatic, the run succeeds and apply never runs.
func TestInboxTriageRejectNeverApplies(t *testing.T) {
	e := triageEnv(t)
	e.inboxFlag("no-auto", true)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()

	c, av := e.startTriage()
	if len(av.Items) != 3 || av.Waiting != 3 {
		t.Fatalf("approval = %+v", av)
	}
	var d struct {
		Resumed bool `json:"resumed"`
	}
	e.okJSON(&d, "approval", "set", c.WaitingOn[0], "--all=reject", "--reason", "not now", "--yes")
	if !d.Resumed {
		t.Fatal("the run was not resumed")
	}
	eventually(t, "the run to succeed", func() bool { return e.show(c.Run).Status == "succeeded" })
	if status, reason := e.show(c.Run).step("apply"); status != "skipped" || reason != "when_false" {
		t.Errorf("apply: %s %s", status, reason)
	}
	if len(e.inboxCalls("apply")) != 0 || len(e.applied()) != 0 {
		t.Errorf("a rejected item was applied: %v %v", e.inboxLog(), e.applied())
	}
	if ap := e.inboxCalls("approve"); len(ap) != 1 || !strings.HasSuffix(ap[0], "--reject 2,3,4 --json") {
		t.Errorf("approve calls: %v", ap)
	}
	for _, k := range []string{"label:Finance", "reply:draft", "archive"} {
		if s, _ := e.trustOf(k); k != "archive" && s != 0 {
			t.Errorf("%s streak %d after a rejection", k, s)
		}
	}
}

// TestInboxTriageAutomaticItemsNeedNoPerson: when trust already approves
// everything the run never parks, and still applies behind a recorded
// decision.
func TestInboxTriageAutomaticItemsNeedNoPerson(t *testing.T) {
	e := triageEnv(t)
	// both training keys graduated, and the message held by a rule is not there
	if err := os.MkdirAll(filepath.Join(e.scratch, "inbox"), 0o750); err != nil {
		t.Fatal(err)
	}
	trust := `{"read":{"streak":3,"threshold":3},"archive":{"streak":3,"threshold":3},"label:Finance":{"streak":3,"threshold":3},"reply:draft":{"streak":5,"threshold":5}}`
	if err := os.WriteFile(filepath.Join(e.scratch, "inbox", "trust.json"), []byte(trust), 0o600); err != nil {
		t.Fatal(err)
	}
	e.inboxFlag("no-guard", true)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()

	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("run: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
	}
	if got := e.applied(); !slices.Equal(got, []string{"applied " + m1, "applied " + m2, "applied " + m3}) {
		t.Errorf("applied = %v", got)
	}
	if len(e.inboxCalls("approve")) != 0 {
		t.Errorf("no person decided, nothing to record: %v", e.inboxCalls("approve"))
	}
	var list []struct {
		ID, Status string
	}
	e.okJSON(&list, "approval", "list")
	if len(list) != 1 || list[0].Status != "decided" {
		t.Errorf("approvals = %+v", list)
	}
}

// TestInboxTriagePartialExit3: the script's exit 3 is data, and the nodes
// turn it into what the spec says: fetch goes on with the accounts it got,
// plan lists what is invalid, apply fails the step when an action failed, and
// a fetch that got nothing stops the run before anything is decided.
func TestInboxTriagePartialExit3(t *testing.T) {
	t.Run("one account is not fetched", func(t *testing.T) {
		e := triageEnv(t)
		e.inboxFlag("fail-account", true)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		c, av := e.startTriage()
		if len(av.Items) != 3 { // beta's message is not in this run
			t.Errorf("items: %+v", av.Items)
		}
		_, out := e.stepOf(c.Run, "fetch")
		if f, _ := out["failed_accounts"].([]any); len(f) != 1 || f[0] != "beta" || fmt.Sprint(out["exit"]) != "3" {
			t.Errorf("fetch output: %v", out)
		}
		if st, _ := e.show(c.Run).step("fetch"); st != "succeeded" {
			t.Errorf("fetch is %s", st)
		}
	})

	t.Run("no account is fetched", func(t *testing.T) {
		e := triageEnv(t)
		e.inboxFlag("fail-all", true)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		var c created
		if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait"); r.Exit == 0 || c.Status != "failed" {
			t.Fatalf("run: exit %d, %+v", r.Exit, c)
		}
		v := e.show(c.Run)
		if st, _ := v.step("fetch"); st != "failed" {
			t.Errorf("fetch is %s", st)
		}
		for _, s := range []string{"decide", "plan", "approve", "apply"} {
			if st, reason := v.step(s); st != "skipped" || reason != "upstream_failed" {
				t.Errorf("%s: %s %s", s, st, reason)
			}
		}
		if len(e.inboxCalls("decide")) != 0 || len(e.inboxCalls("apply")) != 0 {
			t.Errorf("work went on after a failed fetch: %v", e.inboxLog())
		}
		rec, _ := e.stepOf(c.Run, "fetch")
		if rec.Failure == nil || rec.Failure.Code != core.CodeNodeReportedFailure {
			t.Errorf("fetch failure: %+v", rec.Failure)
		}
	})

	t.Run("an item is invalid", func(t *testing.T) {
		e := triageEnv(t)
		e.inboxFlag("invalid", true)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		c, av := e.startTriage()
		if len(av.Items) != 4 {
			t.Errorf("an invalid item was offered: %+v", av.Items)
		}
		_, out := e.stepOf(c.Run, "plan")
		if inv, _ := out["invalid"].([]any); len(inv) != 1 {
			t.Errorf("plan output: %v", out)
		}
		if st, _ := e.show(c.Run).step("plan"); st != "succeeded" {
			t.Errorf("plan is %s", st)
		}
	})

	t.Run("an action fails", func(t *testing.T) {
		e := triageEnv(t)
		e.inboxFlag("fail-apply", true)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		c, av := e.startTriage()
		var d struct{ Resumed bool }
		e.okJSON(&d, "approval", "set", c.WaitingOn[0], "--all=approve", "--yes")
		eventually(t, "the run to fail", func() bool { return e.show(c.Run).Status == "failed" })
		rec, out := e.stepOf(c.Run, "apply")
		if rec.Status != core.StepFailed || rec.Failure == nil || rec.Failure.Code != core.CodeNodeReportedFailure {
			t.Errorf("apply: %s %+v", rec.Status, rec.Failure)
		}
		raw, _ := rec.Raw.(map[string]any)
		if got, _ := raw["stdout"].(string); !strings.Contains(got, "imap: store failed") {
			t.Errorf("the failed items are kept in the record: %v (output %v)", rec.Raw, out)
		}
		// the decisions reached the script before the apply ran, the rest was applied
		if got := e.applied(); !slices.Equal(got, []string{"applied " + m1, "applied " + m3, "applied " + m5}) {
			t.Errorf("applied = %v", got)
		}
		if len(e.inboxCalls("apply")) != 1 || len(av.Items) != 4 {
			t.Errorf("apply was repeated: %v", e.inboxCalls("apply"))
		}
	})
}

// TestInboxTriageParkedRunSkipsTheNextFire: while a run waits for a person the
// schedule's next fire is skipped, not queued; the script carries the pending
// items into the run after the decision.
func TestInboxTriageParkedRunSkipsTheNextFire(t *testing.T) {
	e := triageEnv(t)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()

	c, av := e.startTriage()
	var second struct {
		Run     string `json:"run"`
		Created bool   `json:"created"`
		Skipped bool   `json:"skipped"`
	}
	r := e.jsonOf(&second, "run", "create", "--flow", "inbox-triage")
	if r.Exit != 0 || !second.Skipped || second.Created {
		t.Fatalf("second fire while parked: exit %d, %+v\n%s", r.Exit, second, r.Stderr)
	}
	if n := len(e.inboxCalls("fetch")); n != 1 {
		t.Errorf("a skipped fire fetched: %d fetches", n)
	}
	e.ok("approval", "set", c.WaitingOn[0], "--all=reject", "--yes")
	eventually(t, "the run to succeed", func() bool { return e.show(c.Run).Status == "succeeded" })
	// the group is free again
	var third created
	if r := e.jsonOf(&third, "run", "create", "--flow", "inbox-triage", "--wait"); r.Exit != core.ExitAwaiting || !third.Created {
		t.Fatalf("fire after the decision: exit %d, %+v\n%s", r.Exit, third, r.Stderr)
	}
	_ = av
}

// TestInboxWrappers: the wrappers the trust provider and the plan node run,
// against the fake script, outside the engine.
func TestInboxWrappers(t *testing.T) {
	e := triageEnv(t)
	run := func(stdin, name string, args ...string) (string, string, int) {
		t.Helper()
		r := e.runShim(stdin, name, args...)
		return r.Stdout, r.Stderr, r.Exit
	}
	if _, _, code := run("", "inbox", "fetch", "--json"); code != 0 {
		t.Fatal("fake fetch")
	}
	const stamp = "20261118-090001"
	run("", "inbox", "decide", stamp, "--json")
	planFile := filepath.Join(t.TempDir(), "plan.json")
	if _, errOut, code := run("", "inbox-plan", "--out", planFile, stamp); code != 0 {
		t.Fatalf("inbox-plan: exit %d\n%s", code, errOut)
	}
	var pl struct {
		RunDir string `json:"run_dir"`
		Items  []struct {
			N      int    `json:"n"`
			Status string `json:"status"`
		} `json:"items"`
	}
	b, err := os.ReadFile(planFile) //nolint:gosec // a temp file
	if err != nil || json.Unmarshal(b, &pl) != nil {
		t.Fatalf("plan out: %s, %v", b, err)
	}
	if len(pl.Items) != 4 { // the superseded copy is not there
		t.Errorf("plan items: %+v", pl.Items)
	}

	// trust: the script's counters, and the keys of the automatic items
	out, _, code := run("", "inbox-trust", "list", filepath.Join(pl.RunDir, "plan.json"))
	var levels []struct {
		Class     string `json:"class"`
		Level     string `json:"level"`
		Streak    int    `json:"streak"`
		Threshold int    `json:"threshold"`
		Resets    int    `json:"resets"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &levels) != nil || len(levels) != 4 {
		t.Fatalf("trust list: exit %d\n%s", code, out)
	}
	by := map[string]string{}
	for _, l := range levels {
		by[l.Class] = l.Level
		if l.Class == "reply:draft" && (l.Streak != 0 || l.Threshold != 5 || l.Resets != 1) {
			t.Errorf("reply:draft: %+v", l)
		}
	}
	if by["archive"] != "auto" || by["read"] != "auto" || by["label:Finance"] != "ask" || by["reply:draft"] != "ask" {
		t.Errorf("levels: %v", by)
	}
	// a key the script's status does not list is auto when an automatic item has it
	if err := os.WriteFile(filepath.Join(e.scratch, "inbox", "trust.json"), []byte(`{"archive":{"streak":0,"threshold":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, _ = run("", "inbox-trust", "list", filepath.Join(pl.RunDir, "plan.json"))
	if err := json.Unmarshal([]byte(out), &levels); err != nil {
		t.Fatal(err)
	}
	by = map[string]string{}
	for _, l := range levels {
		by[l.Class] = l.Level
	}
	if by["archive"] != "auto" || by["read"] != "auto" {
		t.Errorf("keys of automatic items: %v", by)
	}

	// set: ask is a reset, auto is not possible
	if _, _, code := run("", "inbox-trust", "set", "archive", "ask", "too eager"); code != 0 {
		t.Errorf("set ask: exit %d", code)
	}
	if s, r := e.trustOf("archive"); s != 0 || r != 1 {
		t.Errorf("after set ask: streak %d resets %d", s, r)
	}
	if _, errOut, code := run("", "inbox-trust", "set", "archive", "auto", "x"); code == 0 || !strings.Contains(errOut, "no command to promote") {
		t.Errorf("set auto: exit %d\n%s", code, errOut)
	}

	// approve: what the script took is applied, what it did not is skipped
	decisions := `[{"id":"a","verdict":"approve","item":{"n":2}},{"id":"b","verdict":"reject","item":{"n":3}},` +
		`{"id":"c","verdict":"approve","item":{"n":1}},{"id":"d","verdict":"approve","item":{"n":42}},` +
		`{"id":"e","verdict":"correct","item":{"n":4}},{"id":"f","verdict":"approve"}]`
	out, errOut, code := run(decisions, "inbox-approve", stamp)
	var res struct{ Applied, Skipped []string }
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil {
		t.Fatalf("inbox-approve: exit %d\n%s\n%s", code, out, errOut)
	}
	if !slices.Equal(res.Applied, []string{"a", "b", "c"}) || !slices.Equal(res.Skipped, []string{"d", "e", "f"}) {
		t.Errorf("approve result: %+v", res)
	}
	if ap := e.inboxCalls("approve"); len(ap) != 1 || !strings.HasSuffix(ap[0], "--only 2,1,42 --reject 3 --json") {
		t.Errorf("approve calls: %v", ap)
	}
	// nothing to record is not a call
	out, _, code = run(`[{"id":"x","verdict":"correct","item":{"n":4}}]`, "inbox-approve", stamp)
	if code != 0 || strings.TrimSpace(out) != `{"applied":[],"skipped":["x"]}` || len(e.inboxCalls("approve")) != 1 {
		t.Errorf("only a correction: exit %d, %s, %v", code, out, e.inboxCalls("approve"))
	}
}

// TestRecordInboxTriage re-records flows/inbox-triage/record against the fake
// script. It is a maintenance step, not a test: it runs only with
// FLO_E2E_RECORD=1, after a change to the flow, its tests/ or the nodes it
// uses (a recording is bound to the exact bundle). The fake keeps its files
// under a fixed directory so the recorded argv and paths do not depend on
// the machine.
func TestRecordInboxTriage(t *testing.T) {
	if os.Getenv("FLO_E2E_RECORD") == "" {
		t.Skip("set FLO_E2E_RECORD=1 to re-record flows/inbox-triage/record")
	}
	fixed := "/tmp/flo-inbox-record"
	_ = os.RemoveAll(fixed)
	if err := os.MkdirAll(fixed, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fixed) })
	e := triageEnv(t, "FLO_E2E_DIR="+fixed)
	bundle := triageBundle(t)
	if err := os.RemoveAll(filepath.Join(bundle, "record")); err != nil {
		t.Fatal(err)
	}
	e.ok("flow", "apply", "-f", bundle, "--yes")
	if res, r := e.flowTest("inbox-triage", "--record", "--yes"); r.Exit != 0 || !res.Passed {
		t.Fatalf("record: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	dst := filepath.Join(repoRoot, "flows", "inbox-triage", "record")
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	if out, err := cpR(filepath.Join(bundle, "record"), dst); err != nil {
		t.Fatalf("copy record: %v\n%s", err, out)
	}
}
