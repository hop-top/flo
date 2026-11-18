package serve

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// ManifestFile is the manifest of an installed pack: <packs>/<name>/pack.yaml.
const ManifestFile = "pack.yaml"

// Packs are the installed packs (node.md §17): a directory per pack under
// the packs directory, each with its manifest and the files it lists. The
// engine reads them at start-up; installing and removing is `flo pack`.
type Packs struct {
	// Dir is the packs directory.
	Dir    string
	Loaded []*node.LoadedPack
}

// LoadPacks reads every pack under dir. A missing directory is no packs; a
// pack that does not load is invalid_node, naming it.
func LoadPacks(dir string) (*Packs, error) {
	p := &Packs{Dir: dir}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "read packs %s: %w", dir, err)
	}
	fsys := os.DirFS(dir)
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		manifest := path.Join(e.Name(), ManifestFile)
		if _, err := fs.Stat(fsys, manifest); err != nil {
			continue
		}
		lp, err := node.LoadPack(fsys, manifest)
		if err != nil {
			errs = append(errs, core.Errorf(core.CodeInvalidNode, "pack %s: %w", e.Name(), err))
			continue
		}
		p.Loaded = append(p.Loaded, lp)
	}
	return p, errors.Join(errs...)
}

// Entries returns the node specs of every pack.
func (p *Packs) Entries() []node.Entry {
	var out []node.Entry
	for _, lp := range p.Loaded {
		out = append(out, lp.Entries()...)
	}
	return out
}

// TrustEntries returns the trust providers of every pack.
func (p *Packs) TrustEntries() []node.TrustEntry {
	var out []node.TrustEntry
	for _, lp := range p.Loaded {
		out = append(out, lp.TrustEntries()...)
	}
	return out
}

// Capabilities returns what the packs declare: the union of their nodes'
// capabilities, sorted. The engine's profile holds exactly these.
func (p *Packs) Capabilities() []string {
	set := map[string]bool{}
	for _, lp := range p.Loaded {
		for _, n := range lp.Nodes {
			for _, c := range n.Capabilities {
				set[c] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Locator returns the trust provider locator over the packs' trusts and
// then the directories dirs (`<name>.yaml`).
func (p *Packs) Locator(dirs ...string) approvals.Locator {
	files := map[string]string{}
	for _, lp := range p.Loaded {
		for _, t := range lp.Trusts {
			if _, dup := files[t.Trust]; !dup {
				files[t.Trust] = filepath.Join(p.Dir, filepath.FromSlash(t.Path))
			}
		}
	}
	return packTrusts{files: files, dirs: approvals.DirLocator{Dirs: dirs}}
}

type packTrusts struct {
	files map[string]string
	dirs  approvals.DirLocator
}

// Find implements approvals.Locator.
func (l packTrusts) Find(ctx context.Context, tenant core.TenantID, name string) (approvals.Provider, error) {
	if f, ok := l.files[name]; ok {
		data, err := os.ReadFile(f) //nolint:gosec // a file the pack manifest lists
		if err != nil {
			return approvals.Provider{}, core.Errorf(core.CodeInternal, "trust provider %s: %w", name, err)
		}
		return approvals.ParseProvider(data) //nolint:wrapcheck // *core.Error
	}
	return l.dirs.Find(ctx, tenant, name) //nolint:wrapcheck // *core.Error
}
