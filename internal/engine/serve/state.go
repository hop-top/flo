package serve

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"hop.top/flo/internal/core"
)

// StateFile is the engine's state file under the data root. `engine serve`
// writes it while it runs and removes it when it stops; `flo status` and
// `flo run create --wait` read it.
const StateFile = "engine.json"

// State is what a running engine says about itself.
type State struct {
	PID     int       `json:"pid" yaml:"pid"`
	Host    string    `json:"host" yaml:"host"`
	Since   time.Time `json:"since" yaml:"since"`
	Version string    `json:"version" yaml:"version"`
	Tenant  string    `json:"tenant" yaml:"tenant"`
	Pollers int       `json:"pollers" yaml:"pollers"`
	Root    string    `json:"root" yaml:"root"`
}

// ReadState reads the state file of the engine at root. ok is false when
// there is none: no engine was started, or it stopped cleanly. A file left
// by a killed engine is returned; Alive tells them apart.
func ReadState(root string) (s State, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(root, StateFile)) //nolint:gosec // the engine's own file
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, core.Wrap(core.CodeInternal, err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, false, core.Errorf(core.CodeInternal, "engine state %s: %w", StateFile, err)
	}
	return s, true, nil
}

// Alive reports whether the process that wrote s still runs. Where a process
// cannot be probed (windows) it reports true.
func (s State) Alive() bool {
	if s.PID <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	p, err := os.FindProcess(s.PID)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// claimState records this process as the engine of root. A live engine
// already there is duplicate: two engines would fight over the schedule's
// compare-and-set for nothing. It returns the release function.
func claimState(root string, s State) (release func(), err error) {
	if cur, ok, rerr := ReadState(root); rerr == nil && ok && cur.PID != s.PID && cur.Alive() {
		return nil, core.Errorf(core.CodeDuplicate, "an engine already serves %s (pid %d)", root, cur.PID).
			WithSuggestion("stop it, or use another XDG_DATA_HOME")
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	path := filepath.Join(root, StateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	return func() {
		if cur, ok, _ := ReadState(root); ok && cur.PID == s.PID {
			_ = os.Remove(path)
		}
	}, nil
}
