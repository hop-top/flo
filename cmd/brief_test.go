package cmd

import (
	"context"
	"strings"
	"testing"

	"hop.top/flo/internal/brief"
	"hop.top/flo/internal/core"
)

func briefRig(t *testing.T) *apRig {
	t.Helper()
	r := newApRig(t, "m1", "m2")
	err := r.fx.DB.Outputs.PutOutput(context.Background(), core.Output{
		ID: "out_1", Tenant: cliTenant, Run: "run_1", StepID: "publish", Kind: core.OutputFindings, Title: "deps",
		Items: []any{map[string]any{"title": "x/net 0.31.0", "label": "security", "detail": "CVE-2026-1234"}}, CreatedAt: r.fx.Clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBriefShowJSON(t *testing.T) {
	r := briefRig(t)
	var b brief.Brief
	decodeJSON(t, r.mustRun("brief", "show", "--format", "json"), &b)
	if b.SchemaVersion != brief.SchemaVersion || len(b.Flows) != 1 || b.Flows[0].Flow != "triage" {
		t.Fatalf("brief = %+v", b)
	}
	f := b.Flows[0]
	if len(f.Findings) != 1 || f.Findings[0].Items[0].Title != "x/net 0.31.0" || len(f.Approvals) != 1 || f.Approvals[0].Waiting != 2 {
		t.Errorf("flow = %+v", f)
	}
	if f.Approvals[0].Show != "flo approval show "+string(r.ap.ID) {
		t.Errorf("show hint = %q", f.Approvals[0].Show)
	}
}

func TestBriefShowText(t *testing.T) {
	r := briefRig(t)
	out := r.mustRun("brief", "show")
	for _, want := range []string{"triage\n", "security  x/net 0.31.0 — CVE-2026-1234", "Waiting for you (1)", "flo approval set " + string(r.ap.ID)} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}

func TestBriefShowFilters(t *testing.T) {
	r := briefRig(t)
	var b brief.Brief
	decodeJSON(t, r.mustRun("brief", "show", "--flow", "other", "--format", "json"), &b)
	if len(b.Flows) != 0 || b.Flow != "other" {
		t.Errorf("--flow other = %+v", b)
	}
	decodeJSON(t, r.mustRun("brief", "show", "--since", "1h", "--format", "json"), &b)
	if b.Totals.Findings != 1 {
		t.Errorf("--since 1h = %+v", b.Totals)
	}
	if _, _, err := r.run("brief", "show", "--since", "yesterday"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("--since yesterday: %v", err)
	}
}
