package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"hop.top/flo/internal/core"
)

const (
	defaultListLimit = 100
	// DefaultMaxAttempts is the run.advance attempt limit of a run whose
	// creator names none.
	DefaultMaxAttempts = 5
)

// RunStore is the SQLite core.RunStore.
type RunStore struct{ s *Store }

func notFoundRun(id core.RunID) error {
	return core.Errorf(core.CodeRunNotFound, "run %s not found", id)
}

func decRun(doc string) (core.Run, error) {
	var r core.Run
	if err := dec([]byte(doc), &r); err != nil {
		return core.Run{}, internal(err)
	}
	return r, nil
}

func loadRun(ctx context.Context, q execer, id core.RunID) (core.Run, error) {
	var doc string
	err := q.QueryRowContext(ctx, `SELECT doc FROM runs WHERE id = ?`, string(id)).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Run{}, notFoundRun(id)
	}
	if err != nil {
		return core.Run{}, internal(err)
	}
	return decRun(doc)
}

func saveRun(ctx context.Context, q execer, r core.Run) error {
	doc, err := enc(r)
	if err != nil {
		return internal(err)
	}
	_, err = q.ExecContext(ctx, `UPDATE runs SET status = ?, grp = ?, admitted = ?, doc = ? WHERE id = ?`,
		string(r.Status), r.Group, b2i(r.Admitted), string(doc), string(r.ID))
	return internal(err)
}

func idemRun(ctx context.Context, q execer, flow, key string) (core.Run, bool, error) {
	var doc string
	err := q.QueryRowContext(ctx, `SELECT doc FROM runs WHERE flow = ? AND idem_key = ?`, flow, key).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Run{}, false, nil
	}
	if err != nil {
		return core.Run{}, false, internal(err)
	}
	r, err := decRun(doc)
	return r, err == nil, err
}

// CreateRun implements core.RunStore.
func (s *RunStore) CreateRun(ctx context.Context, in core.NewRun) (core.Run, bool, error) {
	if err := core.RequireTenant(in.Tenant); err != nil {
		return core.Run{}, false, err
	}
	if in.ID == "" {
		return core.Run{}, false, core.Errorf(core.CodeUsage, "a run needs an id")
	}
	var out core.Run
	created := false
	err := s.s.tx(ctx, in.Tenant, func(tx *sql.Tx) error {
		if in.IdemKey != "" {
			if r, ok, err := idemRun(ctx, tx, in.Flow.Name, in.IdemKey); err != nil || ok {
				out = r
				return err
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id = ?`, string(in.ID)).Scan(&n); err != nil {
			return internal(err)
		}
		if n > 0 {
			return core.Errorf(core.CodeDuplicate, "run %s exists", in.ID)
		}
		at := in.At
		if at.IsZero() {
			at = s.s.clock.Now()
		}
		max := in.MaxAttempts
		if max == 0 {
			max = DefaultMaxAttempts
		}
		run := core.Run{
			ID: in.ID, Tenant: in.Tenant, Flow: in.Flow, Status: core.RunQueued,
			Vars: in.Vars, Trigger: in.Trigger, IdemKey: in.IdemKey,
			Group: in.Group, GroupMax: in.GroupMax, Admitted: in.Admitted,
			StartedBy: in.StartedBy, RunAs: in.RunAs, Record: in.Record, MaxAttempts: max,
			CreatedAt: at, UpdatedAt: at,
		}
		doc, err := enc(run)
		if err != nil {
			return internal(err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, flow, idem_key, status, grp, admitted, created_at, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			string(run.ID), run.Flow.Name, run.IdemKey, string(run.Status), run.Group, b2i(run.Admitted), at.UnixNano(), string(doc)); err != nil {
			return internal(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_claims WHERE run_id = ?`, string(run.ID)); err != nil {
			return internal(err)
		}
		created = true
		out, err = decRun(string(doc)) // what a read returns, normalized
		return err
	})
	if err != nil {
		return core.Run{}, false, err
	}
	return out, created, nil
}

// LookupIdem implements core.RunStore.
func (s *RunStore) LookupIdem(ctx context.Context, tenant core.TenantID, flow, key string) (core.Run, bool, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.Run{}, false, err
	}
	if key == "" {
		return core.Run{}, false, nil
	}
	return idemRun(ctx, q, flow, key)
}

// GetRun implements core.RunStore.
func (s *RunStore) GetRun(ctx context.Context, tenant core.TenantID, id core.RunID) (core.Run, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.Run{}, err
	}
	return loadRun(ctx, q, id)
}

// ListRuns implements core.RunStore.
func (s *RunStore) ListRuns(ctx context.Context, tenant core.TenantID, f core.RunFilter) ([]core.Run, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	if f.Flow != "" {
		where, args = append(where, "flow = ?"), append(args, f.Flow)
	}
	if f.Group != "" {
		where, args = append(where, "grp = ?"), append(args, f.Group)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "created_at >= ?"), append(args, f.Since.UnixNano())
	}
	if len(f.Statuses) > 0 {
		where = append(where, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	}
	return queryDocs(ctx, q, "runs", where, args, "seq DESC", f.Limit, decRun)
}

// queryDocs runs a SELECT of the doc column and decodes each row.
func queryDocs[T any](ctx context.Context, q execer, table string, where []string, args []any, order string, limit int, f func(string) (T, error)) ([]T, error) {
	query := `SELECT doc FROM ` + table
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY ` + order
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []T
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, internal(err)
		}
		v, err := f(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, internal(rows.Err())
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func hasStatus(set []core.RunStatus, s core.RunStatus) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// UpdateRun implements core.RunStore.
func (s *RunStore) UpdateRun(ctx context.Context, tenant core.TenantID, id core.RunID, expect []core.RunStatus, p core.RunPatch) (core.Run, error) {
	var out core.Run
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Status.Terminal() {
			return core.Errorf(core.CodeRunStateConflict, "run %s is %s", id, r.Status)
		}
		if len(expect) > 0 && !hasStatus(expect, r.Status) {
			return core.Errorf(core.CodeRunStateConflict, "run %s is %s, want %v", id, r.Status, expect)
		}
		if p.Status != nil {
			if !p.Status.Valid() || p.Status.Terminal() {
				return core.Errorf(core.CodeUsage, "a patch cannot set status %q; finish the run instead", *p.Status)
			}
			r.Status = *p.Status
		}
		applyPatch(&r, p)
		r.UpdatedAt = s.s.clock.Now()
		if err := saveRun(ctx, tx, r); err != nil {
			return err
		}
		out, err = roundTrip(r)
		return err
	})
	return out, err
}

// roundTrip returns r as a later read would.
func roundTrip(r core.Run) (core.Run, error) {
	doc, err := enc(r)
	if err != nil {
		return core.Run{}, internal(err)
	}
	return decRun(string(doc))
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
		run.StateSnapshot = p.StateSnapshot
	}
}

// RequestCancel implements core.RunStore.
func (s *RunStore) RequestCancel(ctx context.Context, tenant core.TenantID, id core.RunID) (core.Run, error) {
	var out core.Run
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if !r.Status.Terminal() && !r.CancelRequested {
			r.CancelRequested = true
			r.UpdatedAt = s.s.clock.Now()
			if err := saveRun(ctx, tx, r); err != nil {
				return err
			}
		}
		out = r
		return nil
	})
	return out, err
}

func decStep(doc string) (core.StepRun, error) {
	var st core.StepRun
	if err := dec([]byte(doc), &st); err != nil {
		return core.StepRun{}, internal(err)
	}
	return st, nil
}

// PutStep implements core.RunStore.
func (s *RunStore) PutStep(ctx context.Context, tenant core.TenantID, st core.StepRun) error {
	return s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id = ?`, string(st.Run)).Scan(&n); err != nil {
			return internal(err)
		}
		if n == 0 {
			return notFoundRun(st.Run)
		}
		now := s.s.clock.Now()
		st.Tenant = tenant
		var doc string
		err := tx.QueryRowContext(ctx, `SELECT doc FROM steps WHERE run_id = ? AND id = ?`, string(st.Run), string(st.ID)).Scan(&doc)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if st.CreatedAt.IsZero() {
				st.CreatedAt = now
			}
			st.UpdatedAt = now
			b, err := enc(st)
			if err != nil {
				return internal(err)
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO steps (run_id, id, doc) VALUES (?, ?, ?)`, string(st.Run), string(st.ID), string(b))
			return internal(err)
		case err != nil:
			return internal(err)
		}
		old, err := decStep(doc)
		if err != nil {
			return err
		}
		if old.Status.Terminal() {
			return nil // terminal records are never rewritten
		}
		st.CreatedAt = old.CreatedAt
		st.UpdatedAt = now
		b, err := enc(st)
		if err != nil {
			return internal(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE steps SET doc = ? WHERE run_id = ? AND id = ?`, string(b), string(st.Run), string(st.ID))
		return internal(err)
	})
}

// GetStep implements core.RunStore.
func (s *RunStore) GetStep(ctx context.Context, tenant core.TenantID, run core.RunID, id core.StepRunID) (core.StepRun, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.StepRun{}, err
	}
	var doc string
	err = q.QueryRowContext(ctx, `SELECT doc FROM steps WHERE run_id = ? AND id = ?`, string(run), string(id)).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.StepRun{}, core.Errorf(core.CodeRunNotFound, "step record %s of run %s not found", id, run)
	}
	if err != nil {
		return core.StepRun{}, internal(err)
	}
	return decStep(doc)
}

// ListSteps implements core.RunStore.
func (s *RunStore) ListSteps(ctx context.Context, tenant core.TenantID, run core.RunID) ([]core.StepRun, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if _, err := loadRun(ctx, q, run); err != nil {
		return nil, err
	}
	out, err := queryDocs(ctx, q, "steps", []string{"run_id = ?"}, []any{string(run)}, "seq ASC", 0, decStep)
	if out == nil {
		out = []core.StepRun{}
	}
	return out, err
}

// StateStore is the SQLite core.StateStore.
type StateStore struct{ s *Store }

func kv(ctx context.Context, q execer, query string, args ...any) (map[string]any, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	out := map[string]any{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, internal(err)
		}
		val, err := core.DecodeJSONValue([]byte(v))
		if err != nil {
			return nil, internal(err)
		}
		out[k] = val
	}
	return out, internal(rows.Err())
}

// Snapshot implements core.StateStore.
func (s *StateStore) Snapshot(ctx context.Context, tenant core.TenantID, flow string) (map[string]any, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return kv(ctx, q, `SELECT key, value FROM state_kv WHERE flow = ?`, flow)
}

// Stage implements core.StateStore.
func (s *StateStore) Stage(ctx context.Context, tenant core.TenantID, run core.RunID, key string, value any) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	b, err := enc(value)
	if err != nil {
		return core.Wrap(core.CodeInvalidFlow, err)
	}
	if len(b) > core.MaxStateValueBytes {
		return core.Errorf(core.CodeStateTooLarge, "state key %q is %d bytes, over the %d limit", key, len(b), core.MaxStateValueBytes)
	}
	return s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, run)
		if err != nil {
			return err
		}
		if r.Status.Terminal() {
			return core.Errorf(core.CodeRunStateConflict, "run %s is %s", run, r.Status)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO staged (run_id, key, value) VALUES (?, ?, ?) ON CONFLICT (run_id, key) DO UPDATE SET value = excluded.value`,
			string(run), key, string(b))
		return internal(err)
	})
}

// Staged implements core.StateStore.
func (s *StateStore) Staged(ctx context.Context, tenant core.TenantID, run core.RunID) (map[string]any, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return kv(ctx, q, `SELECT key, value FROM staged WHERE run_id = ?`, string(run))
}

// FinishRun implements core.StateStore.
func (s *StateStore) FinishRun(ctx context.Context, tenant core.TenantID, id core.RunID, fin core.RunFinish) (core.Run, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Run{}, err
	}
	if !fin.Status.Terminal() {
		return core.Run{}, core.Errorf(core.CodeUsage, "FinishRun needs a terminal status, got %q", fin.Status)
	}
	var out core.Run
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Status.Terminal() {
			if r.Status == fin.Status {
				out = r
				return nil
			}
			return core.Errorf(core.CodeRunStateConflict, "run %s is already %s", id, r.Status)
		}
		if fin.Status == core.RunSucceeded {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO state_kv (flow, key, value) SELECT ?, key, value FROM staged WHERE run_id = ? ON CONFLICT (flow, key) DO UPDATE SET value = excluded.value`,
				r.Flow.Name, string(id)); err != nil {
				return internal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM staged WHERE run_id = ?`, string(id)); err != nil {
			return internal(err)
		}
		at := fin.At
		if at.IsZero() {
			at = s.s.clock.Now()
		}
		r.Status, r.Failure, r.Result, r.Usage = fin.Status, fin.Failure, fin.Result, fin.Usage
		r.WaitingFor, r.ExpiresAt, r.NextRetryAt = "", nil, nil
		r.FinishedAt = &at
		r.UpdatedAt = s.s.clock.Now()
		if err := saveRun(ctx, tx, r); err != nil {
			return err
		}
		out, err = roundTrip(r)
		return err
	})
	return out, err
}
