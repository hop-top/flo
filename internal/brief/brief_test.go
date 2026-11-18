package brief_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/brief"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const tenant = core.DefaultTenant

var bg = context.Background()

type rig struct {
	t   *testing.T
	fx  *coretest.Fixture
	svc *approvals.Service
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fx := coretest.New()
	svc := approvals.New(approvals.Deps{
		Store: fx.DB.Approvals, Tasks: fx.Tasks, Resumer: fx.Resumer, Trust: fx.Trust, Runs: fx.DB.Runs, Flows: fx.DB.Flows,
		Jobs: fx.Jobs, RunLog: fx.RunLog, Clock: fx.Clock, IDs: fx.IDs,
	})
	return &rig{t: t, fx: fx, svc: svc}
}

func (r *rig) deps() brief.Deps {
	return brief.Deps{Outputs: r.fx.DB.Outputs, Runs: r.fx.DB.Runs, Approvals: r.svc, Clock: r.fx.Clock}
}

func (r *rig) run(id, flow string) {
	r.t.Helper()
	if _, _, err := r.fx.DB.Runs.CreateRun(bg, core.NewRun{
		ID: core.RunID(id), Tenant: tenant, Flow: core.FlowRef{Name: flow, Version: "1", Hash: "h"}, StartedBy: "jad",
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) output(id, run string, kind core.OutputKind, title string, items ...any) {
	r.t.Helper()
	o := core.Output{ID: core.OutputID(id), Tenant: tenant, Run: core.RunID(run), StepID: "publish", Kind: kind, Title: title, CreatedAt: r.fx.Clock.Now()}
	if kind == core.OutputBrief {
		o.Body = "text of " + title
	} else {
		o.Items = items
	}
	if err := r.fx.DB.Outputs.PutOutput(bg, o); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) approve(run string, n int) core.Approval {
	r.t.Helper()
	var items []core.ApprovalItem
	for i := range n {
		items = append(items, core.ApprovalItem{ID: string(rune('a' + i)), Action: "archive", Why: "newsletter"})
	}
	ap, err := r.svc.Open(bg, tenant, core.RunID(run), core.StepRun{ID: "stp_" + core.StepRunID(run), StepID: "approve"}, core.ApprovalRequest{Title: "Approve " + run, Items: items})
	if err != nil {
		r.t.Fatal(err)
	}
	return ap
}

func finding(title, label string) map[string]any {
	return map[string]any{"title": title, "label": label, "detail": "d", "url": "https://x.example/" + title}
}

func (r *rig) seed() {
	r.run("run_dep", "dependency-watch")
	r.run("run_mail", "inbox-triage")
	r.output("out_1", "run_dep", core.OutputFindings, "deps", finding("x/net", "security"), finding("lipgloss", "breaking"))
	r.fx.Clock.Advance(time.Hour)
	r.output("out_2", "run_dep", core.OutputBrief, "Monaam")
	r.approve("run_mail", 3)
	r.approve("run_dep", 1)
}

func TestBuildGroupsByFlow(t *testing.T) {
	r := newRig(t)
	r.seed()
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != brief.SchemaVersion || b.RetentionDays != 90 {
		t.Errorf("header = %q, %d days", b.SchemaVersion, b.RetentionDays)
	}
	if len(b.Flows) != 2 || b.Flows[0].Flow != "dependency-watch" || b.Flows[1].Flow != "inbox-triage" {
		t.Fatalf("flows = %+v", b.Flows)
	}
	if b.Totals != (brief.Totals{Flows: 2, Findings: 1, Briefs: 1, Approvals: 2}) {
		t.Errorf("totals = %+v", b.Totals)
	}
	dep := b.Flows[0]
	if len(dep.Findings) != 1 || len(dep.Findings[0].Items) != 2 || dep.Findings[0].Items[0].Label != "security" || dep.Findings[0].Items[0].URL == "" {
		t.Errorf("findings = %+v", dep.Findings)
	}
	if len(dep.Briefs) != 1 || dep.Briefs[0].Body != "text of Monaam" {
		t.Errorf("briefs = %+v", dep.Briefs)
	}
	mail := b.Flows[1]
	if len(mail.Findings) != 0 || len(mail.Approvals) != 1 || mail.Approvals[0].Items != 3 || mail.Approvals[0].Waiting != 3 {
		t.Errorf("mail = %+v", mail)
	}
	if got := mail.Approvals[0]; !strings.HasPrefix(got.Show, "flo approval show ap_") || !strings.Contains(got.Set, "flo approval set "+got.ID+" --item") {
		t.Errorf("hints = %q, %q", got.Show, got.Set)
	}
}

func TestBuildOnlyOpenApprovals(t *testing.T) {
	r := newRig(t)
	r.run("run_1", "f")
	ap := r.approve("run_1", 1)
	if _, err := r.svc.Decide(bg, tenant, core.DecideRequest{Approval: ap.ID, All: core.VerdictApprove, By: "jad"}); err != nil {
		t.Fatal(err)
	}
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Totals.Approvals != 0 || len(b.Flows) != 0 {
		t.Errorf("a decided approval is in the brief: %+v", b)
	}
}

func TestBuildFlowFilter(t *testing.T) {
	r := newRig(t)
	r.seed()
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{Flow: "inbox-triage"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Flows) != 1 || b.Flows[0].Flow != "inbox-triage" || b.Flow != "inbox-triage" || b.Totals.Findings != 0 {
		t.Errorf("filtered = %+v", b)
	}
}

func TestBuildSince(t *testing.T) {
	r := newRig(t)
	r.seed() // out_1 at t0, out_2 at t0+1h, approvals at t0+1h
	cut := r.fx.Clock.Now().Add(-time.Minute)
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{Since: cut})
	if err != nil {
		t.Fatal(err)
	}
	if b.Totals.Findings != 0 || b.Totals.Briefs != 1 {
		t.Errorf("since kept the wrong outputs: %+v", b.Totals)
	}
	// waiting approvals show whatever their age
	r.fx.Clock.Advance(48 * time.Hour)
	if b, err = brief.Build(bg, r.deps(), tenant, brief.Options{Since: r.fx.Clock.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if b.Totals.Approvals != 2 || b.Totals.Briefs != 0 {
		t.Errorf("old approvals: %+v", b.Totals)
	}
}

// Retention is enforced on read: an output past 90 days never shows, pruned
// or not, and --since cannot reach back further.
func TestBuildNeverShowsPastRetention(t *testing.T) {
	r := newRig(t)
	r.run("run_dep", "dependency-watch")
	r.output("out_old", "run_dep", core.OutputBrief, "old")
	r.fx.Clock.Advance(core.OutputRetention - time.Hour)
	r.output("out_edge", "run_dep", core.OutputBrief, "edge")
	r.fx.Clock.Advance(2 * time.Hour)
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{Since: r.fx.Clock.Now().Add(-365 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if b.Totals.Briefs != 1 || b.Flows[0].Briefs[0].Title != "edge" {
		t.Errorf("briefs = %+v", b.Flows)
	}
	if want := r.fx.Clock.Now().Add(-core.OutputRetention).UTC(); !b.Since.Equal(want) {
		t.Errorf("since = %s, want the retention floor %s", b.Since, want)
	}
}

func TestBuildUnknownRunFilesUnderUnknown(t *testing.T) {
	r := newRig(t)
	r.output("out_1", "run_gone", core.OutputBrief, "orphan")
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Flows) != 1 || b.Flows[0].Flow != brief.UnknownFlow {
		t.Errorf("flows = %+v", b.Flows)
	}
}

func TestBuildEmptyEncodesLists(t *testing.T) {
	r := newRig(t)
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	if !strings.Contains(string(raw), `"flows":[]`) {
		t.Errorf("an empty brief has flows null: %s", raw)
	}
}

func TestWriteText(t *testing.T) {
	r := newRig(t)
	r.seed()
	b, err := brief.Build(bg, r.deps(), tenant, brief.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := brief.WriteText(&buf, b); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"2 flows · 2 findings · 1 briefs · 2 waiting",
		"\ndependency-watch\n", "security  x/net — d <https://x.example/x/net>", "Monaam  brief ready → flo run show run_dep",
		"\ninbox-triage\n", "Waiting for you (1)", "(3 of 3 items waiting)", "next: flo approval set ap_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "security") > strings.Index(out, "Waiting for you") {
		t.Errorf("findings come after waiting:\n%s", out)
	}
	var empty bytes.Buffer
	_ = brief.WriteText(&empty, brief.Brief{GeneratedAt: b.GeneratedAt})
	if !strings.Contains(empty.String(), "Nothing found, nothing waiting.") {
		t.Errorf("empty text: %s", empty.String())
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 18, 7, 30, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                     {},
		"36h":                  now.Add(-36 * time.Hour),
		"7d":                   now.AddDate(0, 0, -7),
		"2026-11-01":           time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		"2026-11-17T06:00:00Z": time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC),
	} {
		got, err := brief.ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseSince(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"yesterday", "-1h", "xd", "2026-13-01"} {
		if _, err := brief.ParseSince(in, now); !core.IsCode(err, core.CodeUsage) {
			t.Errorf("ParseSince(%q): %v, want usage", in, err)
		}
	}
}

func TestPrune(t *testing.T) {
	r := newRig(t)
	p := &brief.Pruner{Outputs: r.fx.DB.Outputs, Jobs: r.fx.Jobs, Clock: r.fx.Clock}
	r.run("run_1", "f")
	r.output("out_old", "run_1", core.OutputBrief, "old")
	r.fx.Clock.Advance(core.OutputRetention + time.Hour)
	r.output("out_new", "run_1", core.OutputBrief, "new")

	if err := p.Ensure(bg, tenant); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(bg, tenant); err != nil {
		t.Fatal(err)
	}
	pending, _ := r.fx.Jobs.List(bg, core.JobFilter{Type: brief.JobPrune, Statuses: []core.JobStatus{core.JobPending}})
	if len(pending) != 1 {
		t.Fatalf("Ensure twice queued %d jobs", len(pending))
	}
	first := pending[0]
	if !first.ScheduledAt.After(r.fx.Clock.Now()) || first.ScheduledAt.Sub(r.fx.Clock.Now()) > brief.PruneEvery {
		t.Errorf("scheduled %s, now %s", first.ScheduledAt, r.fx.Clock.Now())
	}

	// the job fires a day on: it prunes what is past the retention and queues the next day's
	r.fx.Clock.Set(first.ScheduledAt)
	if err := p.Handle(bg, first.Job); err != nil {
		t.Fatal(err)
	}
	outs, _ := r.fx.DB.Outputs.ListOutputs(bg, tenant, core.OutputFilter{})
	if len(outs) != 1 || outs[0].ID != "out_new" {
		t.Errorf("outputs after prune = %+v", outs)
	}
	next, _ := r.fx.Jobs.List(bg, core.JobFilter{Type: brief.JobPrune, Key: "prune@" + itoa(first.ScheduledAt.Add(brief.PruneEvery).Unix())})
	if len(next) != 1 {
		t.Errorf("the next prune is not queued: %+v", next)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
