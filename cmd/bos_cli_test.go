package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/access/provision"
	"hop.top/flo/internal/bos"
	"hop.top/flo/internal/core"
)

const (
	cliBoundScope = "business_scopes_00000000000000000000000001"
	cliAgent      = "principals_00000000000000000000000002"
	cliSetFile    = "../internal/access/provision/testdata/set-operations.json"
)

// `flo capabilities --format json` is the peer profile bos reads.
func TestCapabilitiesIsThePeerProfile(t *testing.T) {
	r := newCLIRig(t)
	t.Setenv("FLO_BOS_MAX_STALENESS", "36h")
	var p bos.PeerProfile
	decodeJSON(t, r.mustRun("capabilities", "--format", "json"), &p)
	if p.Tool != "flo" || p.Limits.MaxStalenessSeconds != 36*3600 {
		t.Fatalf("profile = %+v", p)
	}
	levels := map[string]string{}
	for _, c := range p.Contracts {
		levels[c.ID] = c.Level
	}
	if levels["bos.peer/v1"] != "complete" || levels["bos.access/v1"] != "partial" || levels["bos.flo/v1"] != "complete" {
		t.Errorf("contracts = %v", levels)
	}
	if len(p.Represents) != 2 || p.Represents[0] != "operation_grant" || p.Represents[1] != "expiry" {
		t.Errorf("represents = %v", p.Represents)
	}
}

func bosAccessEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FLO_BOS_SCOPE", cliBoundScope)
	t.Setenv("FLO_BOS_PRINCIPALS", "noor="+cliAgent)
	path := filepath.Join(t.TempDir(), "access-set.json")
	old := accessStatePath
	accessStatePath = func(core.TenantID) (string, error) { return path, nil }
	t.Cleanup(func() { accessStatePath = old })
}

// `flo access apply` writes the operations of a set into the grants of the
// profile bound to the principal, as provisioned grants, and `grant set` then
// refuses to touch them.
func TestAccessApplyProvisionsOperationGrants(t *testing.T) {
	r := newCLIRig(t)
	bosAccessEnv(t)
	r.owner()

	var before provision.StateDoc
	decodeJSON(t, r.mustRun("access", "state", "--format", "json"), &before)
	if before.AppliedEpoch != nil {
		t.Fatalf("state before any set = %+v", before)
	}

	var ack provision.Ack
	decodeJSON(t, r.mustRun("access", "apply", "--file", cliSetFile, "--yes", "--format", "json"), &ack)
	if ack.State != "applied" || ack.Epoch != 7 || len(ack.Unrepresentable) != 1 {
		t.Fatalf("ack = %+v", ack)
	}
	var rows []struct{ Profile, Capability, Source string }
	decodeJSON(t, r.mustRun("grant", "list", "noor", "--format", "json"), &rows)
	if len(rows) != 2 || rows[0].Source != "provisioned" || rows[0].Capability != "billing.pay" || rows[1].Capability != "workflow.request-transition" {
		t.Fatalf("grants = %+v", rows)
	}
	if _, _, err := r.run("grant", "set", "noor", "billing.pay", "--yes"); !core.IsCode(err, core.CodeGrantProvisioned) {
		t.Fatalf("grant set over a provisioned grant = %v", err)
	}

	// again: a replay, which changes nothing
	decodeJSON(t, r.mustRun("access", "apply", "--file", cliSetFile, "--yes", "--format", "json"), &ack)
	if ack.State != "replayed" {
		t.Fatalf("second apply = %+v", ack)
	}
	var after provision.StateDoc
	decodeJSON(t, r.mustRun("access", "state", "--format", "json"), &after)
	if after.AppliedEpoch == nil || *after.AppliedEpoch != 7 {
		t.Fatalf("state = %+v", after)
	}
}

func TestAccessApplyRefusesWithoutConfirmationAndBadSets(t *testing.T) {
	r := newCLIRig(t)
	bosAccessEnv(t)
	if _, _, err := r.run("access", "apply", "--file", cliSetFile); !core.IsCode(err, core.CodeConfirmationRequired) {
		t.Fatalf("no --yes and no terminal = %v", err)
	}
	if _, _, err := r.run("access", "apply", "--file", "nope.json", "--yes"); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("missing file = %v", err)
	}
	_, _, err := r.run("access", "apply", "--yes") // standard input is empty
	if !core.IsCode(err, core.CodeSetInvalid) || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("empty set = %v", err)
	}
}

// The bos port is configured by one business scope: with it the engine admits
// through bos, the profile to principal map comes from the file and the
// environment, and the nodes of the bos pack read config.bos.scope.
func TestBosSettings(t *testing.T) {
	file := "bos:\n  scope: business_scopes_1\n  acting_for: principals_9\n  poll: 45s\n  timeout: 5s\n  narrow_owner: true\n" +
		"  principals:\n    noor: principals_1\n    jad: principals_2\n"
	cfg, err := loaded(t, file, map[string]string{"FLO_BOS_PRINCIPALS": "jad=principals_3,ana=principals_4"})
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Bos
	if b == nil || b.Scope != "business_scopes_1" || b.ActingFor != "principals_9" || !b.NarrowOwner || b.Timeout.String() != "5s" {
		t.Fatalf("bos = %+v", b)
	}
	if len(b.Principals) != 2 || b.Principals["jad"] != "principals_3" || b.Principals["ana"] != "principals_4" {
		t.Errorf("principals = %v (the environment beats the file as a whole)", b.Principals)
	}
	fromFile, err := loaded(t, file, map[string]string{"FLO_BOS_PRINCIPALS": ""})
	if err != nil || fromFile.Bos.Principals["noor"] != "principals_1" || fromFile.Bos.Principals["jad"] != "principals_2" {
		t.Errorf("principals from the file = %+v %v", fromFile.Bos, err)
	}
	flagged, err := loaded(t, "", map[string]string{"FLO_BOS_PRINCIPALS": ""}, "-c", "bos.scope=business_scopes_1", "-c", "bos.principals.noor=principals_7")
	if err != nil || flagged.Bos.Principals["noor"] != "principals_7" {
		t.Errorf("principals from -c = %+v %v", flagged.Bos, err)
	}
	if cfg.BosPollEvery.String() != "45s" {
		t.Errorf("poll = %v", cfg.BosPollEvery)
	}
	if ns, _ := cfg.ExprConfig["bos"].(map[string]any); ns["scope"] != "business_scopes_1" {
		t.Errorf("config.bos = %v", cfg.ExprConfig["bos"])
	}
	// no scope, no bos: flo stands alone
	alone, err := loaded(t, "", map[string]string{"FLO_BOS_PRINCIPALS": ""})
	if err != nil || alone.Bos != nil {
		t.Fatalf("without a scope: %+v %v", alone.Bos, err)
	}
	if _, err := loaded(t, file, map[string]string{"FLO_BOS_PRINCIPALS": "nonsense"}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("a malformed map = %v", err)
	}
}
