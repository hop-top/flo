//go:build !windows

package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// serveEngine runs the engine's worker for the test, as `flo engine serve` does.
func (r *flowRig) serveEngine() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.eng.Serve(ctx) }()
	r.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			r.t.Errorf("serve: %v", err)
		}
	})
}

func (r *flowRig) waitRun(id string) core.Run {
	r.t.Helper()
	end := time.Now().Add(20 * time.Second)
	for time.Now().Before(end) {
		run, err := r.eng.Services.Runs.GetRun(context.Background(), core.DefaultTenant, core.RunID(id))
		if err == nil && run.Status.Terminal() {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatalf("run %s did not finish", id)
	return core.Run{}
}

func TestRunCreateRecordThenReplay(t *testing.T) {
	r := newFlowRig(t)
	for _, args := range [][]string{
		{"flow", "apply", "-f", r.bundle, "--yes"},
		{"flow", "test", "dependency-watch", "--record", "--yes"},
		{"flow", "set", "dependency-watch", "--state", "active", "--yes"},
	} {
		if _, _, err := r.run(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	calls := r.live.calls
	r.serveEngine()

	// --dry-run plans the recording and makes no run
	out, _, err := r.run("run", "create", "--flow", "dependency-watch", "--record", "--dry-run", "--format", "json")
	if err != nil || !strings.Contains(out, "recording:dependency-watch") {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if runs, _ := r.eng.Services.Runs.ListRuns(context.Background(), core.DefaultTenant, core.RunFilter{}); len(runs) != 0 {
		t.Fatal("--dry-run made a run")
	}

	// a run that is not recorded cannot be replayed
	var plain runCreated
	out, _, err = r.run("run", "create", "--flow", "dependency-watch", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, out, &plain)
	if plain.Recording {
		t.Error("a run without --record says it is recorded")
	}
	r.waitRun(plain.Run)
	_, _, err = r.run("run", "replay", plain.Run)
	wantCode(t, err, core.CodeNotFound)

	// the recorded run: no extra confirmation, a recording once it has ended
	var rec runCreated
	out, _, err = r.run("run", "create", "--flow", "dependency-watch", "--record", "--format", "json")
	if err != nil {
		t.Fatalf("--record asks nothing more than run create: %v", err)
	}
	decodeJSON(t, out, &rec)
	if !rec.Recording {
		t.Fatalf("created = %+v", rec)
	}
	if run := r.waitRun(rec.Run); run.Status != core.RunSucceeded || !run.Record {
		t.Fatalf("run = %+v", run)
	}
	live := r.live.calls
	if live-calls != 4 { // two calls for each of the two runs
		t.Fatalf("live calls %d", live-calls)
	}

	out, _, err = r.run("run", "replay", rec.Run, "--format", "json")
	if err != nil {
		t.Fatalf("replay: %v\n%s", err, out)
	}
	var res serve.ReplayResult
	decodeJSON(t, out, &res)
	if !res.Matches || res.Run != rec.Run || res.Calls != 2 || res.Replayed != 2 || r.live.calls != live {
		t.Fatalf("replay = %+v, live calls %d -> %d", res, live, r.live.calls)
	}

	_, _, err = r.run("run", "replay", "not-a-run-id")
	wantCode(t, err, core.CodeUsage)
}
