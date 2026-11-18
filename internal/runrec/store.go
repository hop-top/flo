// Package runrec keeps recordings of real runs: the exec and HTTP traffic of
// a run (xrr cassettes), its trigger payload, vars and starting state, so
// that `flo run replay` can run it again offline, in a sandbox.
//
// A recording is run data. It lives beside the run's log, in the tenant's
// runs directory, readable by the owner only:
//
//	<tenant dir>/runs/<run>.rec/
//	  meta.json       what was recorded, from which flow version, and whether it is whole
//	  trigger.json    the trigger payload, with redacted paths dropped
//	  vars.json       the run's bound vars, likewise
//	  state.json      the flow state the run started from, likewise
//	  cassettes/      one request/response pair per exec or HTTP call
//
// A recording of a flow whose privacy is local-only never leaves the machine:
// nothing here exports one, and Meta.MayLeave says so to whatever might.
package runrec

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// SchemaVersion is the version of meta.json.
const SchemaVersion = "1"

// Files of a recording.
const (
	MetaFile    = "meta.json"
	TriggerFile = "trigger.json"
	VarsFile    = "vars.json"
	StateFile   = "state.json"
	CassetteDir = "cassettes"

	dirSuffix = ".rec"
	dirPerm   = 0o700
	filePerm  = 0o600
)

var runIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Status is how far a recording got.
type Status string

// Recording statuses.
const (
	// StatusRecording: the run has not finished; calls are still being kept.
	StatusRecording Status = "recording"
	// StatusComplete: the run finished and its recording is closed.
	StatusComplete Status = "complete"
)

// Meta is meta.json.
type Meta struct {
	Schema  string `json:"schema"`
	Run     string `json:"run"`
	Flow    string `json:"flow"`
	Version string `json:"version"`
	// Hash is the flow version the run executed: the document hash a replay
	// runs.
	Hash string `json:"hash"`
	// Privacy is the flow's egress ceiling (local-only or cloud-allowed).
	Privacy string `json:"privacy"`
	Status  Status `json:"status"`
	// RunStatus is how the run ended, once it has.
	RunStatus  string     `json:"run_status,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Calls is how many exec and HTTP calls were kept.
	Calls int `json:"calls"`
	// Partial lists what could not be kept; a partial recording cannot be
	// replayed.
	Partial []string `json:"partial,omitempty"`
	// Dropped lists the inputs left out of the recording because a node
	// redacts them (trigger.token, state.password): a replay needs them
	// supplied again.
	Dropped []string `json:"dropped,omitempty"`
	// Redacted lists the redact paths the recording was scrubbed with.
	Redacted []string `json:"redacted,omitempty"`
}

// MayLeave reports whether the recording may be taken off this machine (put in
// a bundle, a repository, an export): only a flow that says cloud-allowed
// allows it. A recording is never moved without the owner asking; this is the
// answer to that ask.
func (m Meta) MayLeave() bool { return m.Privacy == string(specrun.PrivacyCloudAllowed) }

// Replayable is nil when the recording can be replayed, else why not.
func (m Meta) Replayable() error {
	switch {
	case m.Status != StatusComplete:
		return core.Errorf(core.CodeUsage, "the recording of run %s is not finished", m.Run).
			WithSuggestion("a recording is closed when its run ends; wait for the run, then replay")
	case len(m.Partial) > 0:
		return core.Errorf(core.CodeUsage, "the recording of run %s is partial: %s", m.Run, m.Partial[0]).
			WithDetail("partial", m.Partial).WithSuggestion("record again with `flo run create --record`")
	}
	return nil
}

// Store keeps recordings under the runs directory of each tenant.
type Store struct {
	runs func(core.TenantID) string
}

// NewStore returns a Store over runs, which maps a tenant to its runs
// directory (tenants.Registry.RunsDir).
func NewStore(runs func(core.TenantID) string) *Store { return &Store{runs: runs} }

// Dir is the directory of a run's recording.
func (s *Store) Dir(tenant core.TenantID, run core.RunID) (string, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return "", err //nolint:wrapcheck // *core.Error
	}
	if !runIDRe.MatchString(string(run)) {
		return "", core.Errorf(core.CodeUsage, "run id %q is not usable in a file name", string(run))
	}
	return filepath.Join(s.runs(tenant), string(run)+dirSuffix), nil
}

// Cassettes is the directory of a run's cassettes.
func (s *Store) Cassettes(tenant core.TenantID, run core.RunID) (string, error) {
	d, err := s.Dir(tenant, run)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, CassetteDir), nil
}

// Open makes the recording directory (owner only) and its cassettes.
func (s *Store) Open(tenant core.TenantID, run core.RunID) error {
	c, err := s.Cassettes(tenant, run)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c, dirPerm); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

// Load reads a run's meta.json; false when the run has no recording.
func (s *Store) Load(tenant core.TenantID, run core.RunID) (Meta, bool, error) {
	var m Meta
	ok, err := s.ReadJSON(tenant, run, MetaFile, &m)
	return m, ok, err
}

// Save writes meta.json.
func (s *Store) Save(tenant core.TenantID, m Meta) error {
	m.Schema = SchemaVersion
	return s.WriteJSON(tenant, core.RunID(m.Run), MetaFile, m)
}

// WriteJSON writes one file of the recording atomically, owner only.
func (s *Store) WriteJSON(tenant core.TenantID, run core.RunID, name string, v any) error {
	d, err := s.Dir(tenant, run)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, dirPerm); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	tmp, err := os.CreateTemp(d, ".tmp-*")
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return core.Wrap(core.CodeInternal, err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return core.Wrap(core.CodeInternal, err)
	}
	if err := tmp.Close(); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(d, name)); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

// ReadJSON reads one file of the recording into v; false when it is not there.
func (s *Store) ReadJSON(tenant core.TenantID, run core.RunID, name string, v any) (bool, error) {
	d, err := s.Dir(tenant, run)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(d, name)) //nolint:gosec // a path built from validated parts
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, core.Wrap(core.CodeInternal, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, core.Errorf(core.CodeInternal, "recording of %s: %s: %w", run, name, err)
	}
	return true, nil
}

// Exists reports whether the run has a file of the recording.
func (s *Store) Exists(tenant core.TenantID, run core.RunID, name string) bool {
	d, err := s.Dir(tenant, run)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(d, name))
	return err == nil
}

// ReadDoc reads a JSON object of the recording in the engine's generic form
// (integers stay integers); false when it is not there.
func (s *Store) ReadDoc(tenant core.TenantID, run core.RunID, name string) (map[string]any, bool, error) {
	d, err := s.Dir(tenant, run)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Join(d, name)) //nolint:gosec // a path built from validated parts
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, core.Wrap(core.CodeInternal, err)
	}
	v, err := core.DecodeJSONValue(data)
	if err != nil {
		return nil, false, core.Errorf(core.CodeInternal, "recording of %s: %s: %w", run, name, err)
	}
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, true, nil
}
