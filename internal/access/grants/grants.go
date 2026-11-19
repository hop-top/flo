// Package grants is the local implementation of core.Grants: deny by
// default, the wildcard "*" covers every capability, a provisioned grant is
// refused by set and unset (node.md §12, flow.md V18).
package grants

import (
	"context"
	"sort"
	"strings"

	"hop.top/flo/internal/core"
)

// ProfileUsePrefix is the capability that says "may name this profile as
// run_as": `profile.use:<profile>`. A profile may always use itself. Callers
// ask Service.CanUseProfile, never build the string.
const ProfileUsePrefix = "profile.use:"

// Service is the local core.Grants over a GrantStore.
type Service struct {
	store core.GrantStore
	clock core.Clock
}

var _ core.Grants = (*Service)(nil)

// New returns the grants service over store.
func New(store core.GrantStore, clock core.Clock) *Service {
	return &Service{store: store, clock: clock}
}

// UseCapability is the grant that lets a caller name profile as run_as.
func UseCapability(profile string) string { return ProfileUsePrefix + profile }

func (s *Service) held(ctx context.Context, tenant core.TenantID, profile string) (map[string]bool, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	rows, err := s.store.ListGrants(ctx, tenant, profile)
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	now := s.clock.Now()
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.Live(now) {
			out[r.Capability] = true
		}
	}
	return out, nil
}

// Covers implements core.Grants. An empty profile holds nothing.
func (s *Service) Covers(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	if profile == "" {
		return false, nil
	}
	h, err := s.held(ctx, tenant, profile)
	if err != nil {
		return false, err
	}
	return h[core.CapabilityAny] || h[capability], nil
}

// CanUseProfile implements core.Grants.
func (s *Service) CanUseProfile(ctx context.Context, tenant core.TenantID, caller, profile string) (bool, error) {
	if caller == "" {
		return false, nil
	}
	if caller == profile {
		return true, nil
	}
	return s.Covers(ctx, tenant, caller, UseCapability(profile))
}

// CheckFlow implements core.Grants (flow.md V18): the caller may use the
// flow's run_as, and that profile holds every capability. The error is
// grant_denied; for a missing capability its Detail "missing" lists them.
func (s *Service) CheckFlow(ctx context.Context, c core.GrantCheck) error {
	runAs := c.RunAs
	if runAs == "" {
		runAs = c.Caller
	}
	ok, err := s.CanUseProfile(ctx, c.Tenant, c.Caller, runAs)
	if err != nil {
		return err
	}
	if !ok {
		return core.Errorf(core.CodeGrantDenied, "%s may not act as %s", c.Caller, runAs).
			WithDetail("caller", c.Caller).WithDetail("run_as", runAs).
			WithSuggestion("flo grant set " + c.Caller + " " + UseCapability(runAs))
	}
	h, err := s.held(ctx, c.Tenant, runAs)
	if err != nil {
		return err
	}
	if h[core.CapabilityAny] {
		return nil
	}
	var missing []string
	seen := map[string]bool{}
	for _, capability := range c.Capabilities {
		if h[capability] || seen[capability] {
			continue
		}
		seen[capability] = true
		missing = append(missing, capability)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return core.Errorf(core.CodeGrantDenied, "profile %s lacks %s", runAs, strings.Join(missing, ", ")).
		WithDetail("run_as", runAs).WithDetail("missing", missing).
		WithSuggestion("flo grant set " + runAs + " " + missing[0])
}

// Valid reports whether capability is a grantable string: the wildcard, a
// profile.use grant, or `<domain>.<verb>`.
func Valid(capability string) bool {
	if capability == "" || strings.ContainsAny(capability, " \t\n") {
		return false
	}
	if capability == core.CapabilityAny {
		return true
	}
	if rest, ok := strings.CutPrefix(capability, ProfileUsePrefix); ok {
		return rest != ""
	}
	domain, verb, ok := strings.Cut(capability, ".")
	return ok && domain != "" && verb != ""
}

func checkArgs(tenant core.TenantID, profile, capability string) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if profile == "" {
		return core.New(core.CodeUsage, "grant: profile is empty")
	}
	if !Valid(capability) {
		return core.Errorf(core.CodeUsage, "grant: %q is not a capability (want <domain>.<verb>, profile.use:<profile> or *)", capability)
	}
	return nil
}

// provisioned reports a grant of the governance backend.
func (s *Service) provisioned(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	rows, err := s.store.ListGrants(ctx, tenant, profile)
	if err != nil {
		return false, err //nolint:wrapcheck // port error passes through
	}
	for _, r := range rows {
		if r.Capability == capability && r.Source == core.GrantProvisioned {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) refuseProvisioned(ctx context.Context, tenant core.TenantID, profile, capability string) error {
	p, err := s.provisioned(ctx, tenant, profile, capability)
	if err != nil {
		return err
	}
	if p {
		return core.Errorf(core.CodeGrantProvisioned, "%s %s is provisioned by the governance backend", profile, capability).
			WithDetail("profile", profile).WithDetail("capability", capability).
			WithSuggestion("change it in the governance backend")
	}
	return nil
}

// Set implements core.Grants. Granting what is already granted is a no-op
// (changed false); a provisioned grant is grant_provisioned.
func (s *Service) Set(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	if err := checkArgs(tenant, profile, capability); err != nil {
		return false, err
	}
	if err := s.refuseProvisioned(ctx, tenant, profile, capability); err != nil {
		return false, err
	}
	return s.store.SetGrant(ctx, core.Grant{ //nolint:wrapcheck // port error passes through
		Tenant: tenant, Profile: profile, Capability: capability, Source: core.GrantLocal, CreatedAt: s.clock.Now(),
	})
}

// Unset implements core.Grants. Revoking what is not granted is a no-op.
func (s *Service) Unset(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	if err := checkArgs(tenant, profile, capability); err != nil {
		return false, err
	}
	if err := s.refuseProvisioned(ctx, tenant, profile, capability); err != nil {
		return false, err
	}
	return s.store.UnsetGrant(ctx, tenant, profile, capability) //nolint:wrapcheck // port error passes through
}

// List implements core.Grants.
func (s *Service) List(ctx context.Context, tenant core.TenantID, profile string) ([]core.Grant, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	return s.store.ListGrants(ctx, tenant, profile) //nolint:wrapcheck // port error passes through
}

// SeedOwner grants the owner profile the wildcard (phase 1: the owner holds
// every capability). Idempotent; a provisioned wildcard is left alone.
func (s *Service) SeedOwner(ctx context.Context, tenant core.TenantID, owner string) (bool, error) {
	changed, err := s.Set(ctx, tenant, owner, core.CapabilityAny)
	if core.IsCode(err, core.CodeGrantProvisioned) {
		return false, nil
	}
	return changed, err
}

// SyncEngine makes the engine profile hold exactly the capabilities the
// installed packs declare (phase 1: the union of the packs' node
// capabilities): missing ones are granted, local ones no pack declares any
// more are revoked, provisioned ones are left alone. It reports whether
// anything changed.
func (s *Service) SyncEngine(ctx context.Context, tenant core.TenantID, engine string, declared []string) (bool, error) {
	want := map[string]bool{}
	for _, c := range declared {
		if c != "" && c != core.CapabilityAny {
			want[c] = true
		}
	}
	have, err := s.List(ctx, tenant, engine)
	if err != nil {
		return false, err
	}
	changed := false
	for _, g := range have {
		if g.Source != core.GrantLocal || strings.HasPrefix(g.Capability, ProfileUsePrefix) || want[g.Capability] {
			delete(want, g.Capability)
			continue
		}
		ok, err := s.store.UnsetGrant(ctx, tenant, engine, g.Capability)
		if err != nil {
			return changed, err //nolint:wrapcheck // port error passes through
		}
		changed = changed || ok
	}
	add := make([]string, 0, len(want))
	for c := range want {
		add = append(add, c)
	}
	sort.Strings(add)
	for _, c := range add {
		ok, err := s.Set(ctx, tenant, engine, c)
		if core.IsCode(err, core.CodeGrantProvisioned) {
			continue
		}
		if err != nil {
			return changed, err
		}
		changed = changed || ok
	}
	return changed, nil
}
