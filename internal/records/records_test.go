package records_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/records"
)

var at = time.Date(2026, 11, 18, 12, 0, 0, 123, time.UTC)

// schema compiles one of the vendored dek schemas (testdata/dek).
func schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "dek", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource(name+".json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(name + ".json")
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

// validate marshals v as a wire document and validates it.
func validate(t *testing.T, s *jsonschema.Schema, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(doc); err != nil {
		t.Fatalf("does not validate: %v\n%s", err, b)
	}
	return doc
}

func run(status core.RunStatus) core.Run {
	return core.Run{
		ID: "run_01", Tenant: "default", Flow: core.FlowRef{Name: "triage", Version: "1", Hash: "h"},
		Status: status, Attempts: 1, MaxAttempts: 3, UpdatedAt: at,
	}
}

func TestJobSchemaRejectsWhatItShould(t *testing.T) {
	// the validator is live: a status dek does not know, a missing field
	s := schema(t, "Job")
	if err := s.Validate(map[string]any{"id": "x", "kind": "k", "subject": "s", "status": "awaiting_approval", "attempts": 0, "max_attempts": 1, "updated_at": "t"}); err == nil {
		t.Error("awaiting_approval validates as a dek job status")
	}
	if err := s.Validate(map[string]any{"id": "x"}); err == nil {
		t.Error("a job without its required fields validates")
	}
}

func TestRunProjectsOntoJob(t *testing.T) {
	s := schema(t, "Job")
	fail := &core.Failure{Code: core.CodeRunFailed, Message: "boom"}
	exp := at.Add(time.Hour)
	retry := at.Add(time.Minute)
	for _, tc := range []struct {
		name   string
		mut    func(*core.Run)
		status records.JobStatus
	}{
		{"queued", func(r *core.Run) { r.Status = core.RunQueued }, records.JobQueued},
		{"retrying", func(r *core.Run) { r.Status = core.RunQueued; r.NextRetryAt = &retry }, records.JobRetrying},
		{"running", func(r *core.Run) { r.Status = core.RunRunning }, records.JobRunning},
		{"awaiting approval is waiting", func(r *core.Run) {
			r.Status = core.RunAwaitingApproval
			r.WaitingFor = core.WaitingForApproval("ap_01")
			r.ExpiresAt = &exp
		}, records.JobWaiting},
		{"succeeded", func(r *core.Run) { r.Status = core.RunSucceeded; r.Result = map[string]any{"n": 3} }, records.JobSucceeded},
		{"failed", func(r *core.Run) { r.Status = core.RunFailed; r.Failure = fail }, records.JobFailed},
		{"cancelled", func(r *core.Run) {
			r.Status = core.RunCancelled
			r.Failure = &core.Failure{Code: core.CodeRunCancelled, Message: "cancelled"}
		}, records.JobCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(core.RunRunning)
			tc.mut(&r)
			j := records.JobOf(r)
			doc := validate(t, s, j)
			if j.Status != tc.status || doc["status"] != string(tc.status) {
				t.Errorf("status = %s, want %s", j.Status, tc.status)
			}
			if doc["id"] != "run_01" || doc["kind"] != "flo.run" || doc["subject"] != "flow/triage" {
				t.Errorf("identity = %v %v %v", doc["id"], doc["kind"], doc["subject"])
			}
		})
	}
}

func TestJobWaitingForAndOutcome(t *testing.T) {
	r := run(core.RunAwaitingApproval)
	r.WaitingFor = core.WaitingForApproval("ap_01")
	j := records.JobOf(r)
	if j.WaitingFor == nil || *j.WaitingFor != "flo.approval.ap_01" {
		t.Errorf("waiting_for = %v", j.WaitingFor)
	}
	// only a parked run waits for anything
	r.Status = core.RunRunning
	if records.JobOf(r).WaitingFor != nil {
		t.Error("a running run carries waiting_for")
	}
	r = run(core.RunFailed)
	r.Failure = &core.Failure{Code: core.CodeBudgetExceeded, Message: "usd"}
	out, _ := json.Marshal(records.JobOf(r).Outcome)
	if string(out) != `{"failure":{"code":"budget_exceeded","message":"usd"}}` {
		t.Errorf("outcome = %s", out)
	}
	if records.JobOf(run(core.RunSucceeded)).Outcome != nil {
		t.Error("a run without a result has an outcome")
	}
}

func approval() core.Approval {
	decided := at.Add(time.Minute)
	return core.Approval{
		ID: "ap_01", Tenant: "default", Run: "run_01", StepID: "approve", Kind: core.ApprovalItems,
		Version: 4, Status: core.ApprovalOpen, CreatedAt: at,
		Items: []core.ApprovalItem{
			{ID: "m1", Action: "archive", Capability: "mail.archive", Args: map[string]any{"id": "m1"}, Digest: "sha256:abc"},
			{ID: "m2", Why: "newsletter", Decision: &core.ItemDecision{Verdict: core.VerdictApprove, By: "jad", At: decided, Reason: "ok"}},
			{ID: "m3", Action: "label", Decision: &core.ItemDecision{Verdict: core.VerdictReject, By: "jad", At: decided, Reason: "no"}},
			{ID: "m4", Action: "label", Decision: &core.ItemDecision{Verdict: core.VerdictCorrect, By: "jad", At: decided, Correction: "x"}},
			{ID: "m5", Action: "label", Decision: &core.ItemDecision{Verdict: core.VerdictAuto, By: core.DeciderTrust, At: at}},
			{ID: "m6", Action: "label", Decision: &core.ItemDecision{Verdict: core.VerdictReject, By: core.DeciderEngine, At: decided, Reason: core.DecisionReasonTimeout}},
		},
	}
}

func TestItemsProjectOntoProposals(t *testing.T) {
	s := schema(t, "Proposal")
	props := records.ProposalsOf(approval(), "noor")
	if len(props) != 6 {
		t.Fatalf("proposals = %d", len(props))
	}
	want := []struct{ id, status, subject, title string }{
		{"ap_01/m1", "pending", "sha256:abc", "archive"},
		{"ap_01/m2", "approved", "m2", "m2"},
		{"ap_01/m3", "rejected", "m3", "label"},
		{"ap_01/m4", "approved", "m4", "label"},
		{"ap_01/m5", "approved", "m5", "label"},
		{"ap_01/m6", "rejected", "m6", "label"},
	}
	for i, p := range props {
		doc := validate(t, s, p)
		w := want[i]
		if p.ID != w.id || p.Status != w.status || p.Subject != w.subject || p.Title != w.title {
			t.Errorf("proposal %d = %+v, want %+v", i, p, w)
		}
		chain := doc["chain"].([]any)
		if len(chain) != 1 || chain[0].(map[string]any)["status"] != w.status || chain[0].(map[string]any)["level"] != "owner" {
			t.Errorf("%s: chain = %v", w.id, chain)
		}
		if doc["version"].(float64) != 4 || doc["proposed_by"] != "noor" || doc["kind"] != "apply" || doc["target_level"] != "owner" {
			t.Errorf("%s: %v", w.id, doc)
		}
	}
	if props[0].CurrentLevel == nil || *props[0].CurrentLevel != "owner" || props[1].CurrentLevel != nil {
		t.Error("current_level is set for a pending proposal only")
	}
	if props[0].Capability == nil || *props[0].Capability != "mail.archive" || props[1].Capability != nil {
		t.Error("capability")
	}
	lvl := props[1].Chain[0]
	if lvl.By == nil || *lvl.By != "jad" || lvl.Reason == nil || *lvl.Reason != "ok" || lvl.At == nil {
		t.Errorf("decided level = %+v", lvl)
	}
	if props[4].Chain[0].By == nil || *props[4].Chain[0].By != "trust" {
		t.Error("an auto item is decided by trust")
	}
}

func TestProposedByDefaultsToFlo(t *testing.T) {
	if p := records.ProposalOf(approval(), 0, ""); p.ProposedBy != "flo" {
		t.Errorf("proposed_by = %q", p.ProposedBy)
	}
}

func TestDryRunProjectsOntoPlan(t *testing.T) {
	s := schema(t, "Plan")
	step := core.StepRun{
		ID: "stp_01", UpdatedAt: at,
		Plan: &core.Plan{Capability: "mail.send", Args: map[string]any{"to": "a@b"}, IdempotencyKey: "k1", Rendered: []string{"mail", "send"}},
	}
	p, ok := records.PlanOfStep(step)
	if !ok {
		t.Fatal("no plan")
	}
	doc := validate(t, s, p)
	if doc["id"] != "stp_01" || doc["capability"] != "mail.send" || doc["idempotency_key"] != "k1" {
		t.Errorf("plan = %v", doc)
	}
	// every required key is present even when the plan says nothing
	bare := records.PlanOf("p1", core.Plan{Capability: "x.y"}, at)
	doc = validate(t, s, bare)
	for _, k := range []string{"args", "rendered", "idempotency_key"} {
		if _, has := doc[k]; !has {
			t.Errorf("key %s missing", k)
		}
	}
	if _, ok := records.PlanOfStep(core.StepRun{}); ok {
		t.Error("a step without a plan projects a plan")
	}
}
