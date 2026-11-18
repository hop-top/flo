package jobs_test

import (
	"path/filepath"
	"sort"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/jobs"
	"hop.top/flo/internal/store/sqlite"
)

func TestReconcileRequeuesOrphans(t *testing.T) {
	root := t.TempDir()
	clock := coretest.NewClock(coretest.Epoch)
	st := sqlite.Open(root, sqlite.Options{Clock: clock, NoSync: true})
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Tenants.Ensure(bg, T); err != nil {
		t.Fatal(err)
	}
	q := openQueue(t, filepath.Join(root, "jobs.db"), clock, 0)

	mk := func(id core.RunID, status core.RunStatus, admitted bool) {
		if _, _, err := st.Runs.CreateRun(bg, core.NewRun{ID: id, Tenant: T, Flow: core.FlowRef{Name: "w"}, Admitted: admitted}); err != nil {
			t.Fatal(err)
		}
		if status != core.RunQueued {
			if _, err := st.Runs.UpdateRun(bg, T, id, nil, core.RunPatch{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("run_orphan_queued", core.RunQueued, true)
	mk("run_orphan_running", core.RunRunning, true)
	mk("run_has_job", core.RunRunning, true)
	mk("run_waiting_slot", core.RunQueued, false)
	mk("run_parked_open", core.RunAwaitingApproval, true)
	mk("run_parked_decided", core.RunAwaitingApproval, true)
	mk("run_done", core.RunQueued, true)
	if _, err := st.State.FinishRun(bg, T, "run_done", core.RunFinish{Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[core.ApprovalID]core.ApprovalStatus{"ap_open": core.ApprovalOpen, "ap_decided": core.ApprovalDecided} {
		run := core.RunID("run_parked_" + map[core.ApprovalStatus]string{core.ApprovalOpen: "open", core.ApprovalDecided: "decided"}[status])
		if _, _, err := st.Approvals.CreateApproval(bg, core.Approval{ID: id, Tenant: T, Run: run, StepRun: "s", Status: status}); err != nil {
			t.Fatal(err)
		}
		wf := core.WaitingForApproval(id)
		if _, err := st.Runs.UpdateRun(bg, T, run, nil, core.RunPatch{WaitingFor: &wf}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Enqueue(bg, core.AdvanceJob(T, "run_has_job", nil)); err != nil {
		t.Fatal(err)
	}

	deps := jobs.ReconcileDeps{Tenants: st.Tenants, Runs: st.Runs, Jobs: q, Resumer: jobs.NewResumer(q), Approvals: st.Approvals}
	fixed, err := jobs.Reconcile(bg, deps)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(fixed, func(i, j int) bool { return fixed[i] < fixed[j] })
	want := []core.RunID{"run_orphan_queued", "run_orphan_running", "run_parked_decided"}
	if len(fixed) != len(want) {
		t.Fatalf("re-queued %v, want %v", fixed, want)
	}
	for i := range want {
		if fixed[i] != want[i] {
			t.Fatalf("re-queued %v, want %v", fixed, want)
		}
	}
	advances, _ := q.List(bg, core.JobFilter{Type: core.JobRunAdvance, Statuses: []core.JobStatus{core.JobPending}})
	if len(advances) != 4 { // the three, plus run_has_job's own
		t.Errorf("%d advance jobs", len(advances))
	}
	again, err := jobs.Reconcile(bg, deps)
	if err != nil || len(again) != 0 {
		t.Errorf("second pass re-queued %v %v", again, err)
	}
}
