package tlccli_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/tasks/tlccli"
)

const tenant = core.DefaultTenant

func ok(out string) core.CommandResult { return core.CommandResult{Stdout: []byte(out)} }

func show(status, logs string) core.CommandResult {
	return ok(`{"task":{"id":"task_01","status":"` + status + `","title":"Approve triage","assigned_to":"jad"},"logs":[` + logs + `]}`)
}

func argvOf(c core.CommandSpec) string { return strings.Join(c.Argv, " ") }

func TestCreateArgvAndRef(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"tlc", "-C", "/p", "task", "create"}, ok("Created task T-0042: Approve triage\n"))
	a := tlccli.New(cmds, tlccli.Config{Dir: "/p", Profile: "ops"})
	due := time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC)
	ref, err := a.Create(context.Background(), tenant, core.TaskSpec{
		Title: "Approve triage", Description: "3 items", Assignee: "jad", Due: &due,
		Parent: "T-0040", Run: "run_1", Approval: "ap_1", Tags: []string{"inbox"},
	})
	if err != nil || ref != "T-0042" {
		t.Fatalf("ref = %q, %v", ref, err)
	}
	calls := cmds.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.Profile != "ops" || c.Timeout == 0 {
		t.Errorf("profile/timeout = %q %v", c.Profile, c.Timeout)
	}
	want := []string{
		"tlc", "-C", "/p", "task", "create", "Approve triage", "--no-prompt",
		"--description", "3 items", "--assigned-to", "jad", "--due", "2026-11-19",
		"--tag", "flo", "--tag", "inbox", "--tag", "run:run_1", "--tag", "approval:ap_1", "--tag", "parent:T-0040",
	}
	if !slices.Equal(c.Argv, want) {
		t.Errorf("argv = %v\nwant   %v", c.Argv, want)
	}
}

func TestCreateErrors(t *testing.T) {
	cmds := &coretest.Commands{}
	a := tlccli.New(cmds, tlccli.Config{})
	if _, err := a.Create(context.Background(), tenant, core.TaskSpec{}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("no title: %v", err)
	}
	cmds.Reply([]string{"tlc", "task", "create"}, ok("created, somehow\n"))
	if _, err := a.Create(context.Background(), tenant, core.TaskSpec{Title: "x"}); !core.IsCode(err, core.CodeInternal) {
		t.Errorf("no id in output: %v", err)
	}
	cmds2 := &coretest.Commands{}
	cmds2.Reply([]string{"tlc", "task", "create"}, core.CommandResult{ExitCode: 1, Stderr: []byte("boom")})
	if _, err := tlccli.New(cmds2, tlccli.Config{}).Create(context.Background(), tenant, core.TaskSpec{Title: "x"}); !core.IsCode(err, core.CodeInternal) {
		t.Errorf("exit 1: %v", err)
	}
	cmds3 := &coretest.Commands{}
	cmds3.Reply([]string{"tlc", "task", "create"}, core.CommandResult{TimedOut: true})
	if _, err := tlccli.New(cmds3, tlccli.Config{}).Create(context.Background(), tenant, core.TaskSpec{Title: "x"}); !core.IsCode(err, core.CodeTimeout) {
		t.Errorf("timeout: %v", err)
	}
}

// tlc status mapping: DONE is approved, SKIPPED is rejected, the rest open.
func TestGetMapsStatuses(t *testing.T) {
	logs := `{"id":3,"timestamp":"2026-11-18T10:00:00Z","by":"old","action":"DONE","note":"Status changed from TODO to DONE: first"},` +
		`{"id":9,"timestamp":"2026-11-18T11:30:00Z","by":"jad","action":"DONE","note":"Status changed from IN_PROGRESS to DONE: looks fine"},` +
		`{"id":10,"timestamp":"2026-11-18T11:31:00Z","by":"x","action":"UPDATED","note":"other"}`
	for _, tc := range []struct {
		status string
		want   core.TaskState
	}{
		{"TODO", core.TaskOpen}, {"IN_PROGRESS", core.TaskOpen}, {"DONE", core.TaskApproved},
		{"SKIPPED", core.TaskRejected}, {"BLOCKED", core.TaskOpen}, {"done", core.TaskApproved},
	} {
		cmds := &coretest.Commands{}
		cmds.Reply([]string{"tlc", "task", "show", "T-0042"}, show(tc.status, logs))
		info, err := tlccli.New(cmds, tlccli.Config{}).Get(context.Background(), tenant, "T-0042")
		if err != nil {
			t.Fatalf("%s: %v", tc.status, err)
		}
		if info.State != tc.want || info.Ref != "T-0042" || info.Title != "Approve triage" {
			t.Errorf("%s: info = %+v, want state %s", tc.status, info, tc.want)
		}
	}
}

func TestGetDoneCarriesByAtNote(t *testing.T) {
	cmds := &coretest.Commands{}
	logs := `{"id":3,"timestamp":"2026-11-18T10:00:00Z","by":"old","action":"DONE","note":"Status changed from TODO to DONE: first"},` +
		`{"id":9,"timestamp":"2026-11-18T11:30:00Z","by":"jad","action":"DONE","note":"Status changed from IN_PROGRESS to DONE: looks fine"}`
	cmds.Reply([]string{"tlc", "task", "show", "T-0042"}, show("DONE", logs))
	info, err := tlccli.New(cmds, tlccli.Config{}).Get(context.Background(), tenant, "T-0042")
	if err != nil {
		t.Fatal(err)
	}
	if info.By != "jad" || info.Note != "looks fine" || info.At == nil || !info.At.Equal(time.Date(2026, 11, 18, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("info = %+v", info)
	}
	argv := argvOf(cmds.Calls()[0])
	if argv != "tlc task show T-0042 --format json --no-prompt" {
		t.Errorf("argv = %s", argv)
	}
}

func TestGetNotFoundAndGarbage(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"tlc", "task", "show", "T-9"}, core.CommandResult{ExitCode: 1, Stderr: []byte("Error: NOT_FOUND: task T-9 not found")})
	a := tlccli.New(cmds, tlccli.Config{})
	if _, err := a.Get(context.Background(), tenant, "T-9"); !core.IsCode(err, core.CodeTaskNotFound) {
		t.Errorf("not found: %v", err)
	}
	cmds.Reply([]string{"tlc", "task", "show", "T-8"}, ok("not json"))
	if _, err := a.Get(context.Background(), tenant, "T-8"); !core.IsCode(err, core.CodeInternal) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := a.Get(context.Background(), tenant, ""); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("empty ref: %v", err)
	}
}

func TestCloseApprovedCompletes(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"tlc", "task", "show"}, show("TODO", ""))
	cmds.Reply([]string{"tlc", "task", "complete"}, ok("Completed T-0042\n"))
	a := tlccli.New(cmds, tlccli.Config{})
	if err := a.Close(context.Background(), tenant, "T-0042", core.TaskApproved, "decided in flo"); err != nil {
		t.Fatal(err)
	}
	calls := cmds.Calls()
	if len(calls) != 2 || argvOf(calls[1]) != "tlc task complete T-0042 --no-verify --no-prompt --note decided in flo" {
		t.Errorf("calls = %v", calls)
	}
}

func TestCloseRejectedSkips(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"tlc", "task", "show"}, show("IN_PROGRESS", ""))
	cmds.Reply([]string{"tlc", "task", "skip"}, ok("Skipped T-0042\n"))
	if err := tlccli.New(cmds, tlccli.Config{}).Close(context.Background(), tenant, "T-0042", core.TaskRejected, ""); err != nil {
		t.Fatal(err)
	}
	calls := cmds.Calls()
	if len(calls) != 2 || argvOf(calls[1]) != "tlc task skip T-0042 --no-verify --no-prompt" {
		t.Errorf("calls = %v", calls)
	}
}

// Closing a closed task is not an error and runs nothing: an approved task
// is not skipped afterwards.
func TestCloseClosedTaskIsANoOp(t *testing.T) {
	for _, status := range []string{"DONE", "SKIPPED"} {
		cmds := &coretest.Commands{}
		cmds.Reply([]string{"tlc", "task", "show"}, show(status, ""))
		a := tlccli.New(cmds, tlccli.Config{})
		for _, outcome := range []core.TaskState{core.TaskApproved, core.TaskRejected} {
			if err := a.Close(context.Background(), tenant, "T-1", outcome, "n"); err != nil {
				t.Fatalf("%s/%s: %v", status, outcome, err)
			}
		}
		for _, c := range cmds.Calls() {
			if c.Argv[2] != "show" {
				t.Errorf("%s: ran %v on a closed task", status, c.Argv)
			}
		}
	}
}

func TestCloseValidatesOutcomeAndPropagatesNotFound(t *testing.T) {
	cmds := &coretest.Commands{}
	a := tlccli.New(cmds, tlccli.Config{})
	if err := a.Close(context.Background(), tenant, "T-1", core.TaskOpen, ""); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("open outcome: %v", err)
	}
	cmds.Reply([]string{"tlc", "task", "show"}, core.CommandResult{ExitCode: 1, Stderr: []byte("task not found")})
	if err := a.Close(context.Background(), tenant, "T-1", core.TaskApproved, ""); !core.IsCode(err, core.CodeTaskNotFound) {
		t.Errorf("missing task: %v", err)
	}
}
