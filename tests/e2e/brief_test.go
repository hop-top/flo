//go:build !windows

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
)

// briefDoc is the part of `flo brief show --format json` the tests read.
type briefDoc struct {
	SchemaVersion string `json:"schema_version"`
	Flow          string `json:"flow"`
	RetentionDays int    `json:"retention_days"`
	Flows         []struct {
		Flow     string `json:"flow"`
		Findings []struct {
			Run   string `json:"run"`
			Items []struct {
				Title string `json:"title"`
				Label string `json:"label"`
			} `json:"items"`
		} `json:"findings"`
		Briefs    []struct{ Title string } `json:"briefs"`
		Approvals []struct {
			ID      string `json:"id"`
			Waiting int    `json:"waiting"`
			Show    string `json:"show"`
			Set     string `json:"set"`
		} `json:"approvals"`
	} `json:"flows"`
	Totals struct {
		Flows, Findings, Briefs, Approvals int
	} `json:"totals"`
}

// TestBrief is the brief story: a watcher publishes findings, inbox triage
// parks on an approval, and `flo brief show` lists both grouped by flow, then
// drops the approval once it is decided. Outputs past the 90-day retention
// never show, even before the prune job has run.
func TestBrief(t *testing.T) {
	e := newEnv(t)
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	e.ok("flow", "apply", "-f", testdata("flows", "dependency-watch"))
	e.ok("flow", "set", "dependency-watch", "--state", "active", "--yes")
	eng := setupInbox(t, e)
	defer eng.stop()

	var empty briefDoc
	e.okJSON(&empty, "brief", "show")
	if empty.SchemaVersion != "1.0" || empty.RetentionDays != 90 || len(empty.Flows) != 0 {
		t.Fatalf("empty brief = %+v", empty)
	}
	if r := e.ok("brief", "show"); !strings.Contains(r.Stdout, "Nothing found, nothing waiting.") {
		t.Errorf("empty text:\n%s", r.Stdout)
	}

	var w created
	if r := e.jsonOf(&w, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 || w.Status != "succeeded" {
		t.Fatalf("watcher run: exit %d %+v\n%s", r.Exit, w, r.Stderr)
	}
	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait"); r.Exit != core.ExitAwaiting {
		t.Fatalf("triage run: exit %d\n%s", r.Exit, r.Stderr)
	}
	approval := c.WaitingOn[0]

	// An output the retention has already expired: stored, never shown.
	st := e.store()
	if err := st.Outputs.PutOutput(context.Background(), core.Output{
		ID: "out_expired", Tenant: tenant, Run: core.RunID(w.Run), StepID: "findings", Kind: core.OutputFindings,
		Items: []any{map[string]any{"title": "ancient"}}, CreatedAt: time.Now().Add(-core.OutputRetention - 24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var b briefDoc
	e.okJSON(&b, "brief", "show")
	if len(b.Flows) != 2 || b.Flows[0].Flow != "dependency-watch" || b.Flows[1].Flow != "inbox-triage" {
		t.Fatalf("flows = %+v", b.Flows)
	}
	dep, mail := b.Flows[0], b.Flows[1]
	if len(dep.Findings) != 1 || dep.Findings[0].Run != w.Run || len(dep.Findings[0].Items) != 5 || dep.Findings[0].Items[0].Label == "" {
		t.Errorf("findings = %+v", dep.Findings)
	}
	if len(mail.Approvals) != 1 || mail.Approvals[0].ID != approval || mail.Approvals[0].Waiting != 1 ||
		mail.Approvals[0].Show != "flo approval show "+approval || !strings.Contains(mail.Approvals[0].Set, "flo approval set "+approval) {
		t.Errorf("approvals = %+v", mail.Approvals)
	}
	if b.Totals.Flows != 2 || b.Totals.Findings != 1 || b.Totals.Approvals != 1 {
		t.Errorf("totals = %+v", b.Totals)
	}

	// Human text: grouped by flow, findings first, then what waits and how to decide.
	text := e.ok("brief", "show").Stdout
	for _, want := range []string{"\ndependency-watch\n", "\ninbox-triage\n", "Waiting for you (1)", "flo approval show " + approval, "next: flo approval set " + approval} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ancient") {
		t.Errorf("an output past the retention shows:\n%s", text)
	}

	// Filters.
	var f briefDoc
	e.okJSON(&f, "brief", "show", "--flow", "inbox-triage")
	if f.Flow != "inbox-triage" || len(f.Flows) != 1 || f.Totals.Findings != 0 || f.Totals.Approvals != 1 {
		t.Errorf("--flow = %+v", f)
	}
	e.okJSON(&f, "brief", "show", "--since", "1h")
	if f.Totals.Findings != 1 {
		t.Errorf("--since 1h = %+v", f.Totals)
	}
	e.okJSON(&f, "brief", "show", "--since", time.Now().Add(time.Hour).Format(time.RFC3339))
	if f.Totals.Findings != 0 || f.Totals.Approvals != 1 {
		t.Errorf("--since in the future: findings drop, waiting approvals stay: %+v", f.Totals)
	}
	if r := e.run("brief", "show", "--since", "yesterday"); r.Exit != core.ExitUsage {
		t.Errorf("--since yesterday: exit %d\n%s", r.Exit, r.Stderr)
	}

	// Deciding takes the approval out of the brief.
	e.ok("approval", "set", approval, "--item", "1=approve", "--yes")
	e.okJSON(&f, "brief", "show")
	if f.Totals.Approvals != 0 || f.Totals.Findings != 1 {
		t.Errorf("after the decision = %+v", f.Totals)
	}
}
