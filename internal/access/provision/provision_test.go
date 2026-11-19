package provision_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"hop.top/flo/internal/access/grants"
	"hop.top/flo/internal/access/provision"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

var bg = context.Background()

const (
	boundScope = "business_scopes_00000000000000000000000001"
	childScope = "bos://core.business_scopes/business_scopes_00000000000000000000000002"
	agentID    = "principals_00000000000000000000000002"
)

// The fixtures are the access sets of bos's own contract
// (contracts/peer/access/v1/fixtures/set-*.json): the digests in them are the
// ones bos computes.
func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json") //nolint:gosec // a fixture of this package
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func raw(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// edited returns a fixture changed by fn, its digest recomputed for a full set.
func edited(t *testing.T, name string, fn func(m map[string]any)) []byte {
	t.Helper()
	m := fixture(t, name)
	fn(m)
	m["digest"] = provision.ContentDigest(m)
	return raw(t, m)
}

type rig struct {
	fx *coretest.Fixture
	ap *provision.Applier
	g  *grants.Service
}

func newRig(t *testing.T, principals map[string]string) *rig {
	t.Helper()
	fx := coretest.New()
	fx.Clock.Set(time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC))
	cfg := provision.Config{Scope: boundScope, Principals: principals}
	return &rig{fx: fx, ap: provision.New(fx.DB.Grants, fx.Clock, &provision.MemState{}, cfg), g: grants.New(fx.DB.Grants, fx.Clock)}
}

func mapped(t *testing.T) *rig { return newRig(t, map[string]string{"noor": agentID}) }

func (r *rig) covers(t *testing.T, profile, capability string) bool {
	t.Helper()
	ok, err := r.g.Covers(bg, core.DefaultTenant, profile, capability)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// The digest flo computes is the digest bos computes, for sets with and
// without operation grants.
func TestContentDigestIsBosDigest(t *testing.T) {
	for _, name := range []string{"set-full", "set-operations", "set-empty", "set-grant-unknown-property"} {
		m := fixture(t, name)
		if got := provision.ContentDigest(m); got != m["digest"] {
			t.Errorf("%s: digest %s, bos says %v", name, got, m["digest"])
		}
	}
}

func TestApplyWritesOperationsIntoGrantsWithTheirEnd(t *testing.T) {
	r := mapped(t)
	ack, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-operations")))
	if err != nil {
		t.Fatal(err)
	}
	if ack.State != "applied" || ack.Epoch != 7 || ack.Digest != fixture(t, "set-operations")["digest"] {
		t.Fatalf("ack = %+v", ack)
	}
	rows, _ := r.fx.DB.Grants.ListGrants(bg, core.DefaultTenant, "noor")
	if len(rows) != 2 {
		t.Fatalf("grants = %+v", rows)
	}
	end := map[string]time.Time{}
	for _, g := range rows {
		if g.Source != core.GrantProvisioned || g.ExpiresAt == nil {
			t.Fatalf("grant = %+v", g)
		}
		end[g.Capability] = *g.ExpiresAt
	}
	// the set ends one grant at 2026-12-01; the other lasts the staleness limit
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !end["workflow.request-transition"].Equal(want) {
		t.Errorf("request-transition ends %s, want %s", end["workflow.request-transition"], want)
	}
	if want := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC); !end["billing.pay"].Equal(want) {
		t.Errorf("billing.pay ends %s, want %s", end["billing.pay"], want)
	}
	if !r.covers(t, "noor", "billing.pay") || r.covers(t, "noor", "billing.refund") || r.covers(t, "jad", "billing.pay") {
		t.Error("coverage is not the set's")
	}
	// the record read cannot be expressed, and is named
	if len(ack.Unrepresentable) != 1 || ack.Unrepresentable[0].Grant != "access_grants_00000000000000000000000001" {
		t.Fatalf("unrepresentable = %+v", ack.Unrepresentable)
	}
}

// flo denies when the set is older than its staleness limit, and keeps
// working with no bos until then.
func TestStaleSetStopsCovering(t *testing.T) {
	r := mapped(t)
	if _, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-operations"))); err != nil {
		t.Fatal(err)
	}
	r.fx.Clock.Advance(11 * time.Hour) // 23:00; request-transition ended at 00:00
	if !r.covers(t, "noor", "billing.pay") || !r.covers(t, "noor", "workflow.request-transition") {
		t.Fatal("a fresh set does not cover")
	}
	r.fx.Clock.Advance(time.Hour) // 2026-12-01T00:00
	if r.covers(t, "noor", "workflow.request-transition") {
		t.Error("an ended grant still covers")
	}
	r.fx.Clock.Advance(12 * time.Hour) // a day after the set was applied
	if r.covers(t, "noor", "billing.pay") {
		t.Error("a stale set still covers")
	}
}

// A replay of the epoch held confirms it: the age restarts, nothing else moves.
func TestReplayRefreshesTheAge(t *testing.T) {
	r := mapped(t)
	set := raw(t, fixture(t, "set-operations"))
	if _, err := r.ap.Apply(bg, core.DefaultTenant, set); err != nil {
		t.Fatal(err)
	}
	r.fx.Clock.Advance(20 * time.Hour)
	ack, err := r.ap.Apply(bg, core.DefaultTenant, set)
	if err != nil || ack.State != "replayed" || ack.Epoch != 7 {
		t.Fatalf("replay = %+v %v", ack, err)
	}
	r.fx.Clock.Advance(20 * time.Hour) // past the first application's limit
	if !r.covers(t, "noor", "billing.pay") {
		t.Error("a replayed set went stale from its first application")
	}
	st, _ := r.ap.State(bg, core.DefaultTenant)
	if st.AppliedEpoch == nil || *st.AppliedEpoch != 7 || st.MaxStalenessSeconds != 86400 {
		t.Fatalf("state = %+v", st)
	}
}

func TestOlderEpochAndEpochWithAnotherDigestAreStale(t *testing.T) {
	r := mapped(t)
	if _, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-operations"))); err != nil {
		t.Fatal(err)
	}
	older := edited(t, "set-empty", func(m map[string]any) {})
	if _, err := r.ap.Apply(bg, core.DefaultTenant, older); !core.IsCode(err, core.CodeEpochStale) {
		t.Fatalf("older = %v", err)
	}
	same := edited(t, "set-empty", func(m map[string]any) { m["epoch"] = json.Number("7") })
	if _, err := r.ap.Apply(bg, core.DefaultTenant, same); !core.IsCode(err, core.CodeEpochStale) {
		t.Fatalf("same epoch, other digest = %v", err)
	}
	if !r.covers(t, "noor", "billing.pay") {
		t.Error("a refused set changed the grants")
	}
}

// A set that no longer grants an operation revokes it; a local grant stays.
func TestRevokedOperationIsDroppedAndLocalGrantsStay(t *testing.T) {
	r := mapped(t)
	if _, err := r.fx.Grants.Set(bg, core.DefaultTenant, "noor", "mail.read"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-operations"))); err != nil {
		t.Fatal(err)
	}
	next := edited(t, "set-operations", func(m map[string]any) {
		m["epoch"] = json.Number("8")
		m["operations"] = m["operations"].([]any)[:1] // billing.pay stays, the request goes
		m["operations"].([]any)[0].(map[string]any)["operation"] = "billing.refund"
	})
	if _, err := r.ap.Apply(bg, core.DefaultTenant, next); err != nil {
		t.Fatal(err)
	}
	if r.covers(t, "noor", "billing.pay") || r.covers(t, "noor", "workflow.request-transition") || !r.covers(t, "noor", "billing.refund") {
		t.Error("the grants are not the new set's")
	}
	if !r.covers(t, "noor", "mail.read") {
		t.Error("a local grant was dropped")
	}
	// and a revoked grant cannot be unset locally while provisioned
	if _, err := r.fx.Grants.Unset(bg, core.DefaultTenant, "noor", "billing.refund"); !core.IsCode(err, core.CodeGrantProvisioned) {
		t.Errorf("unset of a provisioned grant = %v", err)
	}
}

// Fail closed: nothing flo cannot express is allowed, and each is named.
func TestUnrepresentableIsNamedAndDenied(t *testing.T) {
	// no profile maps to the principal
	r := newRig(t, map[string]string{"noor": "principals_00000000000000000000000099"})
	ack, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-operations")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ack.Unrepresentable) != 3 {
		t.Fatalf("unrepresentable = %+v", ack.Unrepresentable)
	}
	if rows, _ := r.fx.DB.Grants.ListGrants(bg, core.DefaultTenant, ""); len(rows) != 0 {
		t.Fatalf("grants = %+v", rows)
	}
	// a narrower scope needs a grant per scope, which flo does not keep
	r = mapped(t)
	narrow := edited(t, "set-operations", func(m map[string]any) {
		m["operations"].([]any)[0].(map[string]any)["scope"] = childScope
	})
	ack, err = r.ap.Apply(bg, core.DefaultTenant, narrow)
	if err != nil {
		t.Fatal(err)
	}
	if r.covers(t, "noor", "billing.pay") || !r.covers(t, "noor", "workflow.request-transition") || len(ack.Unrepresentable) != 2 {
		t.Fatalf("narrow scope: ack %+v", ack)
	}
}

func TestRefusals(t *testing.T) {
	r := mapped(t)
	cases := []struct {
		name string
		set  []byte
		code core.Code
	}{
		{"digest that is not a digest", func() []byte {
			m := fixture(t, "set-operations")
			m["digest"] = "x"
			return raw(t, m)
		}(), core.CodeSetInvalid},
		{"tampered content", func() []byte {
			m := fixture(t, "set-operations")
			m["operations"].([]any)[0].(map[string]any)["operation"] = "billing.void"
			return raw(t, m)
		}(), core.CodeDigestMismatch},
		{"an operation property flo does not know", raw(t, fixture(t, "set-operation-unknown-property")), core.CodeSetInvalid},
		{"another contract", edited(t, "set-operations", func(m map[string]any) { m["contract"] = "bos.other/v1" }), core.CodeUnsupportedContract},
		{"another scope", edited(t, "set-operations", func(m map[string]any) {
			m["scope"] = "bos://core.business_scopes/business_scopes_00000000000000000000000009"
		}), core.CodeScopeUnknown},
		{"not JSON", []byte("{"), core.CodeSetInvalid},
		{"epoch zero", edited(t, "set-operations", func(m map[string]any) { m["epoch"] = json.Number("0") }), core.CodeSetInvalid},
		{"delta over nothing", raw(t, fixture(t, "set-delta")), core.CodeBaseEpochMismatch},
	}
	for _, c := range cases {
		if _, err := r.ap.Apply(bg, core.DefaultTenant, c.set); !core.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	if rows, _ := r.fx.DB.Grants.ListGrants(bg, core.DefaultTenant, ""); len(rows) != 0 {
		t.Errorf("a refused set wrote grants: %+v", rows)
	}
}

// A delta applies to the set held and produces the digest bos computed for the
// result; operations travel whole.
func TestDeltaAppliesToTheHeldSet(t *testing.T) {
	r := mapped(t)
	if _, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-operations"))); err != nil {
		t.Fatal(err)
	}
	// operations are replaced whole in a delta: this one carries none, so the
	// operations the set held are gone
	ack, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-delta")))
	if err != nil || ack.Epoch != 8 {
		t.Fatalf("delta over the operations set = %+v %v", ack, err)
	}
	if r.covers(t, "noor", "billing.pay") || r.covers(t, "noor", "workflow.request-transition") {
		t.Error("a delta without operations left the operations granted")
	}
	// over set-full (the base bos made it against) its digest is the result's
	r = mapped(t)
	if _, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-full"))); err != nil {
		t.Fatal(err)
	}
	ack, err = r.ap.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-delta")))
	if err != nil || ack.Epoch != 8 || ack.State != "applied" || ack.Digest != fixture(t, "set-delta")["digest"] {
		t.Fatalf("delta = %+v %v", ack, err)
	}
	// a delta whose base is not the epoch held
	stale := fixture(t, "set-delta")
	stale["epoch"], stale["base_epoch"] = json.Number("9"), json.Number("5")
	if _, err := r.ap.Apply(bg, core.DefaultTenant, raw(t, stale)); !core.IsCode(err, core.CodeBaseEpochMismatch) {
		t.Fatalf("delta on another base = %v", err)
	}
}

func TestStateBeforeAnySetHoldsNothing(t *testing.T) {
	r := mapped(t)
	st, err := r.ap.State(bg, core.DefaultTenant)
	if err != nil || st.AppliedEpoch != nil || st.Digest != nil || st.MaxStalenessSeconds != 86400 || st.Contract != provision.Contract {
		t.Fatalf("state = %+v %v", st, err)
	}
}

func TestFileStateSurvivesARestart(t *testing.T) {
	fx := coretest.New()
	fx.Clock.Set(time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC))
	path := t.TempDir() + "/tenant/access-set.json"
	cfg := provision.Config{Scope: boundScope, Principals: map[string]string{"noor": agentID}}
	first := provision.New(fx.DB.Grants, fx.Clock, provision.FileState{Path: path}, cfg)
	if _, err := first.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-full"))); err != nil {
		t.Fatal(err)
	}
	second := provision.New(fx.DB.Grants, fx.Clock, provision.FileState{Path: path}, cfg)
	ack, err := second.Apply(bg, core.DefaultTenant, raw(t, fixture(t, "set-delta")))
	if err != nil || ack.Epoch != 8 {
		t.Fatalf("delta after a restart = %+v %v", ack, err)
	}
}
