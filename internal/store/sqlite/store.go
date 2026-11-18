// Package sqlite is the SQLite implementation of the storage ports of
// package core: runs, steps, flow state, flows, approvals, grants, receipts,
// outputs, triggers and concurrency groups. Each tenant has its own database
// file (D3); a record of one tenant is not reachable through another's.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"hop.top/kit/go/storage/sqldb"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/store/tenants"
)

// Options configure Open.
type Options struct {
	// Clock stamps records; the system clock when nil.
	Clock core.Clock
	// BusyTimeoutMS is SQLite's busy timeout; 0 takes the kit default.
	BusyTimeoutMS int
	// NoSync turns fsync off. For throwaway test databases only.
	NoSync bool
}

// Store holds the tenant databases and the ports over them.
type Store struct {
	opts  Options
	clock core.Clock
	reg   *tenants.Registry

	mu  sync.Mutex
	dbs map[core.TenantID]*sql.DB

	Tenants   *tenants.Registry
	Runs      *RunStore
	State     *StateStore
	Flows     *FlowStore
	Approvals *ApprovalStore
	Grants    *GrantStore
	Receipts  *ReceiptStore
	Outputs   *OutputStore
	Triggers  *TriggerStore
	Groups    *Groups
}

// Open returns a Store rooted at root. Tenant databases open on first use.
func Open(root string, opts Options) *Store {
	s := &Store{opts: opts, clock: opts.Clock, dbs: map[core.TenantID]*sql.DB{}}
	if s.clock == nil {
		s.clock = core.SystemClock{}
	}
	s.reg = tenants.New(root, func(ctx context.Context, t core.TenantID) error {
		_, err := s.db(ctx, t)
		return err
	})
	s.Tenants = s.reg
	s.Runs = &RunStore{s}
	s.State = &StateStore{s}
	s.Flows = &FlowStore{s}
	s.Approvals = &ApprovalStore{s}
	s.Grants = &GrantStore{s}
	s.Receipts = &ReceiptStore{s}
	s.Outputs = &OutputStore{s}
	s.Triggers = &TriggerStore{s}
	s.Groups = &Groups{s}
	return s
}

// Registry returns the tenant registry (layout and listing).
func (s *Store) Registry() *tenants.Registry { return s.reg }

// Close closes every open tenant database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for t, db := range s.dbs {
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.dbs, t)
	}
	return first
}

// OpenDB opens (creating) and migrates the SQLite file at path with the pool
// settings every flo database uses. Writers take the lock when they begin
// (_txlock=immediate) so a read-then-write transaction never fails with
// SQLITE_BUSY halfway; busy_timeout waits for the other writer.
func OpenDB(path string, busyMS int, noSync bool) (*sql.DB, error) {
	o := sqldb.Options{Path: path + "?_txlock=immediate", BusyTimeout: busyMS}
	if noSync {
		o.Synchronous = "OFF"
	}
	db, err := sqldb.Open(o)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func (s *Store) db(ctx context.Context, t core.TenantID) (*sql.DB, error) {
	if err := core.RequireTenant(t); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.dbs[t]; ok {
		return db, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // the caller's own cancellation
	}
	if err := mkdirs(s.reg, t); err != nil {
		return nil, err
	}
	db, err := OpenDB(s.reg.DBPath(t), s.opts.BusyTimeoutMS, s.opts.NoSync)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, core.Wrap(core.CodeInternal, err)
	}
	s.dbs[t] = db
	return db, nil
}

// migrate applies the migrations; a second process doing the same at once
// may lose the race on the version row, in which case the work is done.
func migrate(db *sql.DB) error {
	err := sqldb.Migrate(db, "flo_migrations", migrations)
	if err != nil {
		err = sqldb.Migrate(db, "flo_migrations", migrations)
	}
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Migrate applies the schema to the database at path. It is idempotent.
func Migrate(path string) error {
	db, err := OpenDB(path, 0, false)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // read path closes at exit
	return migrate(db)
}

// execer is what both *sql.DB and *sql.Tx offer.
type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// tx runs fn in one write transaction on the tenant's database.
func (s *Store) tx(ctx context.Context, t core.TenantID, fn func(tx *sql.Tx) error) error {
	db, err := s.db(ctx, t)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return internal(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return internal(err)
	}
	return nil
}

// read gives fn a connection for queries.
func (s *Store) read(ctx context.Context, t core.TenantID) (execer, error) {
	db, err := s.db(ctx, t)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// internal wraps a storage fault; an *core.Error passes through unchanged.
func internal(err error) error {
	if err == nil {
		return nil
	}
	return core.Wrap(core.CodeInternal, err)
}
