//go:build !windows

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"hop.top/flo/cmd"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/engine/serve"
)

func vevent(uid, start, extra string) string {
	return fmt.Sprintf("BEGIN:VEVENT\r\nUID:%s\r\nDTSTART:%s\r\nSUMMARY:%s\r\n%sEND:VEVENT\r\n", uid, start, uid, extra)
}

// TestCalendarFlowFiresOncePerEvent runs the engine in process on a test
// clock over an ICS file: a calendar-triggered flow (start minus 30m, more
// than one attendee) runs once per qualifying event, a fire time already past
// runs at once, and later syncs never run an event again.
func TestCalendarFlowFiresOncePerEvent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("FLO_PROFILE", "jad")
	t.Setenv("USER", "jad")

	two := "ATTENDEE:mailto:a@example.com\r\nATTENDEE:mailto:b@example.com\r\n"
	file := filepath.Join(root, "work.ics")
	cal := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//e2e//EN\r\n" +
		vevent("standup", "20261118T122000Z", two) + // starts in 20m: its fire time (11:50) is past
		vevent("review", "20261118T130000Z", two) + // fires at 12:30
		vevent("solo", "20261118T130000Z", "ATTENDEE:mailto:a@example.com\r\n") + // one attendee: the flow's `when` refuses it
		vevent("axed", "20261118T140000Z", two+"STATUS:CANCELLED\r\n") +
		vevent("far", "20261130T140000Z", two) + // beyond the 7d horizon
		"END:VCALENDAR\r\n"
	if err := os.WriteFile(file, []byte(cal), 0o600); err != nil {
		t.Fatal(err)
	}

	clock := coretest.NewClock(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	eng, err := serve.Open(ctx, serve.Config{
		Clock: clock, Owner: "jad", Interval: 10 * time.Millisecond, NoSync: true,
		ExprConfig: map[string]any{"calendar": map[string]any{"url": file, "sync_interval": "5m"}},
	})
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
		if err := r.Execute(context.Background()); err != nil {
			return out.String(), &execError{err: err, stderr: errOut.String()}
		}
		return out.String(), nil
	}
	bundle := copyBundle(t, "calendar-event") // recording writes into the bundle
	// the test feeds the trigger payload a calendar trigger would
	event := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(event, []byte(`{"event":{"title":"standup","start":"2026-11-18T13:00:00Z","attendees":["a@example.org","b@example.org"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"grant", "set", "automation", "calendar.read", "--yes"},
		{"flow", "apply", "-f", bundle, "--yes"},
		{"flow", "test", "calendar-event", "--record", "--allow-live", "--trigger-file", event, "--yes"},
		{"flow", "set", "calendar-event", "--state", "active", "--yes"},
	} {
		if out, err := flo(args...); err != nil {
			t.Fatalf("flo %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	want := func(titles ...string) func() bool {
		return func() bool { return strings.Join(titlesOf(t, eng), ",") == strings.Join(titles, ",") }
	}
	eventually(t, "the past fire time to run standup at once", want("standup"))
	clock.Advance(30 * time.Minute) // 12:30: review's fire
	eventually(t, "review to run at start minus 30m", want("review", "standup"))
	for i := 0; i < 4; i++ { // later syncs re-plan, nothing runs again
		clock.Advance(15 * time.Minute)
		time.Sleep(200 * time.Millisecond)
	}
	if got := titlesOf(t, eng); strings.Join(got, ",") != "review,standup" {
		t.Errorf("runs after later syncs: %v", got)
	}
}

// titlesOf lists the brief titles of the flow's runs, sorted; the runs'
// number is asserted as well, one per output.
func titlesOf(t *testing.T, eng *serve.Engine) []string {
	t.Helper()
	runs, err := eng.Services.Runs.ListRuns(context.Background(), core.DefaultTenant, core.RunFilter{Flow: "calendar-event"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range runs {
		if r.Status != core.RunSucceeded {
			out = append(out, "("+string(r.Status)+")")
			continue
		}
		outs, _ := eng.Services.Outputs.ListOutputs(context.Background(), core.DefaultTenant, core.OutputFilter{Run: r.ID})
		if len(outs) != 1 {
			out = append(out, fmt.Sprintf("(%d outputs)", len(outs)))
		}
		for _, o := range outs {
			out = append(out, o.Title)
		}
	}
	sort.Strings(out)
	return out
}
