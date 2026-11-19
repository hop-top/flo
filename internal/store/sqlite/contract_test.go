package sqlite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/store/sqlite"
	specrun "hop.top/flo/spec/run"
)

// The stores are tested through the ports, against the fakes of coretest
// (the reference semantics) and against SQLite: every contract test runs on
// both, so the real store cannot drift from what the rest of the engine was
// tested with.

const T = core.DefaultTenant

var bg = context.Background()

type bundle struct {
	clock     *coretest.Clock
	ids       *coretest.IDs
	runs      core.RunStore
	state     core.StateStore
	flows     core.FlowStore
	approvals core.ApprovalStore
	grants    core.GrantStore
	receipts  core.ReceiptStore
	outputs   core.OutputStore
	triggers  core.TriggerStore
	groups    core.Groups
}

func fakeBundle(*testing.T) bundle {
	clock := coretest.NewClock(time.Time{})
	db := coretest.NewDB(clock)
	return bundle{clock: clock, ids: &coretest.IDs{}, runs: db.Runs, state: db.State, flows: db.Flows, approvals: db.Approvals,
		grants: db.Grants, receipts: db.Receipts, outputs: db.Outputs, triggers: db.Triggers, groups: db.Groups}
}

func newStore(t *testing.T, root string, clock core.Clock) *sqlite.Store {
	t.Helper()
	s := sqlite.Open(root, sqlite.Options{Clock: clock, NoSync: true})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sqliteBundle(t *testing.T) bundle {
	clock := coretest.NewClock(time.Time{})
	s := newStore(t, t.TempDir(), clock)
	return bundle{clock: clock, ids: &coretest.IDs{}, runs: s.Runs, state: s.State, flows: s.Flows, approvals: s.Approvals,
		grants: s.Grants, receipts: s.Receipts, outputs: s.Outputs, triggers: s.Triggers, groups: s.Groups}
}

func both(t *testing.T, fn func(t *testing.T, b bundle)) {
	t.Helper()
	t.Run("fake", func(t *testing.T) { fn(t, fakeBundle(t)) })
	t.Run("sqlite", func(t *testing.T) { fn(t, sqliteBundle(t)) })
}

func (b bundle) newRun(t *testing.T, key string) core.Run {
	t.Helper()
	r, _, err := b.runs.CreateRun(bg, core.NewRun{ID: core.NewRunID(b.ids), Tenant: T, Flow: core.FlowRef{Name: "w", Version: "1", Hash: "h"}, IdemKey: key, Group: "w", Admitted: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunStoreIdempotencyAndCAS(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		a := b.newRun(t, "k")
		got, created, err := b.runs.CreateRun(bg, core.NewRun{ID: core.NewRunID(b.ids), Tenant: T, Flow: a.Flow, IdemKey: "k"})
		if err != nil || created || got.ID != a.ID {
			t.Fatalf("idem create = %v %v %v", got.ID, created, err)
		}
		if r, ok, _ := b.runs.LookupIdem(bg, T, "w", "k"); !ok || r.ID != a.ID {
			t.Error("LookupIdem")
		}
		if _, ok, _ := b.runs.LookupIdem(bg, T, "w", ""); ok {
			t.Error("an empty key never matches")
		}
		if _, ok, _ := b.runs.LookupIdem(bg, T, "other", "k"); ok {
			t.Error("the key is per flow")
		}
		if _, _, err := b.runs.CreateRun(bg, core.NewRun{ID: a.ID, Tenant: T, Flow: a.Flow}); !core.IsCode(err, core.CodeDuplicate) {
			t.Errorf("duplicate id: %v", err)
		}
		if _, err := b.runs.GetRun(bg, T, "run_nope"); !core.IsCode(err, core.CodeRunNotFound) {
			t.Error("not found")
		}
		if _, err := b.runs.GetRun(bg, "other", a.ID); !core.IsCode(err, core.CodeRunNotFound) {
			t.Error("tenants are isolated")
		}
		running := core.RunRunning
		if _, err := b.runs.UpdateRun(bg, T, a.ID, []core.RunStatus{core.RunRunning}, core.RunPatch{Status: &running}); !core.IsCode(err, core.CodeRunStateConflict) {
			t.Error("expect mismatch must conflict")
		}
		up, err := b.runs.UpdateRun(bg, T, a.ID, []core.RunStatus{core.RunQueued}, core.RunPatch{Status: &running})
		if err != nil || up.Status != core.RunRunning {
			t.Fatalf("update = %v %v", up.Status, err)
		}
		done := core.RunSucceeded
		if _, err := b.runs.UpdateRun(bg, T, a.ID, nil, core.RunPatch{Status: &done}); !core.IsCode(err, core.CodeUsage) {
			t.Error("a patch must not set a terminal status")
		}
		up.Vars = map[string]any{"x": 1}
		if got2, _ := b.runs.GetRun(bg, T, a.ID); got2.Vars != nil {
			t.Error("returned records must be copies")
		}
	})
}

func TestRunPatchFieldsAndCancel(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		r := b.newRun(t, "")
		at := coretest.Epoch.Add(time.Hour)
		wait, n, usage, task, adm := "flo.approval.ap_1", 2, core.Usage{Cost: core.Cost{USD: 1.5, Tokens: 7}, Calls: 3}, "T-1", false
		st := core.RunAwaitingApproval
		up, err := b.runs.UpdateRun(bg, T, r.ID, nil, core.RunPatch{
			Status: &st, WaitingFor: &wait, ExpiresAt: &at, Attempts: &n, Usage: &usage, TaskRef: &task, Admitted: &adm, StartedAt: &at,
			StateSnapshot: map[string]any{"k": int64(1), "f": 1.5},
		})
		if err != nil {
			t.Fatal(err)
		}
		if up.WaitingFor != wait || up.ExpiresAt == nil || !up.ExpiresAt.Equal(at) || up.Attempts != 2 || up.Usage != usage || up.TaskRef != "T-1" || up.Admitted {
			t.Errorf("patch = %+v", up)
		}
		if up.StateSnapshot["k"] != int64(1) || up.StateSnapshot["f"] != 1.5 {
			t.Errorf("snapshot = %#v", up.StateSnapshot)
		}
		clear, zero := "", time.Time{}
		up, _ = b.runs.UpdateRun(bg, T, r.ID, nil, core.RunPatch{WaitingFor: &clear, ExpiresAt: &zero})
		if up.WaitingFor != "" || up.ExpiresAt != nil {
			t.Errorf("zero clears: %+v", up)
		}
		c, err := b.runs.RequestCancel(bg, T, r.ID)
		if err != nil || !c.CancelRequested || c.Status != core.RunAwaitingApproval {
			t.Errorf("cancel = %+v %v", c, err)
		}
		if _, err := b.runs.RequestCancel(bg, T, "run_nope"); !core.IsCode(err, core.CodeRunNotFound) {
			t.Error("cancel of unknown run")
		}
		fin, _ := b.state.FinishRun(bg, T, r.ID, core.RunFinish{Status: core.RunCancelled})
		again, _ := b.runs.RequestCancel(bg, T, r.ID)
		if again.Status != core.RunCancelled || !again.UpdatedAt.Equal(fin.UpdatedAt) {
			t.Error("a terminal run changes nothing")
		}
		if _, err := b.runs.UpdateRun(bg, T, r.ID, nil, core.RunPatch{TaskRef: &task}); !core.IsCode(err, core.CodeRunStateConflict) {
			t.Error("a terminal run is never patched")
		}
	})
}

func TestListRunsFilterAndOrder(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		var ids []core.RunID
		for i := 0; i < 4; i++ {
			b.clock.Advance(time.Minute)
			ids = append(ids, b.newRun(t, "").ID)
		}
		_, _ = b.state.FinishRun(bg, T, ids[0], core.RunFinish{Status: core.RunFailed})
		all, _ := b.runs.ListRuns(bg, T, core.RunFilter{})
		if len(all) != 4 || all[0].ID != ids[3] || all[3].ID != ids[0] {
			t.Errorf("newest first: %v", all)
		}
		got, _ := b.runs.ListRuns(bg, T, core.RunFilter{Statuses: []core.RunStatus{core.RunFailed}})
		if len(got) != 1 || got[0].ID != ids[0] {
			t.Errorf("status filter: %v", got)
		}
		got, _ = b.runs.ListRuns(bg, T, core.RunFilter{Since: coretest.Epoch.Add(3 * time.Minute), Limit: 5})
		if len(got) != 2 {
			t.Errorf("since: %d", len(got))
		}
		if got, _ = b.runs.ListRuns(bg, T, core.RunFilter{Flow: "w", Group: "w", Limit: 1}); len(got) != 1 {
			t.Errorf("limit: %d", len(got))
		}
		if got, _ = b.runs.ListRuns(bg, T, core.RunFilter{Flow: "x"}); len(got) != 0 {
			t.Error("flow filter")
		}
	})
}

func TestStateStagedCommitsOnlyOnSuccess(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		for _, tc := range []struct {
			status core.RunStatus
		}{{core.RunSucceeded}, {core.RunFailed}, {core.RunCancelled}} {
			r := b.newRun(t, "")
			if err := b.state.Stage(bg, T, r.ID, "seen-"+string(tc.status), []any{"a"}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.state.FinishRun(bg, T, r.ID, core.RunFinish{Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			if st, _ := b.state.Staged(bg, T, r.ID); len(st) != 0 {
				t.Errorf("%s: staged writes survive finish", tc.status)
			}
			if _, err := b.state.FinishRun(bg, T, r.ID, core.RunFinish{Status: tc.status}); err != nil {
				t.Errorf("repeat finish: %v", err)
			}
			other := core.RunFailed
			if tc.status == core.RunFailed {
				other = core.RunSucceeded
			}
			if _, err := b.state.FinishRun(bg, T, r.ID, core.RunFinish{Status: other}); !core.IsCode(err, core.CodeRunStateConflict) {
				t.Errorf("conflicting finish: %v", err)
			}
			if err := b.state.Stage(bg, T, r.ID, "late", 1); !core.IsCode(err, core.CodeRunStateConflict) {
				t.Errorf("stage after finish: %v", err)
			}
		}
		snap, _ := b.state.Snapshot(bg, T, "w")
		if len(snap) != 1 || snap["seen-succeeded"] == nil {
			t.Errorf("only the successful run commits: %v", snap)
		}
	})
}

func TestStateFailAndCancelLeaveStateUntouched(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		ok := b.newRun(t, "")
		_ = b.state.Stage(bg, T, ok.ID, "k", "v1")
		_, _ = b.state.FinishRun(bg, T, ok.ID, core.RunFinish{Status: core.RunSucceeded})
		for _, st := range []core.RunStatus{core.RunFailed, core.RunCancelled} {
			bad := b.newRun(t, "")
			_ = b.state.Stage(bg, T, bad.ID, "k", "v2")
			_, _ = b.state.FinishRun(bg, T, bad.ID, core.RunFinish{Status: st})
		}
		snap, _ := b.state.Snapshot(bg, T, "w")
		if snap["k"] != "v1" {
			t.Errorf("state = %v", snap)
		}
		if _, err := b.state.FinishRun(bg, T, ok.ID, core.RunFinish{Status: core.RunQueued}); !core.IsCode(err, core.CodeUsage) {
			t.Error("finish needs a terminal status")
		}
	})
}

func TestStateStageReplacesAndLimit(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		r := b.newRun(t, "")
		_ = b.state.Stage(bg, T, r.ID, "k", "a")
		_ = b.state.Stage(bg, T, r.ID, "k", "b")
		if st, _ := b.state.Staged(bg, T, r.ID); len(st) != 1 || st["k"] != "b" {
			t.Errorf("staged = %v", st)
		}
		if err := b.state.Stage(bg, T, r.ID, "k", strings.Repeat("x", core.MaxStateValueBytes)); !core.IsCode(err, core.CodeStateTooLarge) {
			t.Errorf("err = %v", err)
		}
		if err := b.state.Stage(bg, T, r.ID, "ok", strings.Repeat("x", core.MaxStateValueBytes-2)); err != nil {
			t.Errorf("exactly at the limit: %v", err)
		}
		if err := b.state.Stage(bg, T, "run_nope", "k", 1); !core.IsCode(err, core.CodeRunNotFound) {
			t.Errorf("unknown run: %v", err)
		}
	})
}

func TestStepRecordsKeepOrderAndNeverRewriteTerminal(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		r := b.newRun(t, "")
		for _, id := range []string{"b", "a"} {
			_ = b.runs.PutStep(bg, T, core.StepRun{ID: core.StepRunID("s-" + id), Run: r.ID, StepID: id, Status: core.StepRunning})
		}
		_ = b.runs.PutStep(bg, T, core.StepRun{ID: "s-b", Run: r.ID, StepID: "b", Status: core.StepSucceeded, Output: 1})
		_ = b.runs.PutStep(bg, T, core.StepRun{ID: "s-b", Run: r.ID, StepID: "b", Status: core.StepFailed})
		steps, _ := b.runs.ListSteps(bg, T, r.ID)
		if len(steps) != 2 || steps[0].StepID != "b" || steps[0].Status != core.StepSucceeded || steps[1].StepID != "a" {
			t.Errorf("steps = %+v", steps)
		}
		if st, err := b.runs.GetStep(bg, T, r.ID, "s-a"); err != nil || st.StepID != "a" {
			t.Errorf("GetStep = %v %v", st.StepID, err)
		}
		if _, err := b.runs.GetStep(bg, T, r.ID, "s-zz"); !core.IsCode(err, core.CodeRunNotFound) {
			t.Error("unknown step")
		}
		if err := b.runs.PutStep(bg, T, core.StepRun{ID: "s-x", Run: "run_nope"}); !core.IsCode(err, core.CodeRunNotFound) {
			t.Error("step of unknown run")
		}
		if _, err := b.runs.ListSteps(bg, T, "run_nope"); !core.IsCode(err, core.CodeRunNotFound) {
			t.Error("steps of unknown run")
		}
		empty := b.newRun(t, "")
		if s, err := b.runs.ListSteps(bg, T, empty.ID); err != nil || len(s) != 0 {
			t.Errorf("empty run: %v %v", s, err)
		}
	})
}

func TestFlowStoreVersions(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		put := func(hash string) core.FlowVersion {
			v, _, err := b.flows.PutVersion(bg, core.NewFlowVersion{Tenant: T, Name: "w", Version: "1", Hash: hash, Source: []byte(hash)})
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		a := put("h1")
		same, created, _ := b.flows.PutVersion(bg, core.NewFlowVersion{Tenant: T, Name: "w", Version: "1", Hash: "h1"})
		if created || same.Seq != a.Seq || string(same.Source) != "h1" {
			t.Error("same bytes must be one version")
		}
		v2 := put("h2")
		if a.Seq != 1 || v2.Seq != 2 {
			t.Errorf("seq %d %d", a.Seq, v2.Seq)
		}
		if _, err := b.flows.Active(bg, T, "w"); !core.IsCode(err, core.CodeFlowNotActive) {
			t.Error("draft is not active")
		}
		if _, err := b.flows.Active(bg, T, "nope"); !core.IsCode(err, core.CodeFlowNotFound) {
			t.Error("unknown flow")
		}
		_, _ = b.flows.SetState(bg, T, "w", "h1", core.FlowActive, time.Time{})
		_, _ = b.flows.SetState(bg, T, "w", "h2", core.FlowActive, time.Time{})
		act, _ := b.flows.Active(bg, T, "w")
		vs, _ := b.flows.ListVersions(bg, T, "w")
		if act.Hash != "h2" || act.ActivatedAt == nil || vs[0].Hash != "h2" || vs[1].State != core.FlowRetired {
			t.Errorf("active %s, versions %+v", act.Hash, vs)
		}
		if _, err := b.flows.SetState(bg, T, "w", "h1", core.FlowPaused, time.Time{}); !core.IsCode(err, core.CodeFlowNotActive) {
			t.Error("only an active version pauses")
		}
		if _, err := b.flows.SetState(bg, T, "w", "h2", core.FlowPaused, time.Time{}); err != nil {
			t.Fatal(err)
		}
		sums, _ := b.flows.ListFlows(bg, T)
		if len(sums) != 1 || !sums[0].Paused || sums[0].Active != nil || sums[0].Latest.Hash != "h2" {
			t.Errorf("summary = %+v", sums)
		}
		if g, _ := b.flows.GetVersion(bg, T, "w", ""); g.Hash != "h2" {
			t.Error("latest when ref is empty")
		}
		if g, _ := b.flows.GetVersion(bg, T, "w", "h1"); g.Hash != "h1" {
			t.Error("by hash")
		}
		if g, err := b.flows.GetVersion(bg, T, "w", "1"); err != nil || g.Hash != "h2" {
			t.Errorf("by document version takes the newest: %v %v", g.Hash, err)
		}
		if _, err := b.flows.GetVersion(bg, T, "w", "zzz"); !core.IsCode(err, core.CodeFlowNotFound) {
			t.Error("unknown ref")
		}
		if _, err := b.flows.SetState(bg, T, "w", "h1", core.FlowRetired, time.Time{}); !core.IsCode(err, core.CodeUsage) {
			t.Error("retired is not settable")
		}
	})
}

func TestApprovalStoreCAS(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		a, created, err := b.approvals.CreateApproval(bg, core.Approval{ID: "ap_1", Tenant: T, Run: "r", StepRun: "s", Items: []core.ApprovalItem{{ID: "1", Args: map[string]any{"n": int64(3)}}}})
		if err != nil || !created || a.Version != 1 || a.Status != core.ApprovalOpen {
			t.Fatalf("create = %+v %v", a, err)
		}
		if _, created, _ := b.approvals.CreateApproval(bg, core.Approval{ID: "ap_2", Tenant: T, Run: "r", StepRun: "s"}); created {
			t.Error("idempotent on (run, step run)")
		}
		if _, _, err := b.approvals.CreateApproval(bg, core.Approval{ID: "ap_1", Tenant: T, Run: "r", StepRun: "other"}); !core.IsCode(err, core.CodeDuplicate) {
			t.Errorf("duplicate id: %v", err)
		}
		a.Status = core.ApprovalDecided
		up, err := b.approvals.UpdateApproval(bg, a)
		if err != nil || up.Version != 2 {
			t.Fatalf("update = %v %v", up.Version, err)
		}
		_, err = b.approvals.UpdateApproval(bg, a)
		var ce *core.Error
		if !core.IsCode(err, core.CodeApprovalConflict) {
			t.Fatalf("stale update: %v", err)
		}
		_ = ce
		if _, err := b.approvals.UpdateApproval(bg, core.Approval{ID: "ap_9", Tenant: T}); !core.IsCode(err, core.CodeApprovalNotFound) {
			t.Error("unknown approval")
		}
		got, _ := b.approvals.GetApproval(bg, T, "ap_1")
		if got.Version != 2 || got.Status != core.ApprovalDecided {
			t.Errorf("get = %+v", got)
		}
		if _, err := b.approvals.GetApproval(bg, T, "ap_x"); !core.IsCode(err, core.CodeApprovalNotFound) {
			t.Error("get unknown")
		}
		_, _, _ = b.approvals.CreateApproval(bg, core.Approval{ID: "ap_3", Tenant: T, Run: "r2", StepRun: "s", StepID: "x"})
		open, _ := b.approvals.ListApprovals(bg, T, core.ApprovalFilter{Status: core.ApprovalOpen})
		if len(open) != 1 || open[0].ID != "ap_3" {
			t.Errorf("open = %+v", open)
		}
		all, _ := b.approvals.ListApprovals(bg, T, core.ApprovalFilter{})
		if len(all) != 2 || all[0].ID != "ap_3" {
			t.Errorf("newest first = %+v", all)
		}
		if r, _ := b.approvals.ListApprovals(bg, T, core.ApprovalFilter{Run: "r"}); len(r) != 1 {
			t.Error("run filter")
		}
	})
}

func TestGrantsReceiptsOutputs(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		g := core.Grant{Tenant: T, Profile: "p", Capability: "mail.send"}
		if ch, _ := b.grants.SetGrant(bg, g); !ch {
			t.Error("first set changes")
		}
		if ch, _ := b.grants.SetGrant(bg, g); ch {
			t.Error("second set is a no-op")
		}
		_, _ = b.grants.SetGrant(bg, core.Grant{Tenant: T, Profile: "a", Capability: "z"})
		_, _ = b.grants.SetGrant(bg, core.Grant{Tenant: T, Profile: "a", Capability: "b", Source: core.GrantProvisioned})
		all, _ := b.grants.ListGrants(bg, T, "")
		if len(all) != 3 || all[0].Capability != "b" || all[0].Source != core.GrantProvisioned || all[1].Capability != "z" || all[2].Profile != "p" || all[2].Source != core.GrantLocal || all[2].CreatedAt.IsZero() {
			t.Errorf("grants = %+v", all)
		}
		if one, _ := b.grants.ListGrants(bg, T, "p"); len(one) != 1 {
			t.Error("profile filter")
		}
		if ch, _ := b.grants.UnsetGrant(bg, T, "p", "mail.send"); !ch {
			t.Error("unset changes")
		}
		if ch, _ := b.grants.UnsetGrant(bg, T, "p", "mail.send"); ch {
			t.Error("unset again is a no-op")
		}
		if other, _ := b.grants.ListGrants(bg, "other", ""); len(other) != 0 {
			t.Error("grants are per tenant")
		}

		r1 := core.Receipt{ID: "rcp_1", Tenant: T, Run: "r", Decision: core.AdmissionDeferred}
		_ = b.receipts.PutReceipt(bg, r1)
		_ = b.receipts.PutReceipt(bg, core.Receipt{ID: "rcp_2", Tenant: T, Run: "r"})
		r1.Decision = core.AdmissionAllow
		_ = b.receipts.PutReceipt(bg, r1)
		rs, _ := b.receipts.ListReceipts(bg, T, "r")
		if len(rs) != 2 || rs[0].ID != "rcp_1" || rs[0].Decision != core.AdmissionAllow {
			t.Errorf("receipts = %+v", rs)
		}
		if _, ok, _ := b.receipts.GetReceipt(bg, T, "rcp_x"); ok {
			t.Error("unknown receipt")
		}
		if got, ok, _ := b.receipts.GetReceipt(bg, T, "rcp_1"); !ok || got.Decision != core.AdmissionAllow {
			t.Error("get receipt")
		}

		now := b.clock.Now()
		for i, id := range []core.OutputID{"out_1", "out_2", "out_3"} {
			_ = b.outputs.PutOutput(bg, core.Output{ID: id, Tenant: T, Run: "r", Kind: core.OutputBrief, CreatedAt: now.Add(time.Duration(i) * time.Hour), Items: []any{map[string]any{"n": int64(1)}}})
		}
		_ = b.outputs.PutOutput(bg, core.Output{ID: "out_4", Tenant: T, Run: "q", Kind: core.OutputFindings})
		if got, err := b.outputs.GetOutput(bg, T, "out_2"); err != nil || got.ID != "out_2" {
			t.Error("get output")
		}
		if _, err := b.outputs.GetOutput(bg, T, "out_x"); !core.IsCode(err, core.CodeOutputNotFound) {
			t.Error("unknown output")
		}
		if l, _ := b.outputs.ListOutputs(bg, T, core.OutputFilter{Run: "r", Kind: core.OutputBrief, Since: now.Add(time.Hour), Limit: 5}); len(l) != 2 || l[0].ID != "out_3" {
			t.Errorf("filtered = %+v", l)
		}
	})
}

// A provisioned grant carries an end and is refreshed by the next set: its end
// moves, and it takes over a local grant of the same capability; a local set
// never moves one.
func TestProvisionedGrantsEndAndRefresh(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		end1 := b.clock.Now().Add(24 * time.Hour).UTC()
		end2 := end1.Add(24 * time.Hour)
		prov := func(end time.Time) core.Grant {
			return core.Grant{Tenant: T, Profile: "p", Capability: "billing.pay", Source: core.GrantProvisioned, ExpiresAt: &end}
		}
		if ch, err := b.grants.SetGrant(bg, prov(end1)); err != nil || !ch {
			t.Fatalf("first provision: %v %v", ch, err)
		}
		if ch, _ := b.grants.SetGrant(bg, prov(end1)); ch {
			t.Error("the same end is a no-op")
		}
		if ch, _ := b.grants.SetGrant(bg, prov(end2)); !ch {
			t.Error("a later end is a change")
		}
		if ch, _ := b.grants.SetGrant(bg, core.Grant{Tenant: T, Profile: "p", Capability: "billing.pay"}); ch {
			t.Error("a local set moved a provisioned grant")
		}
		got, _ := b.grants.ListGrants(bg, T, "p")
		if len(got) != 1 || got[0].Source != core.GrantProvisioned || got[0].ExpiresAt == nil || !got[0].ExpiresAt.Equal(end2) {
			t.Fatalf("grants = %+v", got)
		}
		if !got[0].Live(end2.Add(-time.Second)) || got[0].Live(end2) {
			t.Error("a grant is live until its end, exclusive")
		}
		_, _ = b.grants.SetGrant(bg, core.Grant{Tenant: T, Profile: "q", Capability: "x.y"})
		if ch, _ := b.grants.SetGrant(bg, core.Grant{Tenant: T, Profile: "q", Capability: "x.y", Source: core.GrantProvisioned}); !ch {
			t.Error("a provisioned grant takes over a local one")
		}
		if q, _ := b.grants.ListGrants(bg, T, "q"); q[0].Source != core.GrantProvisioned || q[0].ExpiresAt != nil {
			t.Errorf("takeover = %+v", q)
		}
	})
}

// A receipt keeps what binds a step to an external approval.
func TestReceiptKeepsProposalKeyAndCorrection(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		r := core.Receipt{ID: "rcp_b", Tenant: T, Run: "r", Decision: core.AdmissionAllow, Backend: "bos",
			Proposal: "agent_proposals_1", ExecutionKey: "exec_1", CorrectedInput: map[string]any{"n": int64(2)}}
		_ = b.receipts.PutReceipt(bg, r)
		got, ok, _ := b.receipts.GetReceipt(bg, T, "rcp_b")
		if !ok || got.Proposal != "agent_proposals_1" || got.ExecutionKey != "exec_1" || got.CorrectedInput["n"] != int64(2) {
			t.Fatalf("receipt = %+v", got)
		}
	})
}

func TestOutputPrune(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		old := b.clock.Now().Add(-100 * 24 * time.Hour)
		_ = b.outputs.PutOutput(bg, core.Output{ID: "out_1", Tenant: T, CreatedAt: old})
		_ = b.outputs.PutOutput(bg, core.Output{ID: "out_2", Tenant: T})
		n, _ := b.outputs.PruneOutputs(bg, T, b.clock.Now().Add(-core.OutputRetention))
		left, _ := b.outputs.ListOutputs(bg, T, core.OutputFilter{})
		if n != 1 || len(left) != 1 || left[0].ID != "out_2" {
			t.Errorf("pruned %d, left %+v", n, left)
		}
		b.clock.Advance(core.OutputRetention + time.Hour)
		if n, _ = b.outputs.PruneOutputs(bg, T, b.clock.Now().Add(-core.OutputRetention)); n != 1 {
			t.Errorf("a 90 day old output goes: %d", n)
		}
	})
}

func TestTriggerStore(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		_ = b.triggers.PutTrigger(bg, core.TriggerRecord{ID: "trg_1", Tenant: T, Flow: "w", Index: 1, Spec: specrun.Trigger{Kind: specrun.TriggerSchedule, RRule: "FREQ=DAILY"}, State: core.TriggerActive})
		_ = b.triggers.PutTrigger(bg, core.TriggerRecord{ID: "trg_0", Tenant: T, Flow: "w", Index: 0})
		_ = b.triggers.PutTrigger(bg, core.TriggerRecord{ID: "trg_2", Tenant: T, Flow: "a"})
		ls, _ := b.triggers.ListTriggers(bg, T, "")
		if len(ls) != 3 || ls[0].ID != "trg_2" || ls[1].ID != "trg_0" || ls[2].ID != "trg_1" {
			t.Errorf("order = %+v", ls)
		}
		got, _ := b.triggers.GetTrigger(bg, T, "trg_1")
		if got.Spec.RRule != "FREQ=DAILY" || got.UpdatedAt.IsZero() {
			t.Errorf("get = %+v", got)
		}
		if _, err := b.triggers.GetTrigger(bg, T, "trg_x"); !core.IsCode(err, core.CodeTriggerNotFound) {
			t.Error("unknown trigger")
		}
		at := coretest.Epoch.Add(time.Hour)
		ok, _ := b.triggers.AdvanceTrigger(bg, T, "trg_1", nil, &at, "job_1")
		again, _ := b.triggers.AdvanceTrigger(bg, T, "trg_1", nil, &at, "job_2")
		if !ok || again {
			t.Errorf("first %v second %v: only one worker may enqueue the next fire", ok, again)
		}
		if moved, _ := b.triggers.AdvanceTrigger(bg, T, "trg_1", &at, nil, ""); !moved {
			t.Error("advance from the stored instant")
		}
		if _, err := b.triggers.AdvanceTrigger(bg, T, "trg_x", nil, &at, "j"); !core.IsCode(err, core.CodeTriggerNotFound) {
			t.Error("advance unknown")
		}
		_ = b.triggers.DeleteTriggers(bg, T, "w")
		if ls, _ = b.triggers.ListTriggers(bg, T, ""); len(ls) != 1 {
			t.Errorf("after delete = %+v", ls)
		}

		for i, o := range []core.FireOutcome{core.FireStarted, core.FireMissed, core.FireSkipped} {
			_ = b.triggers.RecordFire(bg, core.Fire{Tenant: T, Flow: "w", Trigger: "trg_1", Key: string(rune('a' + i)), Outcome: o})
		}
		_ = b.triggers.RecordFire(bg, core.Fire{Tenant: T, Flow: "z", Trigger: "trg_9", Key: "q"})
		fs, _ := b.triggers.ListFires(bg, T, "w", "", 0)
		if len(fs) != 3 || fs[0].Outcome != core.FireSkipped || fs[0].RecordedAt.IsZero() {
			t.Errorf("fires = %+v", fs)
		}
		if fs, _ = b.triggers.ListFires(bg, T, "", "trg_1", 2); len(fs) != 2 {
			t.Error("limit")
		}

		p := core.PlannedFire{Tenant: T, Flow: "c", Trigger: "trg_c", Key: "k2", At: coretest.Epoch.Add(2 * time.Hour), Payload: map[string]any{"n": int64(1)}}
		_ = b.triggers.PutPlannedFire(bg, p)
		p.Key, p.At = "k1", coretest.Epoch.Add(time.Hour)
		_ = b.triggers.PutPlannedFire(bg, p)
		p.At = coretest.Epoch.Add(3 * time.Hour)
		_ = b.triggers.PutPlannedFire(bg, p) // replaces k1
		pl, _ := b.triggers.ListPlannedFires(bg, T, "trg_c")
		if len(pl) != 2 || pl[0].Key != "k2" || pl[1].Key != "k1" {
			t.Errorf("planned = %+v", pl)
		}
		if g, ok, _ := b.triggers.GetPlannedFire(bg, T, "trg_c", "k1"); !ok || !g.At.Equal(p.At) {
			t.Error("get planned")
		}
		_ = b.triggers.DeletePlannedFire(bg, T, "trg_c", "k1")
		_ = b.triggers.DeletePlannedFire(bg, T, "trg_c", "k1") // absent is not an error
		if _, ok, _ := b.triggers.GetPlannedFire(bg, T, "trg_c", "k1"); ok {
			t.Error("deleted")
		}
	})
}

func admit(t *testing.T, b bundle, group string, max int, ov specrun.OnOverlap) (core.RunID, core.GroupDecision) {
	t.Helper()
	id := core.NewRunID(b.ids)
	d, err := b.groups.Admit(bg, core.GroupRequest{Tenant: T, Group: group, Max: max, OnOverlap: ov, Run: id})
	if err != nil {
		t.Fatal(err)
	}
	return id, d
}

func create(t *testing.T, b bundle, id core.RunID, group string, max int, d core.GroupDecision) {
	t.Helper()
	_, _, err := b.runs.CreateRun(bg, core.NewRun{ID: id, Tenant: T, Flow: core.FlowRef{Name: "w"}, Group: group, GroupMax: max, Admitted: d.Action == core.GroupRun})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGroupsSkipQueueCancelPrevious(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		// skip
		a, d := admit(t, b, "g1", 1, specrun.OverlapSkip)
		if d.Action != core.GroupRun {
			t.Fatalf("first = %v", d)
		}
		create(t, b, a, "g1", 1, d)
		if _, d = admit(t, b, "g1", 1, specrun.OverlapSkip); d.Action != core.GroupSkip {
			t.Errorf("skip = %v", d)
		}
		// a parked run still holds its slot
		parked := core.RunAwaitingApproval
		_, _ = b.runs.UpdateRun(bg, T, a, nil, core.RunPatch{Status: &parked})
		if _, d = admit(t, b, "g1", 1, specrun.OverlapSkip); d.Action != core.GroupSkip {
			t.Errorf("parked counts active: %v", d)
		}
		// terminal frees it
		_, _ = b.state.FinishRun(bg, T, a, core.RunFinish{Status: core.RunSucceeded})
		if _, d = admit(t, b, "g1", 1, specrun.OverlapSkip); d.Action != core.GroupRun {
			t.Errorf("after finish = %v", d)
		}

		// queue, FIFO release
		first, d := admit(t, b, "g2", 1, specrun.OverlapQueue)
		create(t, b, first, "g2", 1, d)
		q1, d := admit(t, b, "g2", 1, specrun.OverlapQueue)
		if d.Action != core.GroupQueue {
			t.Fatalf("queue = %v", d)
		}
		create(t, b, q1, "g2", 1, d)
		q2, d := admit(t, b, "g2", 1, specrun.OverlapQueue)
		create(t, b, q2, "g2", 1, d)
		if got, _ := b.groups.Release(bg, T, "g2"); len(got) != 0 {
			t.Errorf("full group promotes nothing: %v", got)
		}
		_, _ = b.state.FinishRun(bg, T, first, core.RunFinish{Status: core.RunFailed})
		got, _ := b.groups.Release(bg, T, "g2")
		if len(got) != 1 || got[0] != q1 {
			t.Errorf("oldest first: %v", got)
		}
		if r, _ := b.runs.GetRun(bg, T, q1); !r.Admitted {
			t.Error("promoted run is admitted")
		}
		if got, _ = b.groups.Release(bg, T, "g2"); len(got) != 0 {
			t.Errorf("slot taken: %v", got)
		}

		// cancel_previous
		old, d := admit(t, b, "g3", 1, specrun.OverlapCancelPrevious)
		create(t, b, old, "g3", 1, d)
		nw, d := admit(t, b, "g3", 1, specrun.OverlapCancelPrevious)
		if d.Action != core.GroupRun || len(d.Cancel) != 1 || d.Cancel[0] != old {
			t.Errorf("cancel_previous = %v", d)
		}
		create(t, b, nw, "g3", 1, d)

		// max 2
		x1, d := admit(t, b, "g4", 2, specrun.OverlapQueue)
		create(t, b, x1, "g4", 2, d)
		x2, d := admit(t, b, "g4", 2, specrun.OverlapQueue)
		create(t, b, x2, "g4", 2, d)
		if _, d = admit(t, b, "g4", 2, specrun.OverlapQueue); d.Action != core.GroupQueue {
			t.Errorf("max 2 full: %v", d)
		}
	})
}

func TestGroupReservations(t *testing.T) {
	both(t, func(t *testing.T, b bundle) {
		a, d := admit(t, b, "g", 1, specrun.OverlapQueue)
		if d.Action != core.GroupRun {
			t.Fatal(d)
		}
		again, _ := b.groups.Admit(bg, core.GroupRequest{Tenant: T, Group: "g", Max: 1, Run: a})
		if again.Action != core.GroupRun {
			t.Errorf("same run asks again: %v", again)
		}
		if _, d = admit(t, b, "g", 1, specrun.OverlapQueue); d.Action != core.GroupQueue {
			t.Errorf("reservation holds the slot: %v", d)
		}
		_ = b.groups.Abandon(bg, T, "g", a)
		if _, d = admit(t, b, "g", 1, specrun.OverlapQueue); d.Action != core.GroupRun {
			t.Errorf("abandoned: %v", d)
		}
		// that last reservation expires after a minute
		b.clock.Advance(coretest.ReservationTTL + time.Second)
		if _, d = admit(t, b, "g", 1, specrun.OverlapQueue); d.Action != core.GroupRun {
			t.Errorf("expired: %v", d)
		}
		// CreateRun consumes the reservation: the run is counted once
		c, d := admit(t, b, "h", 2, specrun.OverlapQueue)
		create(t, b, c, "h", 2, d)
		_, d = admit(t, b, "h", 2, specrun.OverlapQueue)
		if d.Action != core.GroupRun {
			t.Errorf("second of two: %v", d)
		}
		if _, d = admit(t, b, "h", 2, specrun.OverlapQueue); d.Action != core.GroupQueue {
			t.Errorf("third: %v", d)
		}
		_ = b.groups.Abandon(bg, T, "h", "run_unknown")
	})
}
