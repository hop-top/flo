package flowtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hop.top/flo/internal/core"
)

// Store keeps the latest test Result of each flow version, one JSON file per
// (tenant, flow, version hash):
//
//	<tenant dir>/tests/<flow>/<hash>.json
//
// The latest test of a version wins: a version that passed and then fails a
// re-test is not tested. Writes are atomic.
type Store struct {
	dir func(core.TenantID) string
}

// NewStore returns a Store under the tenant directory dir names.
func NewStore(dir func(core.TenantID) string) *Store { return &Store{dir: dir} }

func (s *Store) path(tenant core.TenantID, flow, hash string) (string, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return "", err //nolint:wrapcheck // *core.Error
	}
	for _, part := range []string{flow, hash} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return "", core.Errorf(core.CodeInternal, "test result key %q is not a file name", part)
		}
	}
	return filepath.Join(s.dir(tenant), "tests", flow, hash+".json"), nil
}

// Save stores r as the latest result of its version.
func (s *Store) Save(tenant core.TenantID, r Result) error {
	p, err := s.path(tenant, r.Flow, r.Hash)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".result-*")
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return core.Wrap(core.CodeInternal, err)
	}
	if err := tmp.Close(); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

// Load returns the latest result of the version, and false when the version
// was never tested.
func (s *Store) Load(tenant core.TenantID, flow, hash string) (Result, bool, error) {
	p, err := s.path(tenant, flow, hash)
	if err != nil {
		return Result{}, false, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // a path built from validated parts
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, core.Wrap(core.CodeInternal, err)
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return Result{}, false, core.Errorf(core.CodeInternal, "test result %s: %w", p, err)
	}
	return r, true, nil
}

// Digest fingerprints a bundle directory: the relative path and the content of
// every regular file in it, hidden files and directories left out. Anything
// the flow's behaviour reads (nodes/, prompts/, trusts/, tests/, record/)
// moves it. An empty dir (a single-file flow) has the empty digest.
func Digest(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	var rels []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return rerr
			}
			rels = append(rels, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", core.Errorf(core.CodeUsage, "bundle %s: %v", dir, err)
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // inside the bundle walked above
		if err != nil {
			return "", core.Errorf(core.CodeUsage, "bundle %s: %v", dir, err)
		}
		sum := sha256.Sum256(b)
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Binds reports whether r is a passing test of exactly the version with this
// hash and bundle digest.
func (r Result) Binds(hash, digest string) bool {
	return r.Passed && r.Hash == hash && r.Digest == digest
}
