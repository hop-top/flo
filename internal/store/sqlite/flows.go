package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hop.top/flo/internal/core"
)

// FlowStore is the SQLite core.FlowStore.
type FlowStore struct{ s *Store }

func scanFlow(sc interface{ Scan(...any) error }) (core.FlowVersion, error) {
	var doc string
	var src []byte
	if err := sc.Scan(&doc, &src); err != nil {
		return core.FlowVersion{}, err //nolint:wrapcheck // callers classify sql.ErrNoRows
	}
	var v core.FlowVersion
	if err := dec([]byte(doc), &v); err != nil {
		return core.FlowVersion{}, internal(err)
	}
	if src != nil {
		v.Source = src
	}
	return v, nil
}

func saveFlow(ctx context.Context, q execer, v core.FlowVersion) error {
	doc, err := enc(v)
	if err != nil {
		return internal(err)
	}
	_, err = q.ExecContext(ctx, `UPDATE flow_versions SET state = ?, doc = ? WHERE flow = ? AND hash = ?`,
		string(v.State), string(doc), v.Name, v.Hash)
	return internal(err)
}

func flowNotFound(name string) error {
	return core.Errorf(core.CodeFlowNotFound, "flow %q not found", name).
		WithSuggestion("flo flow apply -f <file>")
}

const flowCols = `doc, source`

// versions returns a flow's versions, newest first.
func versions(ctx context.Context, q execer, name string) ([]core.FlowVersion, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+flowCols+` FROM flow_versions WHERE flow = ? ORDER BY vseq DESC`, name)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []core.FlowVersion
	for rows.Next() {
		v, err := scanFlow(rows)
		if err != nil {
			return nil, internal(err)
		}
		out = append(out, v)
	}
	return out, internal(rows.Err())
}

// PutVersion implements core.FlowStore.
func (s *FlowStore) PutVersion(ctx context.Context, in core.NewFlowVersion) (core.FlowVersion, bool, error) {
	if err := core.RequireTenant(in.Tenant); err != nil {
		return core.FlowVersion{}, false, err
	}
	if in.Name == "" || in.Hash == "" {
		return core.FlowVersion{}, false, core.Errorf(core.CodeUsage, "a flow version needs a name and a hash")
	}
	var out core.FlowVersion
	created := false
	err := s.s.tx(ctx, in.Tenant, func(tx *sql.Tx) error {
		v, err := scanFlow(tx.QueryRowContext(ctx, `SELECT `+flowCols+` FROM flow_versions WHERE flow = ? AND hash = ?`, in.Name, in.Hash))
		if err == nil {
			out = v
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return internal(err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(vseq), 0) FROM flow_versions WHERE flow = ?`, in.Name).Scan(&n); err != nil {
			return internal(err)
		}
		at := in.At
		if at.IsZero() {
			at = s.s.clock.Now()
		}
		v = core.FlowVersion{
			Tenant: in.Tenant, Name: in.Name, Version: in.Version, Hash: in.Hash, Seq: n + 1,
			Source: in.Source, Bundle: in.Bundle, State: core.FlowDraft, CreatedAt: at,
		}
		doc, err := enc(v)
		if err != nil {
			return internal(err)
		}
		src := in.Source
		if src == nil {
			src = []byte{}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO flow_versions (flow, hash, version, vseq, state, source, doc) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			v.Name, v.Hash, v.Version, v.Seq, string(v.State), src, string(doc)); err != nil {
			return internal(err)
		}
		created = true
		out = v
		return nil
	})
	if err != nil {
		return core.FlowVersion{}, false, err
	}
	return out, created, nil
}

// GetVersion implements core.FlowStore.
func (s *FlowStore) GetVersion(ctx context.Context, tenant core.TenantID, name, ref string) (core.FlowVersion, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.FlowVersion{}, err
	}
	vs, err := versions(ctx, q, name)
	if err != nil {
		return core.FlowVersion{}, err
	}
	if len(vs) == 0 {
		return core.FlowVersion{}, flowNotFound(name)
	}
	for _, v := range vs { // newest first
		if ref == "" || v.Hash == ref || v.Version == ref {
			return v, nil
		}
	}
	return core.FlowVersion{}, core.Errorf(core.CodeFlowNotFound, "flow %q has no version %q", name, ref)
}

// Active implements core.FlowStore.
func (s *FlowStore) Active(ctx context.Context, tenant core.TenantID, name string) (core.FlowVersion, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.FlowVersion{}, err
	}
	vs, err := versions(ctx, q, name)
	if err != nil {
		return core.FlowVersion{}, err
	}
	if len(vs) == 0 {
		return core.FlowVersion{}, flowNotFound(name)
	}
	for _, v := range vs {
		if v.State == core.FlowActive {
			return v, nil
		}
	}
	return core.FlowVersion{}, core.Errorf(core.CodeFlowNotActive, "flow %q has no active version", name).
		WithSuggestion("flo flow set " + name + " --state active")
}

// ListFlows implements core.FlowStore.
func (s *FlowStore) ListFlows(ctx context.Context, tenant core.TenantID) ([]core.FlowSummary, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT flow FROM flow_versions ORDER BY flow`)
	if err != nil {
		return nil, internal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return nil, internal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, internal(err)
	}
	_ = rows.Close()
	var out []core.FlowSummary
	for _, n := range names {
		vs, err := versions(ctx, q, n)
		if err != nil {
			return nil, err
		}
		sum := core.FlowSummary{Name: n, Latest: vs[0]}
		for _, v := range vs {
			switch v.State {
			case core.FlowActive:
				c := v
				sum.Active = &c
			case core.FlowPaused:
				sum.Paused = true
			}
		}
		out = append(out, sum)
	}
	return out, nil
}

// ListVersions implements core.FlowStore.
func (s *FlowStore) ListVersions(ctx context.Context, tenant core.TenantID, name string) ([]core.FlowVersion, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	vs, err := versions(ctx, q, name)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, flowNotFound(name)
	}
	return vs, nil
}

// SetState implements core.FlowStore.
func (s *FlowStore) SetState(ctx context.Context, tenant core.TenantID, name, hash string, state core.FlowState, at time.Time) (core.FlowVersion, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.FlowVersion{}, err
	}
	if state != core.FlowActive && state != core.FlowPaused {
		return core.FlowVersion{}, core.Errorf(core.CodeUsage, "a version is set active or paused, not %q", state)
	}
	var out core.FlowVersion
	err := s.s.tx(ctx, tenant, func(tx *sql.Tx) error {
		vs, err := versions(ctx, tx, name)
		if err != nil {
			return err
		}
		idx := -1
		for i := range vs {
			if vs[i].Hash == hash {
				idx = i
			}
		}
		if idx < 0 {
			return core.Errorf(core.CodeFlowNotFound, "flow %q has no version %q", name, hash)
		}
		if at.IsZero() {
			at = s.s.clock.Now()
		}
		if vs[idx].State == state {
			out = vs[idx]
			return nil
		}
		if state == core.FlowPaused && vs[idx].State != core.FlowActive {
			return core.Errorf(core.CodeFlowNotActive, "only an active version is paused; %s is %s", hash, vs[idx].State)
		}
		if state == core.FlowActive {
			for i := range vs {
				if i != idx && (vs[i].State == core.FlowActive || vs[i].State == core.FlowPaused) {
					vs[i].State = core.FlowRetired
					if err := saveFlow(ctx, tx, vs[i]); err != nil {
						return err
					}
				}
			}
			vs[idx].ActivatedAt = &at
		}
		vs[idx].State = state
		if err := saveFlow(ctx, tx, vs[idx]); err != nil {
			return err
		}
		out = vs[idx]
		return nil
	})
	return out, err
}
