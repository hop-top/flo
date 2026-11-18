package coretest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"

	"hop.top/flo/internal/core"
)

// ProfileUsePrefix is the capability prefix this fake reads as "may name this
// profile as run_as": `profile.use:<profile>`. The real service decides its
// own representation; callers use core.Grants.CanUseProfile, never the string.
const ProfileUsePrefix = "profile.use:"

// Grants is the fake core.Grants over a GrantStore: deny by default, the
// wildcard covers everything, a provisioned grant refuses set and unset.
type Grants struct {
	Store core.GrantStore
	Clock core.Clock
}

func (g *Grants) held(ctx context.Context, tenant core.TenantID, profile string) (map[string]bool, error) {
	rows, err := g.Store.ListGrants(ctx, tenant, profile)
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.Capability] = true
	}
	return out, nil
}

// Covers implements core.Grants.
func (g *Grants) Covers(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	h, err := g.held(ctx, tenant, profile)
	if err != nil {
		return false, err
	}
	return h[core.CapabilityAny] || h[capability], nil
}

// CanUseProfile implements core.Grants.
func (g *Grants) CanUseProfile(ctx context.Context, tenant core.TenantID, caller, profile string) (bool, error) {
	if caller == profile {
		return true, nil
	}
	return g.Covers(ctx, tenant, caller, ProfileUsePrefix+profile)
}

// CheckFlow implements core.Grants.
func (g *Grants) CheckFlow(ctx context.Context, c core.GrantCheck) error {
	runAs := c.RunAs
	if runAs == "" {
		runAs = c.Caller
	}
	ok, err := g.CanUseProfile(ctx, c.Tenant, c.Caller, runAs)
	if err != nil {
		return err
	}
	if !ok {
		return core.Errorf(core.CodeGrantDenied, "%s may not act as %s", c.Caller, runAs).WithDetail("run_as", runAs)
	}
	var missing []string
	for _, capability := range c.Capabilities {
		cov, err := g.Covers(ctx, c.Tenant, runAs, capability)
		if err != nil {
			return err
		}
		if !cov {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		return core.Errorf(core.CodeGrantDenied, "profile %s lacks %s", runAs, strings.Join(missing, ", ")).
			WithDetail("missing", missing).
			WithSuggestion("flo grant set " + runAs + " " + missing[0])
	}
	return nil
}

func (g *Grants) provisioned(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	rows, err := g.Store.ListGrants(ctx, tenant, profile)
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

// Set implements core.Grants.
func (g *Grants) Set(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	if p, err := g.provisioned(ctx, tenant, profile, capability); err != nil || p {
		if err != nil {
			return false, err
		}
		return false, core.Errorf(core.CodeGrantProvisioned, "%s %s is provisioned by the governance backend", profile, capability)
	}
	return g.Store.SetGrant(ctx, core.Grant{Tenant: tenant, Profile: profile, Capability: capability, Source: core.GrantLocal, CreatedAt: g.Clock.Now()}) //nolint:wrapcheck // port error passes through
}

// Unset implements core.Grants.
func (g *Grants) Unset(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	if p, err := g.provisioned(ctx, tenant, profile, capability); err != nil || p {
		if err != nil {
			return false, err
		}
		return false, core.Errorf(core.CodeGrantProvisioned, "%s %s is provisioned by the governance backend", profile, capability)
	}
	return g.Store.UnsetGrant(ctx, tenant, profile, capability) //nolint:wrapcheck // port error passes through
}

// List implements core.Grants.
func (g *Grants) List(ctx context.Context, tenant core.TenantID, profile string) ([]core.Grant, error) {
	return g.Store.ListGrants(ctx, tenant, profile) //nolint:wrapcheck // port error passes through
}

// Provision stores a grant as provisioned by a governance backend.
func (g *Grants) Provision(ctx context.Context, tenant core.TenantID, profile, capability string) error {
	_, err := g.Store.SetGrant(ctx, core.Grant{Tenant: tenant, Profile: profile, Capability: capability, Source: core.GrantProvisioned, CreatedAt: g.Clock.Now()})
	return err //nolint:wrapcheck // port error passes through
}

// Admission is the fake core.Admission: it allows unless Policy says
// otherwise, records receipts, and keeps a deferred decision deferred until
// Resolve. The digest is a SHA-256 over the request's JSON, not the
// governance backend's canonical form.
type Admission struct {
	Receipts core.ReceiptStore
	IDs      core.IDs
	Clock    core.Clock
	// Policy decides a request; nil allows everything.
	Policy func(core.AdmissionRequest) (core.AdmissionResult, string)

	mu    sync.Mutex
	calls []core.AdmissionRequest
}

func digest(req core.AdmissionRequest) string {
	b, _ := json.Marshal(map[string]any{
		"v": 1, "operation": req.Operation, "scope": req.Tenant, "actor": req.Agent,
		"acting_for": req.Principal, "input": req.Input,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Admit implements core.Admission.
func (a *Admission) Admit(ctx context.Context, req core.AdmissionRequest) (core.AdmissionDecision, error) {
	a.mu.Lock()
	a.calls = append(a.calls, clone(req))
	a.mu.Unlock()
	existing, err := a.Receipts.ListReceipts(ctx, req.Tenant, req.Run)
	if err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	for _, r := range existing {
		if r.StepRun == req.StepRun && r.Digest == digest(req) {
			return core.AdmissionDecision{Result: r.Decision, Receipt: r, Reason: r.Reason}, nil
		}
	}
	res, reason := core.AdmissionAllow, ""
	if a.Policy != nil {
		res, reason = a.Policy(req)
	}
	r := core.Receipt{
		ID: core.NewReceiptID(a.IDs), Tenant: req.Tenant, Run: req.Run, StepRun: req.StepRun, StepID: req.Purpose.StepID,
		Digest: digest(req), Decision: res, Agent: req.Agent, Principal: req.Principal, Operation: req.Operation,
		Capabilities: req.Capabilities, SideEffect: req.SideEffect, Backend: "local", Reason: reason, At: a.Clock.Now(),
	}
	if err := a.Receipts.PutReceipt(ctx, r); err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	return core.AdmissionDecision{Result: res, Receipt: r, Reason: reason}, nil
}

// Resolve decides a deferred receipt, as the backend would.
func (a *Admission) Resolve(ctx context.Context, tenant core.TenantID, id core.ReceiptID, res core.AdmissionResult, reason string) error {
	r, ok, err := a.Receipts.GetReceipt(ctx, tenant, id)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	if !ok {
		return core.Errorf(core.CodeNotFound, "receipt %s not found", id)
	}
	r.Decision, r.Reason = res, reason
	return a.Receipts.PutReceipt(ctx, r) //nolint:wrapcheck // port error passes through
}

// Calls returns the requests received, in order.
func (a *Admission) Calls() []core.AdmissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return clone(a.calls)
}

// Gate is the fake core.ActivationGate: it allows unless Err is set.
type Gate struct {
	mu  sync.Mutex
	err error
}

// Refuse makes Allow return err (nil allows again).
func (g *Gate) Refuse(err error) { g.mu.Lock(); g.err = err; g.mu.Unlock() }

// Allow implements core.ActivationGate.
func (g *Gate) Allow(context.Context, core.TenantID, core.FlowVersion) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// TrigSync is the fake core.TriggerSync: it records the calls.
type TrigSync struct {
	mu       sync.Mutex
	Err      error
	synced   []core.FlowVersion
	unsynced []string
}

// Sync implements core.TriggerSync.
func (t *TrigSync) Sync(_ context.Context, _ core.TenantID, v core.FlowVersion) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.synced = append(t.synced, clone(v))
	return t.Err
}

// Unsync implements core.TriggerSync.
func (t *TrigSync) Unsync(_ context.Context, _ core.TenantID, flow string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unsynced = append(t.unsynced, flow)
	return t.Err
}

// Synced returns the versions Sync was called with.
func (t *TrigSync) Synced() []core.FlowVersion {
	t.mu.Lock()
	defer t.mu.Unlock()
	return clone(t.synced)
}

// Unsynced returns the flows Unsync was called with.
func (t *TrigSync) Unsynced() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.unsynced...)
}

// Starter is the fake core.Starter: the real core.NewStarter over the
// fakes, with its requests recorded.
type Starter struct {
	inner core.Starter
	mu    sync.Mutex
	reqs  []core.StartRequest
}

// NewStarter returns a recording Starter over d.
func NewStarter(d core.StarterDeps) *Starter { return &Starter{inner: core.NewStarter(d)} }

// Start implements core.Starter.
func (s *Starter) Start(ctx context.Context, req core.StartRequest) (core.StartResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, clone(req))
	s.mu.Unlock()
	return s.inner.Start(ctx, req) //nolint:wrapcheck // passes through
}

// Release implements core.Starter.
func (s *Starter) Release(ctx context.Context, run core.Run) error {
	return s.inner.Release(ctx, run) //nolint:wrapcheck // passes through
}

// Requests returns the start requests received, in order.
func (s *Starter) Requests() []core.StartRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.reqs)
}
