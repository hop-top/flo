package sqlite

import (
	"context"
	"database/sql"
	"time"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// ReservationTTL is how long an unconsumed slot reservation holds.
const ReservationTTL = time.Minute

// Groups is the SQLite core.Groups. Occupancy is derived from the admitted,
// unfinished runs plus live reservations (the group_claims table); there is
// no counter to drift. Every decision runs in one write transaction, so two
// simultaneous fires cannot both take the last slot, in this process or
// another.
type Groups struct{ s *Store }

// occupancy lists the admitted active runs of a group, oldest first, and the
// number of live reservations. Expired reservations are dropped.
func (g *Groups) occupancy(ctx context.Context, tx *sql.Tx, group string) (runs []core.RunID, reserved int, err error) {
	now := g.s.clock.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_claims WHERE at < ?`, now-int64(ReservationTTL)); err != nil {
		return nil, 0, internal(err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM runs WHERE grp = ? AND admitted = 1 AND status NOT IN ('succeeded', 'failed', 'cancelled') ORDER BY seq`, group)
	if err != nil {
		return nil, 0, internal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, 0, internal(err)
		}
		runs = append(runs, core.RunID(id))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, internal(err)
	}
	_ = rows.Close()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_claims WHERE grp = ?`, group).Scan(&reserved); err != nil {
		return nil, 0, internal(err)
	}
	return runs, reserved, nil
}

// Admit implements core.Groups.
func (g *Groups) Admit(ctx context.Context, req core.GroupRequest) (core.GroupDecision, error) {
	if err := core.RequireTenant(req.Tenant); err != nil {
		return core.GroupDecision{}, err
	}
	max := req.Max
	if max < 1 {
		max = specrun.DefaultMax
	}
	var dec core.GroupDecision
	err := g.s.tx(ctx, req.Tenant, func(tx *sql.Tx) error {
		var admitted int
		switch err := tx.QueryRowContext(ctx, `SELECT admitted FROM runs WHERE id = ?`, string(req.Run)).Scan(&admitted); {
		case err == nil: // asked again for an existing run
			dec = core.GroupDecision{Action: core.GroupQueue}
			if admitted == 1 {
				dec.Action = core.GroupRun
			}
			return nil
		case err != sql.ErrNoRows: //nolint:errorlint // sentinel compare of a Scan result
			return internal(err)
		}
		runs, reserved, err := g.occupancy(ctx, tx, req.Group)
		if err != nil {
			return err
		}
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_claims WHERE run_id = ?`, string(req.Run)).Scan(&held); err != nil {
			return internal(err)
		}
		if held > 0 {
			dec = core.GroupDecision{Action: core.GroupRun}
			return nil
		}
		reserve := func() error {
			_, err := tx.ExecContext(ctx, `INSERT INTO group_claims (run_id, grp, at) VALUES (?, ?, ?)`,
				string(req.Run), req.Group, g.s.clock.Now().UnixNano())
			return internal(err)
		}
		if len(runs)+reserved < max {
			dec = core.GroupDecision{Action: core.GroupRun}
			return reserve()
		}
		switch req.OnOverlap {
		case specrun.OverlapSkip:
			dec = core.GroupDecision{Action: core.GroupSkip}
			return nil
		case specrun.OverlapCancelPrevious:
			if err := reserve(); err != nil {
				return err
			}
			dec = core.GroupDecision{Action: core.GroupRun}
			for i := 0; i < len(runs) && len(runs)-len(dec.Cancel)+reserved >= max; i++ {
				dec.Cancel = append(dec.Cancel, runs[i])
			}
			return nil
		}
		dec = core.GroupDecision{Action: core.GroupQueue}
		return nil
	})
	return dec, err
}

// Abandon implements core.Groups.
func (g *Groups) Abandon(ctx context.Context, tenant core.TenantID, _ string, run core.RunID) error {
	return g.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM group_claims WHERE run_id = ?`, string(run))
		return internal(err)
	})
}

// Release implements core.Groups.
func (g *Groups) Release(ctx context.Context, tenant core.TenantID, group string) ([]core.RunID, error) {
	var promoted []core.RunID
	err := g.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM runs WHERE grp = ? AND admitted = 0 AND status = 'queued' ORDER BY seq`, group)
		if err != nil {
			return internal(err)
		}
		var queued []core.RunID
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return internal(err)
			}
			queued = append(queued, core.RunID(id))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return internal(err)
		}
		_ = rows.Close()
		for _, id := range queued {
			runs, reserved, err := g.occupancy(ctx, tx, group)
			if err != nil {
				return err
			}
			r, err := loadRun(ctx, tx, id)
			if err != nil {
				return err
			}
			max := r.GroupMax
			if max < 1 {
				max = specrun.DefaultMax
			}
			if len(runs)+reserved >= max {
				break
			}
			r.Admitted = true
			r.UpdatedAt = g.s.clock.Now()
			if err := saveRun(ctx, tx, r); err != nil {
				return err
			}
			promoted = append(promoted, id)
		}
		return nil
	})
	return promoted, err
}

var (
	_ core.RunStore      = (*RunStore)(nil)
	_ core.StateStore    = (*StateStore)(nil)
	_ core.FlowStore     = (*FlowStore)(nil)
	_ core.ApprovalStore = (*ApprovalStore)(nil)
	_ core.GrantStore    = (*GrantStore)(nil)
	_ core.ReceiptStore  = (*ReceiptStore)(nil)
	_ core.OutputStore   = (*OutputStore)(nil)
	_ core.TriggerStore  = (*TriggerStore)(nil)
	_ core.Groups        = (*Groups)(nil)
)
