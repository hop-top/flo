// Package admission is the local implementation of core.Admission
// (node.md §10.2): allow what the acting profile's grants cover, deny the
// rest, and record a receipt either way. Phase 1 has no deferral; a
// governance backend that defers replaces this port.
package admission

import (
	"context"
	"sort"
	"strings"

	"hop.top/flo/internal/access/digest"
	"hop.top/flo/internal/core"
)

// Backend is the name a local receipt carries.
const Backend = "local"

// Service is the local core.Admission.
type Service struct {
	grants   core.Grants
	receipts core.ReceiptStore
	ids      core.IDs
	clock    core.Clock
}

var _ core.Admission = (*Service)(nil)

// New returns the local admission over the grants and the receipt store.
func New(grants core.Grants, receipts core.ReceiptStore, ids core.IDs, clock core.Clock) *Service {
	return &Service{grants: grants, receipts: receipts, ids: ids, clock: clock}
}

// Digest returns the request digest of an admission request:
// operation is the node type as ns.name, scope the tenant, actor the run_as
// profile, acting_for the principal, input the rendered input.
func Digest(req core.AdmissionRequest) (string, error) {
	r, err := digest.ForRun(req.Tenant, digest.Operation("", req.Operation), req.Agent, req.Principal, req.Input)
	if err != nil {
		return "", err //nolint:wrapcheck // *core.Error
	}
	d, err := r.Digest()
	if err != nil {
		return "", core.Wrap(core.CodeInternal, err)
	}
	return d, nil
}

// Admit implements core.Admission. Asking again for the same (run, step
// run, digest) returns the decision of the receipt already recorded.
func (s *Service) Admit(ctx context.Context, req core.AdmissionRequest) (core.AdmissionDecision, error) {
	if err := core.RequireTenant(req.Tenant); err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // *core.Error
	}
	d, err := Digest(req)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	existing, err := s.receipts.ListReceipts(ctx, req.Tenant, req.Run)
	if err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	for _, r := range existing {
		if r.StepRun == req.StepRun && r.Digest == d {
			return core.AdmissionDecision{Result: r.Decision, Receipt: r, Reason: r.Reason}, nil
		}
	}
	result, reason, err := s.decide(ctx, req)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	r := core.Receipt{
		ID: core.NewReceiptID(s.ids), Tenant: req.Tenant, Run: req.Run, StepRun: req.StepRun, StepID: req.Purpose.StepID,
		Digest: d, Decision: result, Agent: req.Agent, Principal: req.Principal, Operation: req.Operation,
		Capabilities: req.Capabilities, SideEffect: req.SideEffect, Backend: Backend, Reason: reason, At: s.clock.Now(),
	}
	if err := s.receipts.PutReceipt(ctx, r); err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	return core.AdmissionDecision{Result: result, Receipt: r, Reason: reason}, nil
}

func (s *Service) decide(ctx context.Context, req core.AdmissionRequest) (core.AdmissionResult, string, error) {
	if req.Agent == "" {
		return core.AdmissionDeny, "no acting profile", nil
	}
	var missing []string
	seen := map[string]bool{}
	for _, c := range req.Capabilities {
		if seen[c] {
			continue
		}
		seen[c] = true
		ok, err := s.grants.Covers(ctx, req.Tenant, req.Agent, c)
		if err != nil {
			return "", "", err //nolint:wrapcheck // port error passes through
		}
		if !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return core.AdmissionDeny, "profile " + req.Agent + " lacks " + strings.Join(missing, ", "), nil
	}
	return core.AdmissionAllow, "", nil
}
