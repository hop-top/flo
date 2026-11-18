package coretest

import (
	"context"
	"sort"
	"time"

	"hop.top/flo/internal/core"
)

func ptrOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// FlowStore is the fake core.FlowStore.
type FlowStore struct{ db *DB }

// PutVersion implements core.FlowStore.
func (s *FlowStore) PutVersion(_ context.Context, in core.NewFlowVersion) (core.FlowVersion, bool, error) {
	if err := core.RequireTenant(in.Tenant); err != nil {
		return core.FlowVersion{}, false, err
	}
	if in.Name == "" || in.Hash == "" {
		return core.FlowVersion{}, false, core.Errorf(core.CodeUsage, "a flow version needs a name and a hash")
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(in.Tenant)
	for _, v := range d.flows[in.Name] {
		if v.Hash == in.Hash {
			return clone(v), false, nil
		}
	}
	at := in.At
	if at.IsZero() {
		at = s.db.clock.Now()
	}
	v := core.FlowVersion{
		Tenant: in.Tenant, Name: in.Name, Version: in.Version, Hash: in.Hash, Seq: len(d.flows[in.Name]) + 1,
		Source: clone(in.Source), Bundle: in.Bundle, State: core.FlowDraft, CreatedAt: at,
	}
	d.flows[in.Name] = append(d.flows[in.Name], v)
	return clone(v), true, nil
}

func flowNotFound(name string) error {
	return core.Errorf(core.CodeFlowNotFound, "flow %q not found", name).
		WithSuggestion("flo flow apply -f <file>")
}

// GetVersion implements core.FlowStore.
func (s *FlowStore) GetVersion(_ context.Context, tenant core.TenantID, name, ref string) (core.FlowVersion, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.FlowVersion{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	vs := s.db.td(tenant).flows[name]
	if len(vs) == 0 {
		return core.FlowVersion{}, flowNotFound(name)
	}
	for i := len(vs) - 1; i >= 0; i-- { // newest first
		if ref == "" || vs[i].Hash == ref || vs[i].Version == ref {
			return clone(vs[i]), nil
		}
	}
	return core.FlowVersion{}, core.Errorf(core.CodeFlowNotFound, "flow %q has no version %q", name, ref)
}

// Active implements core.FlowStore.
func (s *FlowStore) Active(_ context.Context, tenant core.TenantID, name string) (core.FlowVersion, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.FlowVersion{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	vs := s.db.td(tenant).flows[name]
	if len(vs) == 0 {
		return core.FlowVersion{}, flowNotFound(name)
	}
	for _, v := range vs {
		if v.State == core.FlowActive {
			return clone(v), nil
		}
	}
	return core.FlowVersion{}, core.Errorf(core.CodeFlowNotActive, "flow %q has no active version", name).
		WithSuggestion("flo flow set " + name + " --state active")
}

// ListFlows implements core.FlowStore.
func (s *FlowStore) ListFlows(_ context.Context, tenant core.TenantID) ([]core.FlowSummary, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var out []core.FlowSummary
	for name, vs := range s.db.td(tenant).flows {
		sum := core.FlowSummary{Name: name, Latest: clone(vs[len(vs)-1])}
		for _, v := range vs {
			switch v.State {
			case core.FlowActive:
				c := clone(v)
				sum.Active = &c
			case core.FlowPaused:
				sum.Paused = true
			}
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListVersions implements core.FlowStore.
func (s *FlowStore) ListVersions(_ context.Context, tenant core.TenantID, name string) ([]core.FlowVersion, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	vs := s.db.td(tenant).flows[name]
	if len(vs) == 0 {
		return nil, flowNotFound(name)
	}
	out := make([]core.FlowVersion, len(vs))
	for i, v := range vs {
		out[len(vs)-1-i] = clone(v)
	}
	return out, nil
}

// SetState implements core.FlowStore.
func (s *FlowStore) SetState(_ context.Context, tenant core.TenantID, name, hash string, state core.FlowState, at time.Time) (core.FlowVersion, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.FlowVersion{}, err
	}
	if state != core.FlowActive && state != core.FlowPaused {
		return core.FlowVersion{}, core.Errorf(core.CodeUsage, "a version is set active or paused, not %q", state)
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	vs := s.db.td(tenant).flows[name]
	idx := -1
	for i := range vs {
		if vs[i].Hash == hash {
			idx = i
		}
	}
	if idx < 0 {
		return core.FlowVersion{}, core.Errorf(core.CodeFlowNotFound, "flow %q has no version %q", name, hash)
	}
	if at.IsZero() {
		at = s.db.clock.Now()
	}
	if vs[idx].State == state {
		return clone(vs[idx]), nil
	}
	if state == core.FlowPaused && vs[idx].State != core.FlowActive {
		return core.FlowVersion{}, core.Errorf(core.CodeFlowNotActive, "only an active version is paused; %s is %s", hash, vs[idx].State)
	}
	if state == core.FlowActive {
		for i := range vs {
			if i != idx && (vs[i].State == core.FlowActive || vs[i].State == core.FlowPaused) {
				vs[i].State = core.FlowRetired
			}
		}
		vs[idx].ActivatedAt = &at
	}
	vs[idx].State = state
	return clone(vs[idx]), nil
}

// ApprovalStore is the fake core.ApprovalStore.
type ApprovalStore struct{ db *DB }

// CreateApproval implements core.ApprovalStore.
func (s *ApprovalStore) CreateApproval(_ context.Context, a core.Approval) (core.Approval, bool, error) {
	if err := core.RequireTenant(a.Tenant); err != nil {
		return core.Approval{}, false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(a.Tenant)
	for _, r := range d.apprs {
		if r.a.Run == a.Run && r.a.StepRun == a.StepRun {
			return clone(r.a), false, nil
		}
	}
	if _, ok := d.apprs[a.ID]; ok {
		return core.Approval{}, false, core.Errorf(core.CodeDuplicate, "approval %s exists", a.ID)
	}
	now := s.db.clock.Now()
	a.Version = 1
	if a.Status == "" {
		a.Status = core.ApprovalOpen
	}
	a.CreatedAt, a.UpdatedAt = now, now
	d.apprs[a.ID] = &apprRec{a: clone(a), seq: s.db.next()}
	return clone(a), true, nil
}

// GetApproval implements core.ApprovalStore.
func (s *ApprovalStore) GetApproval(_ context.Context, tenant core.TenantID, id core.ApprovalID) (core.Approval, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Approval{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, ok := s.db.td(tenant).apprs[id]
	if !ok {
		return core.Approval{}, core.Errorf(core.CodeApprovalNotFound, "approval %s not found", id)
	}
	return clone(r.a), nil
}

// ListApprovals implements core.ApprovalStore.
func (s *ApprovalStore) ListApprovals(_ context.Context, tenant core.TenantID, f core.ApprovalFilter) ([]core.Approval, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var recs []*apprRec
	for _, r := range s.db.td(tenant).apprs {
		if (f.Status != "" && r.a.Status != f.Status) || (f.Run != "" && r.a.Run != f.Run) || (f.StepID != "" && r.a.StepID != f.StepID) {
			continue
		}
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].seq > recs[j].seq })
	limit := f.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	if len(recs) > limit {
		recs = recs[:limit]
	}
	out := make([]core.Approval, len(recs))
	for i, r := range recs {
		out[i] = clone(r.a)
	}
	return out, nil
}

// UpdateApproval implements core.ApprovalStore.
func (s *ApprovalStore) UpdateApproval(_ context.Context, a core.Approval) (core.Approval, error) {
	if err := core.RequireTenant(a.Tenant); err != nil {
		return core.Approval{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, ok := s.db.td(a.Tenant).apprs[a.ID]
	if !ok {
		return core.Approval{}, core.Errorf(core.CodeApprovalNotFound, "approval %s not found", a.ID)
	}
	if r.a.Version != a.Version {
		return core.Approval{}, core.Errorf(core.CodeApprovalConflict, "approval %s is at version %d, not %d", a.ID, r.a.Version, a.Version).
			WithDetail("current", r.a.Version)
	}
	a.Version++
	a.UpdatedAt = s.db.clock.Now()
	a.CreatedAt = r.a.CreatedAt
	r.a = clone(a)
	return clone(a), nil
}
