package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hop.top/flo/internal/core"
)

// GrantStore is the SQLite core.GrantStore.
type GrantStore struct{ s *Store }

// SetGrant implements core.GrantStore.
func (s *GrantStore) SetGrant(ctx context.Context, g core.Grant) (bool, error) {
	if err := core.RequireTenant(g.Tenant); err != nil {
		return false, err
	}
	if g.Source == "" {
		g.Source = core.GrantLocal
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = s.s.clock.Now()
	}
	var exp any
	if g.ExpiresAt != nil {
		exp = g.ExpiresAt.UnixNano()
	}
	changed := false
	err := s.s.tx(ctx, g.Tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO grants (profile, capability, source, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			g.Profile, g.Capability, string(g.Source), g.CreatedAt.UnixNano(), exp)
		if err != nil {
			return internal(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return internal(err)
		}
		changed = n > 0
		if changed || g.Source != core.GrantProvisioned {
			return nil
		}
		// A provisioned grant is refreshed by the next set: its end moves.
		res, err = tx.ExecContext(ctx,
			`UPDATE grants SET source = ?, expires_at = ? WHERE profile = ? AND capability = ? AND (source <> ? OR expires_at IS NOT ?)`,
			string(g.Source), exp, g.Profile, g.Capability, string(g.Source), exp)
		if err != nil {
			return internal(err)
		}
		n, err = res.RowsAffected()
		changed = n > 0
		return internal(err)
	})
	return changed, err
}

// UnsetGrant implements core.GrantStore.
func (s *GrantStore) UnsetGrant(ctx context.Context, tenant core.TenantID, profile, capability string) (bool, error) {
	changed := false
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM grants WHERE profile = ? AND capability = ?`, profile, capability)
		if err != nil {
			return internal(err)
		}
		n, err := res.RowsAffected()
		changed = n > 0
		return internal(err)
	})
	return changed, err
}

// ListGrants implements core.GrantStore.
func (s *GrantStore) ListGrants(ctx context.Context, tenant core.TenantID, profile string) ([]core.Grant, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	query, args := `SELECT profile, capability, source, created_at, expires_at FROM grants`, []any(nil)
	if profile != "" {
		query += ` WHERE profile = ?`
		args = append(args, profile)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY profile, capability`, args...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []core.Grant
	for rows.Next() {
		g := core.Grant{Tenant: tenant}
		var src string
		var at, exp sql.NullInt64
		if err := rows.Scan(&g.Profile, &g.Capability, &src, &at, &exp); err != nil {
			return nil, internal(err)
		}
		g.Source = core.GrantSource(src)
		if at.Valid {
			g.CreatedAt = time.Unix(0, at.Int64).UTC()
		}
		if exp.Valid {
			t := time.Unix(0, exp.Int64).UTC()
			g.ExpiresAt = &t
		}
		out = append(out, g)
	}
	return out, internal(rows.Err())
}

// ReceiptStore is the SQLite core.ReceiptStore.
type ReceiptStore struct{ s *Store }

func decReceipt(doc string) (core.Receipt, error) {
	var r core.Receipt
	if err := dec([]byte(doc), &r); err != nil {
		return core.Receipt{}, internal(err)
	}
	return r, nil
}

// PutReceipt implements core.ReceiptStore.
func (s *ReceiptStore) PutReceipt(ctx context.Context, r core.Receipt) error {
	if err := core.RequireTenant(r.Tenant); err != nil {
		return err
	}
	b, err := enc(r)
	if err != nil {
		return internal(err)
	}
	return s.s.tx(ctx, r.Tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO receipts (id, run_id, doc) VALUES (?, ?, ?) ON CONFLICT (id) DO UPDATE SET run_id = excluded.run_id, doc = excluded.doc`,
			string(r.ID), string(r.Run), string(b))
		return internal(err)
	})
}

// GetReceipt implements core.ReceiptStore.
func (s *ReceiptStore) GetReceipt(ctx context.Context, tenant core.TenantID, id core.ReceiptID) (core.Receipt, bool, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.Receipt{}, false, err
	}
	var doc string
	err = q.QueryRowContext(ctx, `SELECT doc FROM receipts WHERE id = ?`, string(id)).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Receipt{}, false, nil
	}
	if err != nil {
		return core.Receipt{}, false, internal(err)
	}
	r, err := decReceipt(doc)
	return r, err == nil, err
}

// ListReceipts implements core.ReceiptStore.
func (s *ReceiptStore) ListReceipts(ctx context.Context, tenant core.TenantID, run core.RunID) ([]core.Receipt, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return queryDocs(ctx, q, "receipts", []string{"run_id = ?"}, []any{string(run)}, "seq ASC", 0, decReceipt)
}

// OutputStore is the SQLite core.OutputStore.
type OutputStore struct{ s *Store }

func decOutput(doc string) (core.Output, error) {
	var o core.Output
	if err := dec([]byte(doc), &o); err != nil {
		return core.Output{}, internal(err)
	}
	return o, nil
}

// PutOutput implements core.OutputStore.
func (s *OutputStore) PutOutput(ctx context.Context, o core.Output) error {
	if err := core.RequireTenant(o.Tenant); err != nil {
		return err
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = s.s.clock.Now()
	}
	b, err := enc(o)
	if err != nil {
		return internal(err)
	}
	return s.s.tx(ctx, o.Tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO outputs (id, run_id, kind, created_at, doc) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET run_id = excluded.run_id, kind = excluded.kind, created_at = excluded.created_at, doc = excluded.doc`,
			string(o.ID), string(o.Run), string(o.Kind), o.CreatedAt.UnixNano(), string(b))
		return internal(err)
	})
}

// GetOutput implements core.OutputStore.
func (s *OutputStore) GetOutput(ctx context.Context, tenant core.TenantID, id core.OutputID) (core.Output, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.Output{}, err
	}
	var doc string
	err = q.QueryRowContext(ctx, `SELECT doc FROM outputs WHERE id = ?`, string(id)).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Output{}, core.Errorf(core.CodeOutputNotFound, "output %s not found", id)
	}
	if err != nil {
		return core.Output{}, internal(err)
	}
	return decOutput(doc)
}

// ListOutputs implements core.OutputStore.
func (s *OutputStore) ListOutputs(ctx context.Context, tenant core.TenantID, f core.OutputFilter) ([]core.Output, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	if f.Run != "" {
		where, args = append(where, "run_id = ?"), append(args, string(f.Run))
	}
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, string(f.Kind))
	}
	if !f.Since.IsZero() {
		where, args = append(where, "created_at >= ?"), append(args, f.Since.UnixNano())
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	return queryDocs(ctx, q, "outputs", where, args, "seq DESC", limit, decOutput)
}

// PruneOutputs implements core.OutputStore.
func (s *OutputStore) PruneOutputs(ctx context.Context, tenant core.TenantID, cutoff time.Time) (int, error) {
	n := 0
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM outputs WHERE created_at < ?`, cutoff.UnixNano())
		if err != nil {
			return internal(err)
		}
		c, err := res.RowsAffected()
		n = int(c)
		return internal(err)
	})
	return n, err
}
