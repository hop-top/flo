package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"hop.top/flo/internal/core"
)

// ApprovalStore is the SQLite core.ApprovalStore.
type ApprovalStore struct{ s *Store }

func decApproval(doc string) (core.Approval, error) {
	var a core.Approval
	if err := dec([]byte(doc), &a); err != nil {
		return core.Approval{}, internal(err)
	}
	return a, nil
}

func approvalNotFound(id core.ApprovalID) error {
	return core.Errorf(core.CodeApprovalNotFound, "approval %s not found", id)
}

// CreateApproval implements core.ApprovalStore.
func (s *ApprovalStore) CreateApproval(ctx context.Context, a core.Approval) (core.Approval, bool, error) {
	if err := core.RequireTenant(a.Tenant); err != nil {
		return core.Approval{}, false, err
	}
	var out core.Approval
	created := false
	err := s.s.tx(ctx, a.Tenant, func(tx *sql.Tx) error {
		var doc string
		err := tx.QueryRowContext(ctx, `SELECT doc FROM approvals WHERE run_id = ? AND step_run = ?`, string(a.Run), string(a.StepRun)).Scan(&doc)
		if err == nil {
			out, err = decApproval(doc)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return internal(err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE id = ?`, string(a.ID)).Scan(&n); err != nil {
			return internal(err)
		}
		if n > 0 {
			return core.Errorf(core.CodeDuplicate, "approval %s exists", a.ID)
		}
		now := s.s.clock.Now()
		a.Version = 1
		if a.Status == "" {
			a.Status = core.ApprovalOpen
		}
		a.CreatedAt, a.UpdatedAt = now, now
		b, err := enc(a)
		if err != nil {
			return internal(err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO approvals (id, run_id, step_run, step_id, status, version, doc) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(a.ID), string(a.Run), string(a.StepRun), a.StepID, string(a.Status), a.Version, string(b)); err != nil {
			return internal(err)
		}
		created = true
		out, err = decApproval(string(b))
		return err
	})
	if err != nil {
		return core.Approval{}, false, err
	}
	return out, created, nil
}

// GetApproval implements core.ApprovalStore.
func (s *ApprovalStore) GetApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (core.Approval, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return core.Approval{}, err
	}
	var doc string
	err = q.QueryRowContext(ctx, `SELECT doc FROM approvals WHERE id = ?`, string(id)).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Approval{}, approvalNotFound(id)
	}
	if err != nil {
		return core.Approval{}, internal(err)
	}
	return decApproval(doc)
}

// ListApprovals implements core.ApprovalStore.
func (s *ApprovalStore) ListApprovals(ctx context.Context, tenant core.TenantID, f core.ApprovalFilter) ([]core.Approval, error) {
	q, err := s.s.read(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, string(f.Status))
	}
	if f.Run != "" {
		where, args = append(where, "run_id = ?"), append(args, string(f.Run))
	}
	if f.StepID != "" {
		where, args = append(where, "step_id = ?"), append(args, f.StepID)
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	return queryDocs(ctx, q, "approvals", where, args, "seq DESC", limit, decApproval)
}

// UpdateApproval implements core.ApprovalStore.
func (s *ApprovalStore) UpdateApproval(ctx context.Context, a core.Approval) (core.Approval, error) {
	if err := core.RequireTenant(a.Tenant); err != nil {
		return core.Approval{}, err
	}
	var out core.Approval
	err := s.s.tx(ctx, a.Tenant, func(tx *sql.Tx) error {
		var doc string
		err := tx.QueryRowContext(ctx, `SELECT doc FROM approvals WHERE id = ?`, string(a.ID)).Scan(&doc)
		if errors.Is(err, sql.ErrNoRows) {
			return approvalNotFound(a.ID)
		}
		if err != nil {
			return internal(err)
		}
		cur, err := decApproval(doc)
		if err != nil {
			return err
		}
		if cur.Version != a.Version {
			return core.Errorf(core.CodeApprovalConflict, "approval %s is at version %d, not %d", a.ID, cur.Version, a.Version).
				WithDetail("current", cur.Version)
		}
		a.Version++
		a.UpdatedAt = s.s.clock.Now()
		a.CreatedAt = cur.CreatedAt
		b, err := enc(a)
		if err != nil {
			return internal(err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE approvals SET status = ?, step_id = ?, version = ?, doc = ? WHERE id = ?`,
			string(a.Status), a.StepID, a.Version, string(b), string(a.ID)); err != nil {
			return internal(err)
		}
		out, err = decApproval(string(b))
		return err
	})
	return out, err
}
