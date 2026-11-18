package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hop.top/flo/internal/core"
)

// TriggerStore is the SQLite core.TriggerStore.
type TriggerStore struct{ s *Store }

func decTrigger(doc string) (core.TriggerRecord, error) {
	var t core.TriggerRecord
	if err := dec([]byte(doc), &t); err != nil {
		return core.TriggerRecord{}, internal(err)
	}
	return t, nil
}

func saveTrigger(ctx context.Context, q execer, t core.TriggerRecord) error {
	b, err := enc(t)
	if err != nil {
		return internal(err)
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO triggers (id, flow, idx, next_at, doc) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET flow = excluded.flow, idx = excluded.idx, next_at = excluded.next_at, doc = excluded.doc`,
		string(t.ID), t.Flow, t.Index, tnp(t.NextAt), string(b))
	return internal(err)
}

func loadTrigger(ctx context.Context, q execer, id core.TriggerID) (core.TriggerRecord, error) {
	var doc string
	err := q.QueryRowContext(ctx, `SELECT doc FROM triggers WHERE id = ?`, string(id)).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.TriggerRecord{}, core.Errorf(core.CodeTriggerNotFound, "trigger %s not found", id)
	}
	if err != nil {
		return core.TriggerRecord{}, internal(err)
	}
	return decTrigger(doc)
}

// PutTrigger implements core.TriggerStore.
func (s *TriggerStore) PutTrigger(ctx context.Context, t core.TriggerRecord) error {
	if err := core.RequireTenant(t.Tenant); err != nil {
		return err
	}
	t.UpdatedAt = s.s.clock.Now()
	return s.s.tx(ctx, t.Tenant, func(tx *sql.Tx) error { return saveTrigger(ctx, tx, t) })
}

// GetTrigger implements core.TriggerStore.
func (s *TriggerStore) GetTrigger(ctx context.Context, tenant core.TenantID, id core.TriggerID) (core.TriggerRecord, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.TriggerRecord{}, err
	}
	return loadTrigger(ctx, q, id)
}

// ListTriggers implements core.TriggerStore.
func (s *TriggerStore) ListTriggers(ctx context.Context, tenant core.TenantID, flow string) ([]core.TriggerRecord, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	if flow != "" {
		where, args = append(where, "flow = ?"), append(args, flow)
	}
	return queryDocs(ctx, q, "triggers", where, args, "flow, idx, id", 0, decTrigger)
}

// DeleteTriggers implements core.TriggerStore.
func (s *TriggerStore) DeleteTriggers(ctx context.Context, tenant core.TenantID, flow string) error {
	return s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM triggers WHERE flow = ?`, flow)
		return internal(err)
	})
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// AdvanceTrigger implements core.TriggerStore.
func (s *TriggerStore) AdvanceTrigger(ctx context.Context, tenant core.TenantID, id core.TriggerID, expected, next *time.Time, job core.JobID) (bool, error) {
	won := false
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		t, err := loadTrigger(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sameInstant(t.NextAt, expected) {
			return nil
		}
		t.NextAt, t.NextJob = next, job
		t.UpdatedAt = s.s.clock.Now()
		won = true
		return saveTrigger(ctx, tx, t)
	})
	return won, err
}

func decFire(doc string) (core.Fire, error) {
	var f core.Fire
	if err := dec([]byte(doc), &f); err != nil {
		return core.Fire{}, internal(err)
	}
	return f, nil
}

// RecordFire implements core.TriggerStore.
func (s *TriggerStore) RecordFire(ctx context.Context, f core.Fire) error {
	if err := core.RequireTenant(f.Tenant); err != nil {
		return err
	}
	if f.RecordedAt.IsZero() {
		f.RecordedAt = s.s.clock.Now()
	}
	b, err := enc(f)
	if err != nil {
		return internal(err)
	}
	return s.s.tx(ctx, f.Tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fires (flow, trig, doc) VALUES (?, ?, ?)`, f.Flow, string(f.Trigger), string(b))
		return internal(err)
	})
}

// ListFires implements core.TriggerStore.
func (s *TriggerStore) ListFires(ctx context.Context, tenant core.TenantID, flow string, trigger core.TriggerID, limit int) ([]core.Fire, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	if flow != "" {
		where, args = append(where, "flow = ?"), append(args, flow)
	}
	if trigger != "" {
		where, args = append(where, "trig = ?"), append(args, string(trigger))
	}
	if limit == 0 {
		limit = defaultListLimit
	}
	return queryDocs(ctx, q, "fires", where, args, "seq DESC", limit, decFire)
}

func decPlanned(doc string) (core.PlannedFire, error) {
	var p core.PlannedFire
	if err := dec([]byte(doc), &p); err != nil {
		return core.PlannedFire{}, internal(err)
	}
	return p, nil
}

// PutPlannedFire implements core.TriggerStore.
func (s *TriggerStore) PutPlannedFire(ctx context.Context, p core.PlannedFire) error {
	if err := core.RequireTenant(p.Tenant); err != nil {
		return err
	}
	b, err := enc(p)
	if err != nil {
		return internal(err)
	}
	return s.s.tx(ctx, p.Tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO planned_fires (trig, key, flow, at, doc) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (trig, key) DO UPDATE SET flow = excluded.flow, at = excluded.at, doc = excluded.doc`,
			string(p.Trigger), p.Key, p.Flow, p.At.UnixNano(), string(b))
		return internal(err)
	})
}

// GetPlannedFire implements core.TriggerStore.
func (s *TriggerStore) GetPlannedFire(ctx context.Context, tenant core.TenantID, trigger core.TriggerID, key string) (core.PlannedFire, bool, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.PlannedFire{}, false, err
	}
	var doc string
	err = q.QueryRowContext(ctx, `SELECT doc FROM planned_fires WHERE trig = ? AND key = ?`, string(trigger), key).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PlannedFire{}, false, nil
	}
	if err != nil {
		return core.PlannedFire{}, false, internal(err)
	}
	p, err := decPlanned(doc)
	return p, err == nil, err
}

// ListPlannedFires implements core.TriggerStore.
func (s *TriggerStore) ListPlannedFires(ctx context.Context, tenant core.TenantID, trigger core.TriggerID) ([]core.PlannedFire, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return queryDocs(ctx, q, "planned_fires", []string{"trig = ?"}, []any{string(trigger)}, "at, key", 0, decPlanned)
}

// DeletePlannedFire implements core.TriggerStore.
func (s *TriggerStore) DeletePlannedFire(ctx context.Context, tenant core.TenantID, trigger core.TriggerID, key string) error {
	return s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM planned_fires WHERE trig = ? AND key = ?`, string(trigger), key)
		return internal(err)
	})
}
