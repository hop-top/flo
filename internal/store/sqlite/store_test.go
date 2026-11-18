package sqlite_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/store/sqlite"
	specrun "hop.top/flo/spec/run"
)

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x", "flo.db")
	for i := 0; i < 3; i++ {
		if err := sqlite.Migrate(path); err != nil {
			t.Fatalf("migrate #%d: %v", i, err)
		}
	}
	root := t.TempDir()
	for i := 0; i < 2; i++ { // reopening migrates again
		s := newStore(t, root, nil)
		if err := s.Tenants.Ensure(bg, T); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestStagedSurvivesReopenAndSnapshotIsStable(t *testing.T) {
	root := t.TempDir()
	clock := coretest.NewClock(time.Time{})
	s := newStore(t, root, clock)
	r, _, err := s.Runs.CreateRun(bg, core.NewRun{ID: "run_1", Tenant: T, Flow: core.FlowRef{Name: "w"}, Vars: map[string]any{"n": 7, "f": 1.5}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Vars["n"] != int64(7) || r.Vars["f"] != 1.5 {
		t.Errorf("numbers are int64 or float64 on the way out: %#v", r.Vars)
	}
	if err := s.State.Stage(bg, T, r.ID, "k", map[string]any{"i": 3, "f": 2.5, "list": []any{1, "a"}}); err != nil {
		t.Fatal(err)
	}
	snap := map[string]any{"s": 1}
	if _, err := s.Runs.UpdateRun(bg, T, r.ID, nil, core.RunPatch{StateSnapshot: snap}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := newStore(t, root, clock)
	st, _ := s2.State.Staged(bg, T, r.ID)
	want := map[string]any{"i": int64(3), "f": 2.5, "list": []any{int64(1), "a"}}
	got, _ := st["k"].(map[string]any)
	if got["i"] != want["i"] || got["f"] != want["f"] || fmt.Sprint(got["list"]) != fmt.Sprint(want["list"]) {
		t.Errorf("staged after reopen = %#v", st)
	}
	if _, ok := got["list"].([]any)[0].(int64); !ok {
		t.Error("list items are int64")
	}
	run, _ := s2.Runs.GetRun(bg, T, r.ID)
	if run.StateSnapshot["s"] != int64(1) {
		t.Errorf("snapshot = %#v", run.StateSnapshot)
	}
	if _, err := s2.State.FinishRun(bg, T, r.ID, core.RunFinish{Status: core.RunSucceeded, Result: map[string]any{"n": 1}}); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	s3 := newStore(t, root, clock)
	state, _ := s3.State.Snapshot(bg, T, "w")
	if state["k"].(map[string]any)["i"] != int64(3) { //nolint:errcheck,forcetypeassert // asserted by the comparison
		t.Errorf("committed state = %#v", state)
	}
	if run, _ = s3.Runs.GetRun(bg, T, r.ID); run.Result.(map[string]any)["n"] != int64(1) || run.Status != core.RunSucceeded { //nolint:errcheck,forcetypeassert // test
		t.Errorf("result = %#v", run.Result)
	}
}

func TestFinishRunAtomic(t *testing.T) {
	s := newStore(t, t.TempDir(), nil)
	r, _, _ := s.Runs.CreateRun(bg, core.NewRun{ID: "run_1", Tenant: T, Flow: core.FlowRef{Name: "w"}})
	_ = s.State.Stage(bg, T, r.ID, "a", 1)
	_ = s.State.Stage(bg, T, r.ID, "b", 2)
	var wg sync.WaitGroup
	var ok, conflict atomic.Int32
	for _, st := range []core.RunStatus{core.RunSucceeded, core.RunFailed, core.RunCancelled, core.RunSucceeded} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.State.FinishRun(bg, T, r.ID, core.RunFinish{Status: st})
			if err == nil {
				ok.Add(1)
			} else if core.IsCode(err, core.CodeRunStateConflict) {
				conflict.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	run, _ := s.Runs.GetRun(bg, T, r.ID)
	snap, _ := s.State.Snapshot(bg, T, "w")
	staged, _ := s.State.Staged(bg, T, r.ID)
	if len(staged) != 0 {
		t.Error("staged writes dropped or committed")
	}
	if committed := len(snap) == 2; committed != (run.Status == core.RunSucceeded) {
		t.Errorf("status %s but state %v: status and state moved together or not at all", run.Status, snap)
	}
	if ok.Load() < 1 || ok.Load()+conflict.Load() != 4 {
		t.Errorf("ok %d conflict %d", ok.Load(), conflict.Load())
	}
}

// Writers from several pools (as several processes would be) on one tenant
// database must not fail with SQLITE_BUSY.
func TestConcurrentWritersNoBusy(t *testing.T) {
	root := t.TempDir()
	const stores, perStore, writers = 3, 4, 25
	var ss []*sqlite.Store
	for i := 0; i < stores; i++ {
		ss = append(ss, sqlite.Open(root, sqlite.Options{}))
		t.Cleanup(func() { _ = ss[len(ss)-1].Close() })
	}
	ids := &coretest.IDs{}
	var wg sync.WaitGroup
	errs := make(chan error, stores*perStore*writers)
	for si := 0; si < stores; si++ {
		for w := 0; w < perStore; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := ss[si]
				for i := 0; i < writers; i++ {
					id := core.NewRunID(ids)
					if _, _, err := s.Runs.CreateRun(bg, core.NewRun{ID: id, Tenant: T, Flow: core.FlowRef{Name: "w"}}); err != nil {
						errs <- err
						return
					}
					if err := s.State.Stage(bg, T, id, "k", i); err != nil {
						errs <- err
						return
					}
					if _, err := s.State.FinishRun(bg, T, id, core.RunFinish{Status: core.RunSucceeded}); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer failed: %v", err)
	}
	runs, _ := ss[0].Runs.ListRuns(bg, T, core.RunFilter{Limit: 100000})
	if len(runs) != stores*perStore*writers {
		t.Errorf("%d runs, want %d", len(runs), stores*perStore*writers)
	}
}

func TestConcurrentIdemCreateOneRun(t *testing.T) {
	root := t.TempDir()
	ids := &coretest.IDs{}
	var wg sync.WaitGroup
	var created atomic.Int32
	seen := sync.Map{}
	for i := 0; i < 12; i++ {
		s := sqlite.Open(root, sqlite.Options{})
		t.Cleanup(func() { _ = s.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, c, err := s.Runs.CreateRun(bg, core.NewRun{ID: core.NewRunID(ids), Tenant: T, Flow: core.FlowRef{Name: "w"}, IdemKey: "same"})
			if err != nil {
				t.Error(err)
				return
			}
			if c {
				created.Add(1)
			}
			seen.Store(r.ID, true)
		}()
	}
	wg.Wait()
	n := 0
	seen.Range(func(_, _ any) bool { n++; return true })
	if created.Load() != 1 || n != 1 {
		t.Errorf("created %d, distinct runs returned %d", created.Load(), n)
	}
}

func TestConcurrentAdmitTakesLastSlotOnce(t *testing.T) {
	root := t.TempDir()
	ids := &coretest.IDs{}
	var wg sync.WaitGroup
	var granted atomic.Int32
	const max = 3
	for i := 0; i < 20; i++ {
		s := sqlite.Open(root, sqlite.Options{})
		t.Cleanup(func() { _ = s.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := s.Groups.Admit(bg, core.GroupRequest{Tenant: T, Group: "g", Max: max, OnOverlap: specrun.OverlapSkip, Run: core.NewRunID(ids)})
			if err != nil {
				t.Error(err)
				return
			}
			if d.Action == core.GroupRun {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != max {
		t.Errorf("%d slots granted, want %d", granted.Load(), max)
	}
}

func TestTenantIsolationAndRegistry(t *testing.T) {
	root := t.TempDir()
	s := newStore(t, root, nil)
	if l, _ := s.Tenants.List(bg); len(l) != 0 {
		t.Errorf("fresh root lists %v", l)
	}
	for _, id := range []core.TenantID{"zeta", "alpha"} {
		if err := s.Tenants.Ensure(bg, id); err != nil {
			t.Fatal(err)
		}
		if err := s.Tenants.Ensure(bg, id); err != nil {
			t.Fatal(err)
		}
	}
	if l, _ := s.Tenants.List(bg); len(l) != 2 || l[0] != "alpha" || l[1] != "zeta" {
		t.Errorf("tenants = %v", l)
	}
	if err := s.Tenants.Ensure(bg, "../x"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("bad tenant: %v", err)
	}
	for _, id := range []core.TenantID{"alpha", "zeta"} {
		if _, _, err := s.Runs.CreateRun(bg, core.NewRun{ID: "run_1", Tenant: id, Flow: core.FlowRef{Name: "w"}, IdemKey: "k"}); err != nil {
			t.Fatalf("same id in two tenants: %v", err)
		}
	}
	_ = s.Grants
	if _, err := s.Runs.GetRun(bg, "alpha", "run_1"); err != nil {
		t.Error(err)
	}
	for _, id := range []core.TenantID{"alpha", "zeta"} {
		if _, err := os.Stat(filepath.Join(root, "tenants", string(id), "flo.db")); err != nil {
			t.Errorf("file per tenant: %v", err)
		}
	}
	if _, err := s.Runs.GetRun(bg, "gamma", "run_1"); !core.IsCode(err, core.CodeRunNotFound) {
		t.Errorf("a tenant never ensured sees nothing: %v", err)
	}
	if _, err := s.Runs.GetRun(bg, "../x", "run_1"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("invalid tenant: %v", err)
	}
}
