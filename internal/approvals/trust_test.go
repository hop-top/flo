package approvals_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const inboxProvider = `
trust: inbox
kind: command
env: { INBOX_HOME: /data/inbox }
read:
  argv: [inbox, trust, list, --format, json]
decide:
  argv: [inbox, approve, "{{ in.context.run }}", --decisions, "-", --format, json]
  stdin: { json: "{{ in.decisions }}" }
set:
  argv: [inbox, trust, "{{ in.level == 'auto' ? 'promote' : 'reset' }}", "{{ in.class }}"]
`

func provider(t *testing.T) approvals.Provider {
	t.Helper()
	p, err := approvals.ParseProvider([]byte(inboxProvider))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTrust(t *testing.T, cmds *coretest.Commands) *approvals.CommandTrust {
	t.Helper()
	tr, err := approvals.NewCommandTrust(cmds, approvals.StaticLocator{"inbox": provider(t)})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTrustLevelsReadsTheProvider(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"inbox", "trust", "list"}, core.CommandResult{Stdout: []byte(
		`[{"class":"archive","level":"auto","streak":9,"threshold":8,"resets":1,"last_reset":"2026-11-01T10:00:00Z"},{"class":"unsubscribe","level":"ask"}]`)})
	levels, err := newTrust(t, cmds).Levels(context.Background(), core.DefaultTenant, "inbox", map[string]any{"run": "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 || levels[0].Class != "archive" || levels[0].Level != core.TrustAuto || levels[0].Streak != 9 ||
		levels[0].LastReset == nil || levels[1].Level != core.TrustAsk {
		t.Errorf("levels = %+v", levels)
	}
	c := cmds.Calls()[0]
	if !slices.Equal(c.Argv, []string{"inbox", "trust", "list", "--format", "json"}) || c.Env["INBOX_HOME"] != "/data/inbox" || c.Profile != "" {
		t.Errorf("call = %+v", c)
	}
}

func TestTrustLevelsRefusesAnUnknownLevel(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"inbox", "trust", "list"}, core.CommandResult{Stdout: []byte(`[{"class":"archive","level":"graduated"}]`)})
	if _, err := newTrust(t, cmds).Levels(context.Background(), core.DefaultTenant, "inbox", nil); !core.IsCode(err, core.CodeOutputInvalid) {
		t.Errorf("err = %v", err)
	}
	cmds2 := &coretest.Commands{}
	cmds2.Reply([]string{"inbox", "trust", "list"}, core.CommandResult{Stdout: []byte(`not json`)})
	if _, err := newTrust(t, cmds2).Levels(context.Background(), core.DefaultTenant, "inbox", nil); !core.IsCode(err, core.CodeOutputInvalid) {
		t.Errorf("garbage: %v", err)
	}
}

// decide argv: the provider's argv rendered over in, the decisions as JSON
// on stdin.
func TestTrustDecideArgvAndStdin(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"inbox", "approve"}, core.CommandResult{Stdout: []byte(`{"applied":["m1","m2"],"skipped":["m3"]}`)})
	got, err := newTrust(t, cmds).Decide(context.Background(), core.DefaultTenant, "inbox", map[string]any{"run": "run_1"}, []core.TrustDecision{
		{ID: "m1", Verdict: core.VerdictApprove, Item: map[string]any{"id": "m1", "class": []any{"archive"}}},
		{ID: "m2", Verdict: core.VerdictReject, Reason: "spam?"},
		{ID: "m3", Verdict: core.VerdictCorrect},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Applied, []string{"m1", "m2"}) || !slices.Equal(got.Skipped, []string{"m3"}) {
		t.Errorf("applied = %+v", got)
	}
	c := cmds.Calls()[0]
	if !slices.Equal(c.Argv, []string{"inbox", "approve", "run_1", "--decisions", "-", "--format", "json"}) {
		t.Errorf("argv = %v", c.Argv)
	}
	var stdin []map[string]any
	if err := json.Unmarshal(c.Stdin, &stdin); err != nil {
		t.Fatalf("stdin %q: %v", c.Stdin, err)
	}
	if len(stdin) != 3 || stdin[0]["id"] != "m1" || stdin[0]["verdict"] != "approve" || stdin[1]["reason"] != "spam?" ||
		stdin[0]["item"].(map[string]any)["id"] != "m1" {
		t.Errorf("stdin = %s", c.Stdin)
	}
	if _, has := stdin[0]["reason"]; has {
		t.Error("an empty reason is sent")
	}
}

func TestTrustSetPromotesAndDemotes(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"inbox", "trust"}, core.CommandResult{})
	tr := newTrust(t, cmds)
	ctx := context.Background()
	if err := tr.Set(ctx, core.DefaultTenant, "inbox", "archive", core.TrustAuto, "streak"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Set(ctx, core.DefaultTenant, "inbox", "archive", core.TrustAsk, ""); err != nil {
		t.Fatal(err)
	}
	calls := cmds.Calls()
	if !slices.Equal(calls[0].Argv, []string{"inbox", "trust", "promote", "archive"}) || !slices.Equal(calls[1].Argv, []string{"inbox", "trust", "reset", "archive"}) {
		t.Errorf("argv = %v / %v", calls[0].Argv, calls[1].Argv)
	}
}

func TestTrustSetCapped(t *testing.T) {
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"inbox", "trust"}, core.CommandResult{ExitCode: 2, Stdout: []byte(`{"code":"trust_capped","message":"unsubscribe never graduates"}`)})
	err := newTrust(t, cmds).Set(context.Background(), core.DefaultTenant, "inbox", "unsubscribe", core.TrustAuto, "")
	if !core.IsCode(err, core.CodeTrustCapped) {
		t.Errorf("err = %v, want trust_capped", err)
	}
}

func TestTrustProviderFailures(t *testing.T) {
	ctx := context.Background()
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"inbox", "trust", "list"}, core.CommandResult{ExitCode: 1, Stderr: []byte("locked")})
	if _, err := newTrust(t, cmds).Levels(ctx, core.DefaultTenant, "inbox", nil); !core.IsCode(err, core.CodeNodeReportedFailure) {
		t.Errorf("exit 1: %v", err)
	}
	cmds = &coretest.Commands{}
	cmds.Reply([]string{"inbox", "trust", "list"}, core.CommandResult{TimedOut: true})
	if _, err := newTrust(t, cmds).Levels(ctx, core.DefaultTenant, "inbox", nil); !core.IsCode(err, core.CodeTimeout) {
		t.Errorf("timeout: %v", err)
	}
	if _, err := newTrust(t, &coretest.Commands{}).Levels(ctx, core.DefaultTenant, "nobody", nil); !core.IsCode(err, core.CodeNotFound) {
		t.Errorf("unknown provider: %v", err)
	}
	// an unresolvable reference in argv is an error, not a literal
	cmds = &coretest.Commands{}
	if _, err := newTrust(t, cmds).Decide(ctx, core.DefaultTenant, "inbox", map[string]any{}, nil); err == nil {
		t.Error("in.context.run is missing and Decide ran")
	}
	if len(cmds.Calls()) != 0 {
		t.Error("a command ran with an unrendered argv")
	}
}

func TestParseProviderStrict(t *testing.T) {
	for name, doc := range map[string]string{
		"no name":      "kind: command\n",
		"http":         "trust: x\nkind: http\n",
		"unknown key":  "trust: x\nkind: command\nbogus: 1\n",
		"empty argv":   "trust: x\nkind: command\nread: { argv: [] }\n",
		"unknown kind": "trust: x\nkind: grpc\n",
	} {
		if _, err := approvals.ParseProvider([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := approvals.ParseProvider([]byte("trust: x\nkind: http\n")); !core.IsCode(err, core.CodeEngineFeature) {
		t.Errorf("http: %v", err)
	}
}

// A provider document is read by two parsers, the spec's (at install) and the
// engine's (at run time): what the first accepts the second must too.
func TestParseProviderAcceptsTheSpecsKeys(t *testing.T) {
	doc := `trust: inbox
kind: command
description: Trust state owned by a script.
side_effect: write-shared
x-owner: ops
read: { argv: [inbox-trust, list] }
`
	p, err := approvals.ParseProvider([]byte(doc))
	if err != nil {
		t.Fatalf("a document the spec accepts: %v", err)
	}
	if p.Description != "Trust state owned by a script." || p.Read == nil || p.Read.Argv[0] != "inbox-trust" {
		t.Errorf("provider = %+v", p)
	}
}

func TestDirLocator(t *testing.T) {
	flowDir, packDir := t.TempDir(), t.TempDir()
	write := func(dir, name, doc string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(packDir, "inbox.yaml", "trust: inbox\nkind: command\nread: { argv: [pack] }\n")
	write(flowDir, "inbox.yml", "trust: inbox\nkind: command\nread: { argv: [flow] }\n")
	write(packDir, "other.yaml", "trust: inbox\nkind: command\n")
	loc := approvals.DirLocator{Dirs: []string{flowDir, packDir}}
	p, err := loc.Find(context.Background(), core.DefaultTenant, "inbox")
	if err != nil || p.Read.Argv[0] != "flow" {
		t.Fatalf("flow-local first: %+v %v", p, err)
	}
	if _, err := loc.Find(context.Background(), core.DefaultTenant, "other"); !core.IsCode(err, core.CodeInvalidNode) {
		t.Errorf("a file naming another provider: %v", err)
	}
	if _, err := loc.Find(context.Background(), core.DefaultTenant, "missing"); !core.IsCode(err, core.CodeNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := loc.Find(context.Background(), core.DefaultTenant, "../etc/passwd"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("path traversal: %v", err)
	}
}
