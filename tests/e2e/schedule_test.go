//go:build !windows

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/flo/cmd"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/engine/serve"
)

// TestScheduleFiresOnTheClock assembles the engine in process on a test clock
// and shows the whole schedule path: activation registers the trigger and
// queues its first fire, the fire starts a run as the engine's profile, the
// executor runs it and the next occurrence is queued. A schedule cannot be
// waited for on the wall clock, so this one test runs in process.
func TestScheduleFiresOnTheClock(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	scratch := filepath.Join(root, "scratch")
	for _, d := range []string{shimDir, scratch} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte(body), 0o755); err != nil { //nolint:gosec // an executable fake
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PATH", shimDir+":/usr/bin:/bin")
	t.Setenv("FLO_PROFILE", "jad")
	t.Setenv("USER", "jad")
	t.Setenv("FLO_E2E_DIR", scratch)
	t.Setenv("FLO_E2E_TESTDATA", testdata())

	clock := coretest.NewClock(time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)) // the flow fires daily at 07:00 UTC
	ctx, cancel := context.WithCancel(context.Background())
	eng, err := serve.Open(ctx, serve.Config{Clock: clock, Owner: "jad", Interval: 10 * time.Millisecond, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- eng.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("engine serve: %v", err)
		}
		_ = eng.Close(context.Background())
	})

	flo := func(args ...string) (string, error) {
		r := cmd.NewRootOver(func(context.Context) (*serve.Engine, error) { return eng, nil })
		var out, errOut bytes.Buffer
		r.Cmd.SetOut(&out)
		r.Cmd.SetErr(&errOut)
		r.SetArgs(append(args, "--format", "json"))
		err := r.Execute(context.Background())
		if err != nil {
			err = &execError{err: err, stderr: errOut.String()}
		}
		return out.String(), err
	}
	for _, args := range [][]string{
		{"grant", "set", "automation", "vcs.read", "--yes"},
		{"flow", "apply", "-f", testdata("flows", "dependency-watch")},
		{"flow", "set", "dependency-watch", "--state", "active", "--yes"},
	} {
		if out, err := flo(args...); err != nil {
			t.Fatalf("flo %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	list := func() []runRowView {
		out, err := flo("run", "list", "--flow", "dependency-watch")
		if err != nil {
			t.Fatal(err)
		}
		var rows []runRowView
		if err := jsonUnmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return rows
	}

	if n := len(list()); n != 0 {
		t.Fatalf("%d runs before the schedule is due", n)
	}
	clock.Advance(30 * time.Minute) // 06:30: not yet
	time.Sleep(150 * time.Millisecond)
	if n := len(list()); n != 0 {
		t.Fatalf("%d runs at 06:30", n)
	}
	clock.Advance(time.Hour) // 07:30: the 07:00 fire is due
	eventually(t, "the 07:00 fire to run", func() bool {
		rows := list()
		return len(rows) == 1 && rows[0].Status == "succeeded"
	})
	if rows := list(); rows[0].Trigger != "schedule" {
		t.Errorf("the run was started by %q", rows[0].Trigger)
	}
	run, err := eng.Services.Runs.GetRun(context.Background(), core.DefaultTenant, core.RunID(list()[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if run.StartedBy != serve.DefaultEngineProfile || run.RunAs != "automation" {
		t.Errorf("started by %q, as %q", run.StartedBy, run.RunAs)
	}
	outs, _ := eng.Services.Outputs.ListOutputs(context.Background(), core.DefaultTenant, core.OutputFilter{Run: run.ID})
	if len(outs) != 1 {
		t.Errorf("the scheduled run published %d outputs", len(outs))
	}

	// the next occurrence is queued for 07:00 the next day
	var trig []triggerRow
	out, err := flo("trigger", "list", "--flow", "dependency-watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal([]byte(out), &trig); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range trig {
		if x.Kind == "schedule" {
			found = true
			if x.Next != "2026-11-19T07:00:00Z" {
				t.Errorf("next fire %q, want 2026-11-19T07:00:00Z", x.Next)
			}
		}
	}
	if !found {
		t.Errorf("no schedule trigger: %+v", trig)
	}
}

type runRowView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Trigger string `json:"trigger"`
}

// execError keeps the stderr an in-process command wrote next to its error.
type execError struct {
	err    error
	stderr string
}

func (e *execError) Error() string { return e.err.Error() + "\n" + e.stderr }
func (e *execError) Unwrap() error { return e.err }
