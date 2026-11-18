package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const cliTenant = core.DefaultTenant

// cliRig runs flo commands against the fakes.
type cliRig struct {
	t  *testing.T
	fx *coretest.Fixture
}

func newCLIRig(t *testing.T) *cliRig {
	t.Helper()
	t.Setenv(EnvProfile, "jad")
	t.Setenv(EnvTenant, "")
	// no terminal unless a test gives one
	old := promptSource
	promptSource = func() *kitcli.PromptTTY { return nil }
	t.Cleanup(func() { promptSource = old })
	return &cliRig{t: t, fx: coretest.New()}
}

// owner makes the caller the owner: the wildcard grant.
func (r *cliRig) owner() {
	r.t.Helper()
	if _, err := r.fx.Grants.Set(context.Background(), cliTenant, "jad", core.CapabilityAny); err != nil {
		r.t.Fatal(err)
	}
}

func (r *cliRig) run(args ...string) (string, string, error) {
	r.t.Helper()
	root := NewRootWith(func(context.Context) (*core.Services, error) { return r.fx.Services(), nil })
	var out, errOut bytes.Buffer
	root.Cmd.SetOut(&out)
	root.Cmd.SetErr(&errOut)
	root.Cmd.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err := root.Execute(context.Background())
	return out.String(), errOut.String(), err
}

func (r *cliRig) mustRun(args ...string) string {
	r.t.Helper()
	out, errOut, err := r.run(args...)
	if err != nil {
		r.t.Fatalf("flo %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func decodeJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.NewDecoder(strings.NewReader(s)).Decode(v); err != nil && err != io.EOF {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
}

func TestGrantListSetUnset(t *testing.T) {
	r := newCLIRig(t)
	r.owner()
	out := r.mustRun("grant", "set", "noor", "mail.read", "--yes", "--format", "json")
	var ch struct {
		Profile, Capability, Action string
		Changed                     bool
	}
	decodeJSON(t, out, &ch)
	if ch.Profile != "noor" || ch.Capability != "mail.read" || ch.Action != "set" || !ch.Changed {
		t.Errorf("set = %+v", ch)
	}
	// idempotent
	decodeJSON(t, r.mustRun("grant", "set", "noor", "mail.read", "--yes", "--format", "json"), &ch)
	if ch.Changed {
		t.Error("the second set changed something")
	}
	var rows []grantRow
	decodeJSON(t, r.mustRun("grant", "list", "noor", "--format", "json"), &rows)
	if len(rows) != 1 || rows[0].Capability != "mail.read" || rows[0].Source != "local" {
		t.Errorf("rows = %+v", rows)
	}
	decodeJSON(t, r.mustRun("grant", "list", "--format", "json"), &rows)
	if len(rows) != 2 { // the owner's wildcard and noor's grant
		t.Errorf("every profile: %+v", rows)
	}
	decodeJSON(t, r.mustRun("grant", "unset", "noor", "mail.read", "--yes", "--format", "json"), &ch)
	if !ch.Changed || ch.Action != "unset" {
		t.Errorf("unset = %+v", ch)
	}
	decodeJSON(t, r.mustRun("grant", "unset", "noor", "mail.read", "--yes", "--format", "json"), &ch)
	if ch.Changed {
		t.Error("the second unset changed something")
	}
}

func TestGrantProvisionedIsExit4(t *testing.T) {
	r := newCLIRig(t)
	r.owner()
	if err := r.fx.Grants.Provision(context.Background(), cliTenant, "noor", "mail.read"); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"set", "unset"} {
		_, _, err := r.run("grant", verb, "noor", "mail.read", "--yes")
		if !core.IsCode(err, core.CodeGrantProvisioned) || core.ExitCode(err) != 4 {
			t.Errorf("%s: %v (exit %d)", verb, err, core.ExitCode(err))
		}
	}
}

func TestGrantNeedsTheWildcardAndAWriteNeedsYes(t *testing.T) {
	r := newCLIRig(t)
	// the caller holds nothing: deny by default
	_, _, err := r.run("grant", "set", "noor", "mail.read", "--yes")
	if !core.IsCode(err, core.CodeGrantDenied) || core.ExitCode(err) != 5 {
		t.Errorf("a caller without the wildcard: %v", err)
	}
	r.owner()
	// no terminal, no --yes
	_, _, err = r.run("grant", "set", "noor", "mail.read")
	if !core.IsCode(err, core.CodeConfirmationRequired) || core.ExitCode(err) != 2 {
		t.Errorf("without --yes: %v (exit %d)", err, core.ExitCode(err))
	}
	if gs, _ := r.fx.Grants.List(context.Background(), cliTenant, "noor"); len(gs) != 0 {
		t.Errorf("a refused write granted %v", gs)
	}
}

func TestGrantDryRunWritesNothing(t *testing.T) {
	r := newCLIRig(t)
	r.owner()
	out := r.mustRun("grant", "set", "noor", "mail.read", "--dry-run", "--format", "json")
	var plan kitcli.Plan
	decodeJSON(t, out, &plan)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "create" || plan.Effects[0].Target != "grant:noor/mail.read" {
		t.Errorf("plan = %+v", plan)
	}
	if gs, _ := r.fx.Grants.List(context.Background(), cliTenant, "noor"); len(gs) != 0 {
		t.Errorf("--dry-run granted %v", gs)
	}
	// a no-op is said so
	if _, err := r.fx.Grants.Set(context.Background(), cliTenant, "noor", "mail.read"); err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, r.mustRun("grant", "set", "noor", "mail.read", "--dry-run", "--format", "json"), &plan)
	if len(plan.Effects) != 0 || len(plan.Warnings) != 1 {
		t.Errorf("no-op plan = %+v", plan)
	}
}

func TestGrantSetAsksOnATerminal(t *testing.T) {
	r := newCLIRig(t)
	r.owner()
	var question bytes.Buffer
	promptSource = kitcli.PromptSourceFromReadWriter(strings.NewReader("y\n"), &question)
	r.mustRun("grant", "set", "noor", "mail.read")
	if !strings.Contains(question.String(), "set noor mail.read?") {
		t.Errorf("question = %q", question.String())
	}
	if ok, _ := r.fx.Grants.Covers(context.Background(), cliTenant, "noor", "mail.read"); !ok {
		t.Error("answering y did not grant")
	}
	promptSource = kitcli.PromptSourceFromReadWriter(strings.NewReader("n\n"), io.Discard)
	_, _, err := r.run("grant", "unset", "noor", "mail.read")
	if !core.IsCode(err, core.CodeConfirmationRequired) {
		t.Errorf("answering n: %v", err)
	}
	if ok, _ := r.fx.Grants.Covers(context.Background(), cliTenant, "noor", "mail.read"); !ok {
		t.Error("answering n revoked")
	}
}

func TestCallerIdentityFromEnvAndConfig(t *testing.T) {
	r := newCLIRig(t)
	if _, err := r.fx.Grants.Set(context.Background(), cliTenant, "lead", core.CapabilityAny); err != nil {
		t.Fatal(err)
	}
	// FLO_PROFILE is jad (no grants): denied
	if _, _, err := r.run("grant", "set", "noor", "mail.read", "--yes"); !core.IsCode(err, core.CodeGrantDenied) {
		t.Fatalf("jad: %v", err)
	}
	t.Setenv(EnvProfile, "lead")
	r.mustRun("grant", "set", "noor", "mail.read", "--yes")
	// without the override, the owner_profile of the config, then $USER
	t.Setenv(EnvProfile, "")
	t.Setenv("USER", "lead")
	r.mustRun("grant", "set", "noor", "mail.send", "--yes")
	root := NewRootWith(func(context.Context) (*core.Services, error) { return r.fx.Services(), nil })
	root.Viper.Set(ConfigOwnerProfile, "cfgowner")
	if got, err := accessCaller(root); err != nil || got != "cfgowner" {
		t.Errorf("config owner_profile: %q %v", got, err)
	}
	t.Setenv(EnvProfile, "override")
	if got, _ := accessCaller(root); got != "override" {
		t.Errorf("FLO_PROFILE does not override the config: %q", got)
	}
	t.Setenv(EnvProfile, "")
	t.Setenv("USER", "")
	if _, err := accessCaller(NewRootWith(func(context.Context) (*core.Services, error) { return nil, nil })); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("no identity at all: %v", err)
	}
}

// ---- approvals

type apRig struct {
	*cliRig
	svc *approvals.Service
	ap  core.Approval
}

func newApRig(t *testing.T, ids ...string) *apRig {
	t.Helper()
	cr := newCLIRig(t)
	fx := cr.fx
	svc := approvals.New(approvals.Deps{
		Store: fx.DB.Approvals, Tasks: fx.Tasks, Resumer: fx.Resumer, Trust: fx.Trust, Runs: fx.DB.Runs, Flows: fx.DB.Flows,
		Jobs: fx.Jobs, RunLog: fx.RunLog, Clock: fx.Clock, IDs: fx.IDs,
	})
	r := &apRig{cliRig: cr, svc: svc}
	if _, _, err := fx.DB.Runs.CreateRun(context.Background(), core.NewRun{
		ID: "run_1", Tenant: cliTenant, Flow: core.FlowRef{Name: "triage", Version: "1", Hash: "h"}, StartedBy: "jad",
	}); err != nil {
		t.Fatal(err)
	}
	var items []core.ApprovalItem
	for _, id := range ids {
		items = append(items, core.ApprovalItem{ID: id, Action: "archive", Why: "newsletter"})
	}
	ap, err := svc.Open(context.Background(), cliTenant, "run_1", core.StepRun{ID: "stp_1", StepID: "approve"}, core.ApprovalRequest{Title: "Approve triage", Items: items})
	if err != nil {
		t.Fatal(err)
	}
	r.ap = ap
	return r
}

func (r *apRig) run(args ...string) (string, string, error) {
	r.t.Helper()
	root := NewRootWith(func(context.Context) (*core.Services, error) {
		s := r.fx.Services()
		s.ApprovalSvc = r.svc
		return s, nil
	})
	var out, errOut bytes.Buffer
	root.Cmd.SetOut(&out)
	root.Cmd.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute(context.Background())
	return out.String(), errOut.String(), err
}

func (r *apRig) mustRun(args ...string) string {
	r.t.Helper()
	out, errOut, err := r.run(args...)
	if err != nil {
		r.t.Fatalf("flo %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func (r *apRig) approval() core.Approval {
	r.t.Helper()
	a, err := r.svc.Get(context.Background(), cliTenant, r.ap.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func TestApprovalListAndShow(t *testing.T) {
	r := newApRig(t, "m1", "m2")
	var rows []approvalRow
	decodeJSON(t, r.mustRun("approval", "list", "--format", "json"), &rows)
	if len(rows) != 1 || rows[0].ID != string(r.ap.ID) || rows[0].Status != "open" || rows[0].Decided != "0/2" || rows[0].Run != "run_1" || rows[0].Task == "" {
		t.Errorf("rows = %+v", rows)
	}
	decodeJSON(t, r.mustRun("approval", "list", "--status", "decided", "--format", "json"), &rows)
	if len(rows) != 0 {
		t.Errorf("decided = %+v", rows)
	}
	if _, _, err := r.run("approval", "list", "--status", "nope"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("bad --status: %v", err)
	}
	var v approvalView
	decodeJSON(t, r.mustRun("approval", "show", string(r.ap.ID), "--format", "json"), &v)
	if v.ID != string(r.ap.ID) || v.Status != "open" || v.Waiting != 2 || len(v.Items) != 2 || v.Items[0].N != 1 || v.Items[1].ID != "m2" ||
		v.Items[0].Action != "archive" || len(v.Verdicts) != 3 {
		t.Errorf("view = %+v", v)
	}
	if _, _, err := r.run("approval", "show", "ap_0000000000000000000000zzzz"); !core.IsCode(err, core.CodeApprovalNotFound) || core.ExitCode(err) != 3 {
		t.Errorf("unknown approval: %v", err)
	}
	if _, _, err := r.run("approval", "show", "run_1"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("not an approval id: %v", err)
	}
}

func TestApprovalSetItemsByIdAndPosition(t *testing.T) {
	r := newApRig(t, "m1", "m2", "m3")
	out := r.mustRun("approval", "set", string(r.ap.ID), "--item", "m1=approve", "--item", "2=reject", "--reason", "spam", "--yes", "--format", "json")
	var d approvalDecision
	decodeJSON(t, out, &d)
	if d.Status != "open" || d.Decided != 2 || d.Waiting != 1 || !d.Changed || d.Resumed {
		t.Errorf("decision = %+v", d)
	}
	a := r.approval()
	if a.Items[0].Decision.Verdict != core.VerdictApprove || a.Items[0].Decision.By != "jad" || a.Items[0].Decision.Reason != "spam" ||
		a.Items[1].Decision.Verdict != core.VerdictReject || a.Items[2].Decision != nil {
		t.Errorf("items = %+v", a.Items)
	}
	// the last one completes it and resumes the run, once
	decodeJSON(t, r.mustRun("approval", "set", string(r.ap.ID), "--all=approve", "--yes", "--format", "json"), &d)
	if d.Status != "decided" || !d.Resumed || d.Waiting != 0 {
		t.Errorf("decision = %+v", d)
	}
	if n := len(r.fx.Resumer.Calls()); n != 1 {
		t.Errorf("resumes = %d", n)
	}
}

// the same request twice yields the same state, and resumes once
func TestApprovalSetIsIdempotent(t *testing.T) {
	r := newApRig(t, "m1", "m2")
	args := []string{"approval", "set", string(r.ap.ID), "--all=approve", "--yes", "--format", "json"}
	var first, second approvalDecision
	decodeJSON(t, r.mustRun(args...), &first)
	decodeJSON(t, r.mustRun(args...), &second)
	if !first.Changed || !first.Resumed || second.Changed || second.Resumed {
		t.Errorf("first = %+v second = %+v", first, second)
	}
	if n := len(r.fx.Resumer.Calls()); n != 1 {
		t.Errorf("resumes = %d", n)
	}
}

func TestApprovalSetWithoutTerminalAndWithoutYes(t *testing.T) {
	r := newApRig(t, "m1")
	_, _, err := r.run("approval", "set", string(r.ap.ID), "--all=approve")
	if !core.IsCode(err, core.CodeConfirmationRequired) || core.ExitCode(err) != 2 {
		t.Fatalf("err = %v (exit %d), want confirmation_required exit 2", err, core.ExitCode(err))
	}
	if a := r.approval(); a.Items[0].Decision != nil || len(r.fx.Resumer.Calls()) != 0 {
		t.Error("a write without confirmation decided something")
	}
	// --yes
	r.mustRun("approval", "set", string(r.ap.ID), "--all=approve", "--yes")
	if a := r.approval(); a.Items[0].Decision == nil {
		t.Error("--yes did not decide")
	}
}

func TestApprovalSetAsksOnATerminal(t *testing.T) {
	r := newApRig(t, "m1")
	var q bytes.Buffer
	promptSource = kitcli.PromptSourceFromReadWriter(strings.NewReader("yes\n"), &q)
	r.mustRun("approval", "set", string(r.ap.ID), "--item", "m1=reject")
	if !strings.Contains(q.String(), "m1=reject") || r.approval().Items[0].Decision == nil {
		t.Errorf("question %q, approval %+v", q.String(), r.approval())
	}
}

func TestApprovalSetDryRun(t *testing.T) {
	r := newApRig(t, "m1", "m2")
	r.mustRun("approval", "set", string(r.ap.ID), "--item", "m1=approve", "--yes")
	var plan kitcli.Plan
	decodeJSON(t, r.mustRun("approval", "set", string(r.ap.ID), "--item", "m1=reject", "--item", "m2=approve", "--dry-run", "--format", "json"), &plan)
	kinds := map[string]string{}
	for _, e := range plan.Effects {
		kinds[e.Kind+" "+e.Target] = e.Detail
	}
	if kinds["decide item:"+string(r.ap.ID)+"/m2"] != "approve" || len(kinds) < 3 {
		t.Errorf("effects = %+v", plan.Effects)
	}
	if _, resume := kinds["resume run:run_1"]; !resume {
		t.Errorf("the plan does not say the last item resumes the run: %+v", plan.Effects)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "m1 keeps its decision (approve)") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	a := r.approval()
	if a.Items[1].Decision != nil || len(r.fx.Resumer.Calls()) != 0 {
		t.Error("--dry-run decided")
	}
	// --dry-run needs no --yes and no terminal
}

func TestApprovalSetCorrection(t *testing.T) {
	r := newApRig(t, "m1")
	r.mustRun("approval", "set", string(r.ap.ID), "--item", "1=correct", "--correction", `1={"label":"bills"}`, "--yes")
	d := r.approval().Items[0].Decision
	if d.Verdict != core.VerdictCorrect || d.Correction.(map[string]any)["label"] != "bills" {
		t.Errorf("decision = %+v", d)
	}
	r2 := newApRig(t, "m1")
	if _, _, err := r2.run("approval", "set", string(r2.ap.ID), "--item", "1=approve", "--correction", `1={"x":1}`, "--yes"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("correction on an approved item: %v", err)
	}
	if _, _, err := r2.run("approval", "set", string(r2.ap.ID), "--item", "1=correct", "--correction", `1=nope`, "--yes"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("correction that is not JSON: %v", err)
	}
}

func TestApprovalSetUsageErrors(t *testing.T) {
	r := newApRig(t, "m1")
	id := string(r.ap.ID)
	for name, args := range map[string][]string{
		"nothing":           {"approval", "set", id, "--yes"},
		"bad item syntax":   {"approval", "set", id, "--item", "m1", "--yes"},
		"auto by a person":  {"approval", "set", id, "--item", "m1=auto", "--yes"},
		"bad all":           {"approval", "set", id, "--all=maybe", "--yes"},
		"no such item":      {"approval", "set", id, "--item", "zz=approve", "--yes"},
		"position too high": {"approval", "set", id, "--item", "9=approve", "--yes"},
	} {
		_, _, err := r.run(args...)
		if err == nil || core.ExitCode(err) == 0 {
			t.Errorf("%s: no error", name)
			continue
		}
		if got := core.ExitCode(err); got != 2 && got != 3 {
			t.Errorf("%s: exit %d (%v), want 2 or 3", name, got, err)
		}
	}
	if a := r.approval(); a.Items[0].Decision != nil {
		t.Error("a refused write decided")
	}
}

func TestApprovalSetOnACancelledApproval(t *testing.T) {
	r := newApRig(t, "m1")
	if err := r.svc.CancelRun(context.Background(), cliTenant, "run_1"); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.run("approval", "set", string(r.ap.ID), "--all=approve", "--yes")
	if !core.IsCode(err, core.CodeApprovalAlreadyClosed) || core.ExitCode(err) != 4 {
		t.Errorf("err = %v", err)
	}
}
