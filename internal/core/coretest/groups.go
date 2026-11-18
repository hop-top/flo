package coretest

import (
	"context"
	"sort"
	"time"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// ReservationTTL is how long an unconsumed slot reservation holds.
const ReservationTTL = time.Minute

// Groups is the fake core.Groups: occupancy is derived from the admitted,
// unfinished runs plus live reservations.
type Groups struct{ db *DB }

// occupied lists the admitted active runs of a group, oldest first, and the
// number of live reservations. The caller holds db.mu.
func (g *Groups) occupied(d *tenantData, group string) (runs []*runRec, reserved int) {
	now := g.db.clock.Now()
	for _, r := range d.runs {
		if r.run.Group == group && r.run.Admitted && r.run.Status.Active() {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].seq < runs[j].seq })
	for id, rv := range d.resv {
		if rv.group != group {
			continue
		}
		if now.Sub(rv.at) > ReservationTTL {
			delete(d.resv, id)
			continue
		}
		reserved++
	}
	return runs, reserved
}

// Admit implements core.Groups.
func (g *Groups) Admit(_ context.Context, req core.GroupRequest) (core.GroupDecision, error) {
	if err := core.RequireTenant(req.Tenant); err != nil {
		return core.GroupDecision{}, err
	}
	max := req.Max
	if max < 1 {
		max = specrun.DefaultMax
	}
	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	d := g.db.td(req.Tenant)
	if r, ok := d.runs[req.Run]; ok { // asked again for an existing run
		if r.run.Admitted {
			return core.GroupDecision{Action: core.GroupRun}, nil
		}
		return core.GroupDecision{Action: core.GroupQueue}, nil
	}
	if _, ok := d.resv[req.Run]; ok {
		return core.GroupDecision{Action: core.GroupRun}, nil
	}
	runs, reserved := g.occupied(d, req.Group)
	active := len(runs) + reserved
	reserve := func() { d.resv[req.Run] = reservation{group: req.Group, at: g.db.clock.Now()} }
	if active < max {
		reserve()
		return core.GroupDecision{Action: core.GroupRun}, nil
	}
	switch req.OnOverlap {
	case specrun.OverlapSkip:
		return core.GroupDecision{Action: core.GroupSkip}, nil
	case specrun.OverlapCancelPrevious:
		reserve()
		var cancel []core.RunID
		for i := 0; i < len(runs) && len(runs)-len(cancel)+reserved >= max; i++ {
			cancel = append(cancel, runs[i].run.ID)
		}
		return core.GroupDecision{Action: core.GroupRun, Cancel: cancel}, nil
	}
	return core.GroupDecision{Action: core.GroupQueue}, nil
}

// Abandon implements core.Groups.
func (g *Groups) Abandon(_ context.Context, tenant core.TenantID, _ string, run core.RunID) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	delete(g.db.td(tenant).resv, run)
	return nil
}

// Release implements core.Groups.
func (g *Groups) Release(_ context.Context, tenant core.TenantID, group string) ([]core.RunID, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err
	}
	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	d := g.db.td(tenant)
	var queued []*runRec
	for _, r := range d.runs {
		if r.run.Group == group && !r.run.Admitted && r.run.Status == core.RunQueued {
			queued = append(queued, r)
		}
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].seq < queued[j].seq })
	var promoted []core.RunID
	for _, r := range queued {
		runs, reserved := g.occupied(d, group)
		max := r.run.GroupMax
		if max < 1 {
			max = specrun.DefaultMax
		}
		if len(runs)+reserved >= max {
			break
		}
		r.run.Admitted = true
		r.run.UpdatedAt = g.db.clock.Now()
		promoted = append(promoted, r.run.ID)
	}
	return promoted, nil
}
