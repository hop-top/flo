package builder

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// Ref names a version of a flow.
type Ref struct {
	Version string `json:"version" yaml:"version"`
	Hash    string `json:"hash" yaml:"hash"`
}

// DryRun is the proposal replayed on the flow's last recording: where its
// outcome differs from the recorded run's.
type DryRun struct {
	// Ran is false when there was nothing to replay on; Reason says why.
	Ran    bool   `json:"ran" yaml:"ran"`
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Run is the recorded run, and Recorded the version it executed.
	Run            string `json:"run,omitempty" yaml:"run,omitempty"`
	Recorded       Ref    `json:"recorded,omitempty" yaml:"recorded,omitempty"`
	OriginalStatus string `json:"original_status,omitempty" yaml:"original_status,omitempty"`
	Status         string `json:"status,omitempty" yaml:"status,omitempty"`
	// Matches is true when the replay ended as the run did with no step
	// differing and no call without a recording.
	Matches bool `json:"matches" yaml:"matches"`
	// Diffs are where the outcomes differ; Problems the calls that have no
	// recording.
	Diffs    []string `json:"diffs" yaml:"diffs"`
	Problems []string `json:"problems" yaml:"problems"`
	Calls    int      `json:"calls" yaml:"calls"`
	Replayed int      `json:"replayed" yaml:"replayed"`
}

// Draft is a proposed full next version of a flow.
type Draft struct {
	ID          string `json:"id" yaml:"id"`
	Flow        string `json:"flow" yaml:"flow"`
	Instruction string `json:"instruction" yaml:"instruction"`
	// Base is the version the model was shown (the active one, else the
	// newest) and Head the newest stored one; the draft is stale when either
	// moves.
	Base Ref    `json:"base" yaml:"base"`
	Head string `json:"head" yaml:"head"`
	// Version and Hash are the proposed document's.
	Version string `json:"version" yaml:"version"`
	Hash    string `json:"hash" yaml:"hash"`
	Source  string `json:"source" yaml:"source"`
	// Bundle is the base's bundle directory, which the draft's nodes/ and
	// trusts/ come from.
	Bundle    string    `json:"bundle,omitempty" yaml:"bundle,omitempty"`
	Model     string    `json:"model" yaml:"model"`
	Attempts  []Attempt `json:"attempts" yaml:"attempts"`
	Warnings  []Finding `json:"warnings" yaml:"warnings"`
	Cost      core.Cost `json:"cost" yaml:"cost"`
	DryRun    *DryRun   `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
	CreatedAt time.Time `json:"created_at" yaml:"created_at"`
	CreatedBy string    `json:"created_by" yaml:"created_by"`
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Store keeps drafts, one JSON file each, readable by the owner only:
//
//	<tenant dir>/drafts/<id>.json
type Store struct{ dir func(core.TenantID) string }

// NewStore returns a Store under the tenant directory dir names.
func NewStore(dir func(core.TenantID) string) *Store { return &Store{dir: dir} }

func (s *Store) path(tenant core.TenantID, id string) (string, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return "", err //nolint:wrapcheck // *core.Error
	}
	if !idRe.MatchString(id) {
		return "", core.Errorf(core.CodeUsage, "draft id %q is not usable in a file name", id)
	}
	return filepath.Join(s.dir(tenant), "drafts", id+".json"), nil
}

// Save writes d atomically.
func (s *Store) Save(tenant core.TenantID, d Draft) error {
	p, err := s.path(tenant, d.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".draft-*")
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
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

func notFound(id string) error {
	return core.Errorf(core.CodeDraftNotFound, "draft %q not found", id).WithSuggestion("flo flow draft list")
}

// Load reads a draft; draft_not_found when there is none.
func (s *Store) Load(tenant core.TenantID, id string) (Draft, error) {
	p, err := s.path(tenant, id)
	if err != nil {
		return Draft{}, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // a path built from a validated id
	if errors.Is(err, fs.ErrNotExist) {
		return Draft{}, notFound(id)
	}
	if err != nil {
		return Draft{}, core.Wrap(core.CodeInternal, err)
	}
	var d Draft
	if err := json.Unmarshal(data, &d); err != nil {
		return Draft{}, core.Errorf(core.CodeInternal, "draft %s: %w", id, err)
	}
	return d, nil
}

// Delete removes a draft; draft_not_found when there is none.
func (s *Store) Delete(tenant core.TenantID, id string) error {
	p, err := s.path(tenant, id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, fs.ErrNotExist) {
		return notFound(id)
	} else if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

// List returns the drafts of flow (all drafts when empty), newest first.
func (s *Store) List(tenant core.TenantID, flow string) ([]Draft, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	dir := filepath.Join(s.dir(tenant), "drafts")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Draft{}, nil
	}
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	out := []Draft{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		d, err := s.Load(tenant, id)
		if err != nil {
			return nil, err
		}
		if flow == "" || d.Flow == flow {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}
