//go:build !windows

package e2e

import (
	"bufio"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
)

// setupInbox installs the inbox pack, grants its capabilities to the flow's
// profile, applies and activates inbox-triage and starts the engine.
func setupInbox(t *testing.T, e *env) *engine {
	t.Helper()
	e.installPack("inbox")
	e.ok("grant", "set", "automation", "mail.read", "--yes")
	e.ok("grant", "set", "automation", "mail.write", "--yes")
	e.applyTested("inbox-triage")
	e.ok("flow", "set", "inbox-triage", "--state", "active", "--yes")
	return e.startEngine()
}

// TestInboxApproval is the inbox-shaped story: the run parks on an approval
// (--wait exits 7), nothing irreversible ran, one decision resumes it to
// success, and the run log is an ordered stream of CloudEvents.
func TestInboxApproval(t *testing.T) {
	e := newEnv(t)
	eng := setupInbox(t, e)
	defer eng.stop()

	var c created
	r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait")
	if r.Exit != core.ExitAwaiting || !strings.Contains(r.Stderr, "awaiting_approval") {
		t.Fatalf("run create --wait: exit %d, want 7\nstdout: %s\nstderr: %s", r.Exit, r.Stdout, r.Stderr)
	}
	if c.Status != "awaiting_approval" || len(c.WaitingOn) != 1 {
		t.Fatalf("created = %+v", c)
	}
	approval := c.WaitingOn[0]
	if lines := readLines(t, filepath.Join(e.scratch, "applied.log")); len(lines) != 0 {
		t.Fatalf("apply ran before the approval: %v", lines)
	}
	var av struct {
		Status string `json:"status"`
		Items  []struct {
			N      int    `json:"n"`
			Action string `json:"action"`
		} `json:"items"`
	}
	e.okJSON(&av, "approval", "show", approval)
	if av.Status != "open" || len(av.Items) != 1 || av.Items[0].N != 1 || !strings.Contains(av.Items[0].Action, "Invoice") {
		t.Fatalf("approval = %+v", av)
	}
	if got := e.show(c.Run); got.Status != "awaiting_approval" {
		t.Errorf("run show: %s", got.Status)
	}

	var d struct {
		Status  string `json:"status"`
		Resumed bool   `json:"resumed"`
	}
	e.okJSON(&d, "approval", "set", approval, "--item", "1=approve", "--yes")
	if d.Status != "decided" || !d.Resumed {
		t.Fatalf("decision = %+v", d)
	}
	eventually(t, "the run to succeed", func() bool { return e.show(c.Run).Status == "succeeded" })
	if lines := readLines(t, filepath.Join(e.scratch, "applied.log")); len(lines) != 1 || lines[0] != "applied m1" {
		t.Errorf("applied = %v", lines)
	}
	if got := strings.TrimSpace(strings.Join(readLines(t, filepath.Join(e.scratch, "tlc", "T-0001")), "")); got != "DONE" {
		t.Errorf("the tlc task of the approval is %q", got)
	}
	// The same decision again changes nothing.
	e.okJSON(&d, "approval", "set", approval, "--item", "1=approve", "--yes")
	if d.Resumed {
		t.Error("a repeated decision resumed the run again")
	}

	// The run log: CloudEvents in order, ending with the terminal event.
	logs := e.ok("run", "logs", c.Run)
	var seqs []int
	var types []string
	sc := bufio.NewScanner(strings.NewReader(logs.Stdout))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev struct {
			SpecVersion string `json:"specversion"`
			ID          string `json:"id"`
			Type        string `json:"type"`
			Subject     string `json:"subject"`
			Seq         int    `json:"seq"`
		}
		if err := jsonUnmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, sc.Text())
		}
		if ev.SpecVersion != "1.0" || ev.ID == "" || ev.Subject != c.Run {
			t.Fatalf("not a CloudEvent of the run: %+v", ev)
		}
		seqs = append(seqs, ev.Seq)
		types = append(types, ev.Type)
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("events are not in order: %v", seqs)
		}
	}
	want := []string{"flo.run.run.created", "flo.run.approval.requested", "flo.run.run.parked", "flo.run.approval.decided", "flo.run.step.resumed"}
	next := 0
	for _, ty := range types {
		if next < len(want) && ty == want[next] {
			next++
		}
	}
	if next != len(want) || types[len(types)-1] != "flo.run.run.succeeded" {
		t.Errorf("event types %v\nwant %v in order, ending in flo.run.run.succeeded", types, want)
	}
}

// TestIdempotencyKeyReturnsTheFirstRun: a second create with the same key
// is the same run.
func TestIdempotencyKeyReturnsTheFirstRun(t *testing.T) {
	e := newEnv(t)
	eng := setupInbox(t, e)
	defer eng.stop()

	var a, b, other created
	e.okJSON(&a, "run", "create", "--flow", "inbox-triage", "--idempotency-key", "k1")
	e.okJSON(&b, "run", "create", "--flow", "inbox-triage", "--idempotency-key", "k1")
	if a.Run == "" || a.Run != b.Run || !a.Created || b.Created {
		t.Errorf("same key: %+v then %+v", a, b)
	}
	e.okJSON(&other, "run", "create", "--flow", "inbox-triage", "--idempotency-key", "k2")
	if other.Run == a.Run {
		t.Error("another key made no new run")
	}
	// The key also settles a run that --wait follows.
	r := e.jsonOf(&b, "run", "create", "--flow", "inbox-triage", "--idempotency-key", "k1", "--wait")
	if b.Run != a.Run || r.Exit != core.ExitAwaiting {
		t.Errorf("wait on the first run: exit %d, %+v", r.Exit, b)
	}
}

// TestCancelParkedRun: cancelling a run parked on an approval finishes it
// and closes its approval.
func TestCancelParkedRun(t *testing.T) {
	e := newEnv(t)
	eng := setupInbox(t, e)
	defer eng.stop()

	var c created
	e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait")
	if c.Status != "awaiting_approval" {
		t.Fatalf("created = %+v", c)
	}
	if r := e.run("run", "cancel", c.Run); r.Exit != core.ExitUsage {
		t.Errorf("cancel without --yes or a terminal: exit %d, want 2", r.Exit)
	}
	e.ok("run", "cancel", c.Run, "--yes")
	eventually(t, "the run to be cancelled", func() bool { return e.show(c.Run).Status == "cancelled" })
	var av struct{ Status string }
	e.okJSON(&av, "approval", "show", c.WaitingOn[0])
	if av.Status != "cancelled" {
		t.Errorf("the approval of a cancelled run is %q", av.Status)
	}
	e.ok("run", "cancel", c.Run, "--yes") // already cancelled is success
	if lines := readLines(t, filepath.Join(e.scratch, "applied.log")); len(lines) != 0 {
		t.Errorf("a cancelled run applied %v", lines)
	}
}
