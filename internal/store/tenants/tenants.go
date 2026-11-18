// Package tenants is the tenant registry and the on-disk layout (D3): one
// SQLite file per tenant under the data root, one shared jobs database, one
// JSON Lines file per run.
//
//	<root>/jobs.db
//	<root>/tenants/<tenant>/flo.db
//	<root>/tenants/<tenant>/runs/<run>.jsonl
//
// A tenant is registered when its directory holds its database; the registry
// has no table of its own, so it cannot disagree with the files.
package tenants

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"hop.top/flo/internal/core"
)

// DBFile and JobsFile are the database file names.
const (
	DBFile   = "flo.db"
	JobsFile = "jobs.db"
)

// InitFunc prepares a tenant's database: it opens it and applies the
// migrations. The registry calls it from Ensure.
type InitFunc func(ctx context.Context, t core.TenantID) error

// Registry implements core.Tenants over the directory layout.
type Registry struct {
	root string
	init InitFunc
}

// New returns a registry rooted at root. init may be nil, in which case
// Ensure only creates the directories.
func New(root string, init InitFunc) *Registry { return &Registry{root: root, init: init} }

// DefaultRoot returns the data root: $XDG_DATA_HOME/flo, else
// ~/.local/share/flo.
func DefaultRoot() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "flo"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", core.Wrap(core.CodeInternal, err)
	}
	return filepath.Join(h, ".local", "share", "flo"), nil
}

// Root returns the data root.
func (r *Registry) Root() string { return r.root }

// JobsPath is the shared jobs database.
func (r *Registry) JobsPath() string { return filepath.Join(r.root, JobsFile) }

// Dir is a tenant's directory. t must be valid (core.RequireTenant), which
// keeps the join inside the root.
func (r *Registry) Dir(t core.TenantID) string { return filepath.Join(r.root, "tenants", string(t)) }

// DBPath is a tenant's database file.
func (r *Registry) DBPath(t core.TenantID) string { return filepath.Join(r.Dir(t), DBFile) }

// RunsDir is the directory of a tenant's run logs.
func (r *Registry) RunsDir(t core.TenantID) string { return filepath.Join(r.Dir(t), "runs") }

// Ensure implements core.Tenants.
func (r *Registry) Ensure(ctx context.Context, t core.TenantID) error {
	if err := core.RequireTenant(t); err != nil {
		return err
	}
	if err := os.MkdirAll(r.RunsDir(t), 0o750); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if r.init != nil {
		return r.init(ctx, t)
	}
	return nil
}

// List implements core.Tenants.
func (r *Registry) List(context.Context) ([]core.TenantID, error) {
	ents, err := os.ReadDir(filepath.Join(r.root, "tenants"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, core.Wrap(core.CodeInternal, err)
	}
	var out []core.TenantID
	for _, e := range ents {
		t := core.TenantID(e.Name())
		if !e.IsDir() || !t.Valid() {
			continue
		}
		if _, err := os.Stat(r.DBPath(t)); err == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

var _ core.Tenants = (*Registry)(nil)
