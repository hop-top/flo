package coretest

import (
	"context"
	"encoding/json"
	"sort"

	"hop.top/flo/internal/core"
)

const defaultListLimit = 100

// DefaultMaxAttempts is the run.advance attempt limit a run gets when its
// creator names none.
const DefaultMaxAttempts = 5

// RunStore is the fake core.RunStore.
type RunStore struct{ db *DB }

func notFoundRun(id core.RunID) error {
	return core.Errorf(core.CodeRunNotFound, "run %s not found", id)
}

func (s *RunStore) rec(tenant core.TenantID, id core.RunID) (*runRec, error) {
	r, ok := s.db.td(tenant).runs[id]
	if !ok {
		return nil, notFoundRun(id)
	}
	return r, nil
}

// CreateRun implements core.RunStore.
func (s *RunStore) CreateRun(_ context.Context, in core.NewRun) (core.Run, bool, error) {
	if err := core.RequireTenant(in.Tenant); err != nil {
		return core.Run{}, false, err
	}
	if in.ID == "" {
		return core.Run{}, false, core.Errorf(core.CodeUsage, "a run needs an id")
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	d := s.db.td(in.Tenant)
	if in.IdemKey != "" {
		if r := findIdem(d, in.Flow.Name, in.IdemKey); r != nil {
			return clone(r.run), false, nil
		}
	}
	if _, ok := d.runs[in.ID]; ok {
		return core.Run{}, false, core.Errorf(core.CodeDuplicate, "run %s exists", in.ID)
	}
	at := in.At
	if at.IsZero() {
		at = s.db.clock.Now()
	}
	max := in.MaxAttempts
	if max == 0 {
		max = DefaultMaxAttempts
	}
	run := core.Run{
		ID: in.ID, Tenant: in.Tenant, Flow: in.Flow, Status: core.RunQueued,
		Vars: clone(in.Vars), Trigger: clone(in.Trigger), IdemKey: in.IdemKey,
		Group: in.Group, GroupMax: in.GroupMax, Admitted: in.Admitted,
		StartedBy: in.StartedBy, RunAs: in.RunAs, Record: in.Record, MaxAttempts: max,
		CreatedAt: at, UpdatedAt: at,
	}
	d.runs[in.ID] = &runRec{run: run, seq: s.db.next()}
	delete(d.resv, in.ID) // the reservation is consumed by the run
	return clone(run), true, nil
}

func findIdem(d *tenantData, flow, key string) *runRec {
	var found *runRec
	for _, r := range d.runs {
		if r.run.IdemKey == key && r.run.Flow.Name == flow && (found == nil || r.seq < found.seq) {
			found = r
		}
	}
	return found
}

// LookupIdem implements core.RunStore.
func (s *RunStore) LookupIdem(_ context.Context, tenant core.TenantID, flow, key string) (core.Run, bool, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Run{}, false, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if r := findIdem(s.db.td(tenant), flow, key); r != nil && key != "" {
		return clone(r.run), true, nil
	}
	return core.Run{}, false, nil
}

// GetRun implements core.RunStore.
func (s *RunStore) GetRun(_ context.Context, tenant core.TenantID, id core.RunID) (core.Run, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Run{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, err := s.rec(tenant, id)
	if err != nil {
		return core.Run{}, err
	}
	return clone(r.run), nil
}

// ListRuns implements core.RunStore.
func (s *RunStore) ListRuns(_ context.Context, tenant core.TenantID, f core.RunFilter) ([]core.Run, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var recs []*runRec
	for _, r := range s.db.td(tenant).runs {
		if f.Flow != "" && r.run.Flow.Name != f.Flow {
			continue
		}
		if f.Group != "" && r.run.Group != f.Group {
			continue
		}
		if !f.Since.IsZero() && r.run.CreatedAt.Before(f.Since) {
			continue
		}
		if len(f.Statuses) > 0 && !containsStatus(f.Statuses, r.run.Status) {
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
	out := make([]core.Run, len(recs))
	for i, r := range recs {
		out[i] = clone(r.run)
	}
	return out, nil
}

func containsStatus(set []core.RunStatus, s core.RunStatus) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// UpdateRun implements core.RunStore.
func (s *RunStore) UpdateRun(_ context.Context, tenant core.TenantID, id core.RunID, expect []core.RunStatus, p core.RunPatch) (core.Run, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Run{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, err := s.rec(tenant, id)
	if err != nil {
		return core.Run{}, err
	}
	if r.run.Status.Terminal() {
		return core.Run{}, core.Errorf(core.CodeRunStateConflict, "run %s is %s", id, r.run.Status)
	}
	if len(expect) > 0 && !containsStatus(expect, r.run.Status) {
		return core.Run{}, core.Errorf(core.CodeRunStateConflict, "run %s is %s, want %v", id, r.run.Status, expect)
	}
	if p.Status != nil {
		if !p.Status.Valid() || p.Status.Terminal() {
			return core.Run{}, core.Errorf(core.CodeUsage, "a patch cannot set status %q; finish the run instead", *p.Status)
		}
		r.run.Status = *p.Status
	}
	applyPatch(&r.run, p)
	r.run.UpdatedAt = s.db.clock.Now()
	return clone(r.run), nil
}

func applyPatch(run *core.Run, p core.RunPatch) {
	if p.WaitingFor != nil {
		run.WaitingFor = *p.WaitingFor
	}
	if p.ExpiresAt != nil {
		run.ExpiresAt = ptrOrNil(*p.ExpiresAt)
	}
	if p.NextRetryAt != nil {
		run.NextRetryAt = ptrOrNil(*p.NextRetryAt)
	}
	if p.StartedAt != nil {
		run.StartedAt = ptrOrNil(*p.StartedAt)
	}
	if p.Attempts != nil {
		run.Attempts = *p.Attempts
	}
	if p.Usage != nil {
		run.Usage = *p.Usage
	}
	if p.TaskRef != nil {
		run.TaskRef = *p.TaskRef
	}
	if p.Admitted != nil {
		run.Admitted = *p.Admitted
	}
	if p.StateSnapshot != nil {
		run.StateSnapshot = clone(p.StateSnapshot)
	}
}

// RequestCancel implements core.RunStore.
func (s *RunStore) RequestCancel(_ context.Context, tenant core.TenantID, id core.RunID) (core.Run, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Run{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, err := s.rec(tenant, id)
	if err != nil {
		return core.Run{}, err
	}
	if !r.run.Status.Terminal() && !r.run.CancelRequested {
		r.run.CancelRequested = true
		r.run.UpdatedAt = s.db.clock.Now()
	}
	return clone(r.run), nil
}

// PutStep implements core.RunStore.
func (s *RunStore) PutStep(_ context.Context, tenant core.TenantID, st core.StepRun) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if _, err := s.rec(tenant, st.Run); err != nil {
		return err
	}
	d := s.db.td(tenant)
	now := s.db.clock.Now()
	steps := d.steps[st.Run]
	for i := range steps {
		if steps[i].ID != st.ID {
			continue
		}
		if steps[i].Status.Terminal() {
			return nil // terminal records are never rewritten
		}
		st.CreatedAt = steps[i].CreatedAt
		st.UpdatedAt = now
		steps[i] = clone(st)
		return nil
	}
	if st.CreatedAt.IsZero() {
		st.CreatedAt = now
	}
	st.UpdatedAt = now
	st.Tenant = tenant
	d.steps[st.Run] = append(steps, clone(st))
	return nil
}

// GetStep implements core.RunStore.
func (s *RunStore) GetStep(_ context.Context, tenant core.TenantID, run core.RunID, id core.StepRunID) (core.StepRun, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.StepRun{}, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	for _, st := range s.db.td(tenant).steps[run] {
		if st.ID == id {
			return clone(st), nil
		}
	}
	return core.StepRun{}, core.Errorf(core.CodeRunNotFound, "step record %s of run %s not found", id, run)
}

// ListSteps implements core.RunStore.
func (s *RunStore) ListSteps(_ context.Context, tenant core.TenantID, run core.RunID) ([]core.StepRun, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if _, err := s.rec(tenant, run); err != nil {
		return nil, err
	}
	return clone(s.db.td(tenant).steps[run]), nil
}

// StateStore is the fake core.StateStore.
type StateStore struct{ db *DB }

// Snapshot implements core.StateStore.
func (s *StateStore) Snapshot(_ context.Context, tenant core.TenantID, flow string) (map[string]any, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	out := clone(s.db.td(tenant).state[flow])
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// Stage implements core.StateStore.
func (s *StateStore) Stage(_ context.Context, tenant core.TenantID, run core.RunID, key string, value any) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return core.Wrap(core.CodeInvalidFlow, err)
	}
	if len(b) > core.MaxStateValueBytes {
		return core.Errorf(core.CodeStateTooLarge, "state key %q is %d bytes, over the %d limit", key, len(b), core.MaxStateValueBytes)
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, err := (&RunStore{s.db}).rec(tenant, run)
	if err != nil {
		return err
	}
	if r.run.Status.Terminal() {
		return core.Errorf(core.CodeRunStateConflict, "run %s is %s", run, r.run.Status)
	}
	d := s.db.td(tenant)
	if d.staged[run] == nil {
		d.staged[run] = map[string]any{}
	}
	d.staged[run][key] = clone(value)
	return nil
}

// Staged implements core.StateStore.
func (s *StateStore) Staged(_ context.Context, tenant core.TenantID, run core.RunID) (map[string]any, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	out := clone(s.db.td(tenant).staged[run])
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// FinishRun implements core.StateStore.
func (s *StateStore) FinishRun(_ context.Context, tenant core.TenantID, id core.RunID, fin core.RunFinish) (core.Run, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Run{}, err
	}
	if !fin.Status.Terminal() {
		return core.Run{}, core.Errorf(core.CodeUsage, "FinishRun needs a terminal status, got %q", fin.Status)
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	r, err := (&RunStore{s.db}).rec(tenant, id)
	if err != nil {
		return core.Run{}, err
	}
	if r.run.Status.Terminal() {
		if r.run.Status == fin.Status {
			return clone(r.run), nil
		}
		return core.Run{}, core.Errorf(core.CodeRunStateConflict, "run %s is already %s", id, r.run.Status)
	}
	d := s.db.td(tenant)
	if fin.Status == core.RunSucceeded {
		if d.state[r.run.Flow.Name] == nil {
			d.state[r.run.Flow.Name] = map[string]any{}
		}
		for k, v := range d.staged[id] {
			d.state[r.run.Flow.Name][k] = clone(v)
		}
	}
	delete(d.staged, id)
	at := fin.At
	if at.IsZero() {
		at = s.db.clock.Now()
	}
	r.run.Status = fin.Status
	r.run.Failure = clone(fin.Failure)
	r.run.Result = clone(fin.Result)
	r.run.Usage = fin.Usage
	r.run.WaitingFor = ""
	r.run.ExpiresAt = nil
	r.run.NextRetryAt = nil
	r.run.FinishedAt = &at
	r.run.UpdatedAt = s.db.clock.Now()
	return clone(r.run), nil
}
