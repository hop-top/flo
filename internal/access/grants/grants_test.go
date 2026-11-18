package grants_test

import (
	"context"
	"slices"
	"testing"

	"hop.top/flo/internal/access/grants"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const tenant = core.DefaultTenant

func newService(t *testing.T) (*grants.Service, *coretest.DB) {
	t.Helper()
	clock := coretest.NewClock(coretest.Epoch)
	db := coretest.NewDB(clock)
	return grants.New(db.Grants, clock), db
}

func mustSet(t *testing.T, s *grants.Service, profile, capability string) {
	t.Helper()
	if _, err := s.Set(context.Background(), tenant, profile, capability); err != nil {
		t.Fatalf("set %s %s: %v", profile, capability, err)
	}
}

func TestDenyByDefault(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	ok, err := s.Covers(ctx, tenant, "noor", "mail.read")
	if err != nil || ok {
		t.Fatalf("Covers = %v, %v; want false", ok, err)
	}
	if ok, _ := s.Covers(ctx, tenant, "", "mail.read"); ok {
		t.Error("the empty profile holds a capability")
	}
	ok, err = s.CanUseProfile(ctx, tenant, "jad", "noor")
	if err != nil || ok {
		t.Fatalf("CanUseProfile = %v, %v; want false", ok, err)
	}
	err = s.CheckFlow(ctx, core.GrantCheck{Tenant: tenant, Caller: "jad", RunAs: "noor", Capabilities: []string{"mail.read"}})
	if !core.IsCode(err, core.CodeGrantDenied) || core.ExitCode(err) != 5 {
		t.Fatalf("CheckFlow = %v, want grant_denied exit 5", err)
	}
}

func TestWildcardCoversEverything(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mustSet(t, s, "jad", core.CapabilityAny)
	for _, c := range []string{"mail.send", "vcs.read", "anything.at-all"} {
		if ok, err := s.Covers(ctx, tenant, "jad", c); err != nil || !ok {
			t.Errorf("wildcard does not cover %s: %v %v", c, ok, err)
		}
	}
	if ok, _ := s.Covers(ctx, tenant, "noor", "mail.send"); ok {
		t.Error("the wildcard of one profile covers another")
	}
	// the owner may run a flow as another profile only when that profile holds the capabilities
	mustSet(t, s, "jad", grants.UseCapability("noor"))
	err := s.CheckFlow(ctx, core.GrantCheck{Tenant: tenant, Caller: "jad", RunAs: "noor", Capabilities: []string{"mail.send"}})
	if !core.IsCode(err, core.CodeGrantDenied) {
		t.Fatalf("noor holds nothing: %v", err)
	}
}

func TestCheckFlowMissingListsCapabilities(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mustSet(t, s, "noor", "mail.read")
	err := s.CheckFlow(ctx, core.GrantCheck{
		Tenant: tenant, Caller: "noor",
		Capabilities: []string{"mail.read", "mail.send", "vcs.read", "mail.send"},
	})
	var ce *core.Error
	if !core.IsCode(err, core.CodeGrantDenied) {
		t.Fatalf("CheckFlow = %v", err)
	}
	ce = core.Wrap(core.CodeInternal, err)
	missing, _ := ce.Detail["missing"].([]string)
	if !slices.Equal(missing, []string{"mail.send", "vcs.read"}) {
		t.Errorf("missing = %v", missing)
	}
	if ce.Suggestion == "" {
		t.Error("no suggestion")
	}
	// all held: ok; no capabilities: ok
	mustSet(t, s, "noor", "mail.send")
	mustSet(t, s, "noor", "vcs.read")
	if err := s.CheckFlow(ctx, core.GrantCheck{Tenant: tenant, Caller: "noor", Capabilities: []string{"mail.read", "mail.send", "vcs.read"}}); err != nil {
		t.Errorf("held: %v", err)
	}
	if err := s.CheckFlow(ctx, core.GrantCheck{Tenant: tenant, Caller: "noor"}); err != nil {
		t.Errorf("none needed: %v", err)
	}
}

func TestRunAsNeedsUseGrant(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mustSet(t, s, "noor", "mail.read")
	check := core.GrantCheck{Tenant: tenant, Caller: "jad", RunAs: "noor", Capabilities: []string{"mail.read"}}
	if err := s.CheckFlow(ctx, check); !core.IsCode(err, core.CodeGrantDenied) {
		t.Fatalf("jad may not act as noor: %v", err)
	}
	mustSet(t, s, "jad", grants.UseCapability("noor"))
	if err := s.CheckFlow(ctx, check); err != nil {
		t.Fatalf("with the use grant: %v", err)
	}
	if ok, _ := s.CanUseProfile(ctx, tenant, "noor", "noor"); !ok {
		t.Error("a profile may use itself")
	}
}

func TestSetUnsetIdempotent(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	changed, err := s.Set(ctx, tenant, "noor", "mail.read")
	if err != nil || !changed {
		t.Fatalf("first set: %v %v", changed, err)
	}
	changed, err = s.Set(ctx, tenant, "noor", "mail.read")
	if err != nil || changed {
		t.Fatalf("second set: %v %v", changed, err)
	}
	changed, err = s.Unset(ctx, tenant, "noor", "mail.read")
	if err != nil || !changed {
		t.Fatalf("first unset: %v %v", changed, err)
	}
	changed, err = s.Unset(ctx, tenant, "noor", "mail.read")
	if err != nil || changed {
		t.Fatalf("second unset: %v %v", changed, err)
	}
	rows, err := s.List(ctx, tenant, "noor")
	if err != nil || len(rows) != 0 {
		t.Fatalf("list = %v %v", rows, err)
	}
}

func TestProvisionedRefusedExit4(t *testing.T) {
	s, db := newService(t)
	ctx := context.Background()
	if _, err := db.Grants.SetGrant(ctx, core.Grant{Tenant: tenant, Profile: "noor", Capability: "mail.read", Source: core.GrantProvisioned}); err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() (bool, error){
		"set":   func() (bool, error) { return s.Set(ctx, tenant, "noor", "mail.read") },
		"unset": func() (bool, error) { return s.Unset(ctx, tenant, "noor", "mail.read") },
	} {
		changed, err := op()
		if changed || !core.IsCode(err, core.CodeGrantProvisioned) || core.ExitCode(err) != 4 {
			t.Errorf("%s provisioned: changed=%v err=%v, want grant_provisioned exit 4", name, changed, err)
		}
	}
	// still there, and still covers
	if ok, _ := s.Covers(ctx, tenant, "noor", "mail.read"); !ok {
		t.Error("a refused unset removed the grant")
	}
	// a local grant of the same profile is not affected
	mustSet(t, s, "noor", "mail.send")
}

func TestSetValidatesCapability(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	for _, bad := range []string{"", "mail", "mail send", ".read", "mail.", "profile.use:"} {
		if _, err := s.Set(ctx, tenant, "noor", bad); !core.IsCode(err, core.CodeUsage) {
			t.Errorf("Set(%q) = %v, want usage", bad, err)
		}
	}
	if _, err := s.Set(ctx, tenant, "", "mail.read"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("empty profile: %v", err)
	}
	if _, err := s.Set(ctx, "Bad Tenant", "noor", "mail.read"); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("bad tenant: %v", err)
	}
	for _, good := range []string{"*", "mail.read", "profile.use:noor", "ai.generate.text"} {
		if _, err := s.Set(ctx, tenant, "noor", good); err != nil {
			t.Errorf("Set(%q) = %v", good, err)
		}
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.Set(ctx, "acme", "noor", "mail.read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Covers(ctx, tenant, "noor", "mail.read"); ok {
		t.Error("a grant of one tenant covers another")
	}
}

func TestSeedOwner(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	changed, err := s.SeedOwner(ctx, tenant, "jad")
	if err != nil || !changed {
		t.Fatalf("seed: %v %v", changed, err)
	}
	changed, err = s.SeedOwner(ctx, tenant, "jad")
	if err != nil || changed {
		t.Fatalf("seed again: %v %v", changed, err)
	}
	if ok, _ := s.Covers(ctx, tenant, "jad", "mail.send"); !ok {
		t.Error("the owner does not hold every capability")
	}
}

func TestSyncEngineIsTheUnionOfPackCapabilities(t *testing.T) {
	s, db := newService(t)
	ctx := context.Background()
	engine := "flo-engine"
	changed, err := s.SyncEngine(ctx, tenant, engine, []string{"vcs.read", "mail.read", "vcs.read", "*"})
	if err != nil || !changed {
		t.Fatalf("sync: %v %v", changed, err)
	}
	caps := func() []string {
		rows, _ := s.List(ctx, tenant, engine)
		var out []string
		for _, r := range rows {
			out = append(out, r.Capability)
		}
		return out
	}
	if got := caps(); !slices.Equal(got, []string{"mail.read", "vcs.read"}) {
		t.Fatalf("engine holds %v (the wildcard must never be synced)", got)
	}
	if changed, _ := s.SyncEngine(ctx, tenant, engine, []string{"mail.read", "vcs.read"}); changed {
		t.Error("syncing the same set changed something")
	}
	// a pack went away; a provisioned grant stays
	if _, err := db.Grants.SetGrant(ctx, core.Grant{Tenant: tenant, Profile: engine, Capability: "cal.read", Source: core.GrantProvisioned}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SyncEngine(ctx, tenant, engine, []string{"mail.read"}); err != nil || !changed {
		t.Fatalf("resync: %v %v", changed, err)
	}
	if got := caps(); !slices.Equal(got, []string{"cal.read", "mail.read"}) {
		t.Fatalf("engine holds %v", got)
	}
}
