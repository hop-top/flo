package coretest

import (
	"context"
	"sort"
	"time"

	"hop.top/flo/internal/core"
)

// Tenants is the fake core.Tenants.
type Tenants struct{ db *DB }

// Ensure implements core.Tenants.
func (s *Tenants) Ensure(_ context.Context, t core.TenantID) error {
	if err := core.RequireTenant(t); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.td(t)
	return nil
}

// List implements core.Tenants.
func (s *Tenants) List(context.Context) ([]core.TenantID, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	out := make([]core.TenantID, 0, len(s.db.tenants))
	for t := range s.db.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// GrantStore is the fake core.GrantStore.
type GrantStore struct{ db *DB }

// SetGrant implements core.GrantStore.
func (s *GrantStore) SetGrant(_ context.Context, g core.Grant) (bool, error) {
	if err := core.RequireTenant(g.Tenant); err != nil {
		return false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(g.Tenant)
	for i, x := range d.grants {
		if x.Profile == g.Profile && x.Capability == g.Capability {
			if g.Source != core.GrantProvisioned {
				return false, nil
			}
			same := x.Source == g.Source && ((x.ExpiresAt == nil && g.ExpiresAt == nil) ||
				(x.ExpiresAt != nil && g.ExpiresAt != nil && x.ExpiresAt.Equal(*g.ExpiresAt)))
			d.grants[i].Source, d.grants[i].ExpiresAt = g.Source, g.ExpiresAt
			return !same, nil
		}
	}
	if g.Source == "" {
		g.Source = core.GrantLocal
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = s.db.clock.Now()
	}
	d.grants = append(d.grants, g)
	return true, nil
}

// UnsetGrant implements core.GrantStore.
func (s *GrantStore) UnsetGrant(_ context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(tenant)
	for i, x := range d.grants {
		if x.Profile == profile && x.Capability == capability {
			d.grants = append(d.grants[:i], d.grants[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// ListGrants implements core.GrantStore.
func (s *GrantStore) ListGrants(_ context.Context, tenant core.TenantID, profile string) ([]core.Grant, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var out []core.Grant
	for _, g := range s.db.td(tenant).grants {
		if profile == "" || g.Profile == profile {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		return out[i].Capability < out[j].Capability
	})
	return out, nil
}

// ReceiptStore is the fake core.ReceiptStore.
type ReceiptStore struct{ db *DB }

// PutReceipt implements core.ReceiptStore.
func (s *ReceiptStore) PutReceipt(_ context.Context, r core.Receipt) error {
	if err := core.RequireTenant(r.Tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(r.Tenant)
	for i := range d.rcpts {
		if d.rcpts[i].ID == r.ID {
			d.rcpts[i] = clone(r)
			return nil
		}
	}
	d.rcpts = append(d.rcpts, clone(r))
	return nil
}

// GetReceipt implements core.ReceiptStore.
func (s *ReceiptStore) GetReceipt(_ context.Context, tenant core.TenantID, id core.ReceiptID) (core.Receipt, bool, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Receipt{}, false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	for _, r := range s.db.td(tenant).rcpts {
		if r.ID == id {
			return clone(r), true, nil
		}
	}
	return core.Receipt{}, false, nil
}

// ListReceipts implements core.ReceiptStore.
func (s *ReceiptStore) ListReceipts(_ context.Context, tenant core.TenantID, run core.RunID) ([]core.Receipt, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var out []core.Receipt
	for _, r := range s.db.td(tenant).rcpts {
		if r.Run == run {
			out = append(out, clone(r))
		}
	}
	return out, nil
}

// OutputStore is the fake core.OutputStore.
type OutputStore struct{ db *DB }

// PutOutput implements core.OutputStore.
func (s *OutputStore) PutOutput(_ context.Context, o core.Output) error {
	if err := core.RequireTenant(o.Tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(o.Tenant)
	if o.CreatedAt.IsZero() {
		o.CreatedAt = s.db.clock.Now()
	}
	d.outputs = append(d.outputs, clone(o))
	return nil
}

// GetOutput implements core.OutputStore.
func (s *OutputStore) GetOutput(_ context.Context, tenant core.TenantID, id core.OutputID) (core.Output, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Output{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	for _, o := range s.db.td(tenant).outputs {
		if o.ID == id {
			return clone(o), nil
		}
	}
	return core.Output{}, core.Errorf(core.CodeOutputNotFound, "output %s not found", id)
}

// ListOutputs implements core.OutputStore.
func (s *OutputStore) ListOutputs(_ context.Context, tenant core.TenantID, f core.OutputFilter) ([]core.Output, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	all := s.db.td(tenant).outputs
	var out []core.Output
	for i := len(all) - 1; i >= 0; i-- { // newest first
		o := all[i]
		if (f.Run != "" && o.Run != f.Run) || (f.Kind != "" && o.Kind != f.Kind) || (!f.Since.IsZero() && o.CreatedAt.Before(f.Since)) {
			continue
		}
		out = append(out, clone(o))
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneOutputs implements core.OutputStore.
func (s *OutputStore) PruneOutputs(_ context.Context, tenant core.TenantID, cutoff time.Time) (int, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return 0, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(tenant)
	keep := d.outputs[:0]
	n := 0
	for _, o := range d.outputs {
		if o.CreatedAt.Before(cutoff) {
			n++
			continue
		}
		keep = append(keep, o)
	}
	d.outputs = keep
	return n, nil
}

// TriggerStore is the fake core.TriggerStore.
type TriggerStore struct{ db *DB }

// PutTrigger implements core.TriggerStore.
func (s *TriggerStore) PutTrigger(_ context.Context, t core.TriggerRecord) error {
	if err := core.RequireTenant(t.Tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	t.UpdatedAt = s.db.clock.Now()
	s.db.td(t.Tenant).trigs[t.ID] = clone(t)
	return nil
}

// GetTrigger implements core.TriggerStore.
func (s *TriggerStore) GetTrigger(_ context.Context, tenant core.TenantID, id core.TriggerID) (core.TriggerRecord, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.TriggerRecord{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	t, ok := s.db.td(tenant).trigs[id]
	if !ok {
		return core.TriggerRecord{}, core.Errorf(core.CodeTriggerNotFound, "trigger %s not found", id)
	}
	return clone(t), nil
}

// ListTriggers implements core.TriggerStore.
func (s *TriggerStore) ListTriggers(_ context.Context, tenant core.TenantID, flow string) ([]core.TriggerRecord, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var out []core.TriggerRecord
	for _, t := range s.db.td(tenant).trigs {
		if flow == "" || t.Flow == flow {
			out = append(out, clone(t))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Flow != out[j].Flow {
			return out[i].Flow < out[j].Flow
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

// DeleteTriggers implements core.TriggerStore.
func (s *TriggerStore) DeleteTriggers(_ context.Context, tenant core.TenantID, flow string) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(tenant)
	for id, t := range d.trigs {
		if t.Flow == flow {
			delete(d.trigs, id)
		}
	}
	return nil
}

// AdvanceTrigger implements core.TriggerStore.
func (s *TriggerStore) AdvanceTrigger(_ context.Context, tenant core.TenantID, id core.TriggerID, expected, next *time.Time, job core.JobID) (bool, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(tenant)
	t, ok := d.trigs[id]
	if !ok {
		return false, core.Errorf(core.CodeTriggerNotFound, "trigger %s not found", id)
	}
	if !sameInstant(t.NextAt, expected) {
		return false, nil
	}
	t.NextAt, t.NextJob = clone(next), job
	t.UpdatedAt = s.db.clock.Now()
	d.trigs[id] = t
	return true, nil
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// RecordFire implements core.TriggerStore.
func (s *TriggerStore) RecordFire(_ context.Context, f core.Fire) error {
	if err := core.RequireTenant(f.Tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if f.RecordedAt.IsZero() {
		f.RecordedAt = s.db.clock.Now()
	}
	d := s.db.td(f.Tenant)
	d.fires = append(d.fires, clone(f))
	return nil
}

// ListFires implements core.TriggerStore.
func (s *TriggerStore) ListFires(_ context.Context, tenant core.TenantID, flow string, trigger core.TriggerID, limit int) ([]core.Fire, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	all := s.db.td(tenant).fires
	var out []core.Fire
	for i := len(all) - 1; i >= 0; i-- { // newest first
		f := all[i]
		if (flow != "" && f.Flow != flow) || (trigger != "" && f.Trigger != trigger) {
			continue
		}
		out = append(out, clone(f))
	}
	if limit == 0 {
		limit = defaultListLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PutPlannedFire implements core.TriggerStore.
func (s *TriggerStore) PutPlannedFire(_ context.Context, p core.PlannedFire) error {
	if err := core.RequireTenant(p.Tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.td(p.Tenant).planned[plannedKey{p.Trigger, p.Key}] = clone(p)
	return nil
}

// GetPlannedFire implements core.TriggerStore.
func (s *TriggerStore) GetPlannedFire(_ context.Context, tenant core.TenantID, trigger core.TriggerID, key string) (core.PlannedFire, bool, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.PlannedFire{}, false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	p, ok := s.db.td(tenant).planned[plannedKey{trigger, key}]
	return clone(p), ok, nil
}

// ListPlannedFires implements core.TriggerStore.
func (s *TriggerStore) ListPlannedFires(_ context.Context, tenant core.TenantID, trigger core.TriggerID) ([]core.PlannedFire, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var out []core.PlannedFire
	for k, p := range s.db.td(tenant).planned {
		if k.trigger == trigger {
			out = append(out, clone(p))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// DeletePlannedFire implements core.TriggerStore.
func (s *TriggerStore) DeletePlannedFire(_ context.Context, tenant core.TenantID, trigger core.TriggerID, key string) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	delete(s.db.td(tenant).planned, plannedKey{trigger, key})
	return nil
}
