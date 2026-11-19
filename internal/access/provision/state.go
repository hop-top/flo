package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hop.top/flo/internal/core"
)

// State keeps the set flo holds, so a delta applies to it and the next process
// knows its epoch.
type State interface {
	// Load returns the held set; ok is false when none was applied.
	Load() (h Held, ok bool, err error)
	// Save replaces the held set.
	Save(h Held) error
}

// MemState keeps the held set in memory.
type MemState struct {
	mu  sync.Mutex
	h   Held
	set bool
}

// Load implements State.
func (m *MemState) Load() (Held, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.h, m.set, nil
}

// Save implements State.
func (m *MemState) Save(h Held) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.h, m.set = h, true
	return nil
}

// FileState keeps the held set in one JSON file, replaced atomically.
type FileState struct{ Path string }

type fileHeld struct {
	Epoch           int64             `json:"epoch"`
	Digest          string            `json:"digest"`
	AppliedAt       time.Time         `json:"applied_at"`
	LimitSeconds    int64             `json:"limit_seconds"`
	Content         map[string]any    `json:"content"`
	Unrepresentable []Unrepresentable `json:"unrepresentable"`
}

// Load implements State.
func (f FileState) Load() (Held, bool, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Held{}, false, nil
	}
	if err != nil {
		return Held{}, false, core.Errorf(core.CodeInternal, "access state %s: %w", f.Path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var h fileHeld
	if err := dec.Decode(&h); err != nil {
		return Held{}, false, core.Errorf(core.CodeInternal, "access state %s: %w", f.Path, err)
	}
	return Held{Epoch: h.Epoch, Digest: h.Digest, AppliedAt: h.AppliedAt, Limit: time.Duration(h.LimitSeconds) * time.Second,
		Content: h.Content, Unrepresentable: h.Unrepresentable}, true, nil
}

// Save implements State.
func (f FileState) Save(h Held) error {
	b, err := json.Marshal(fileHeld{Epoch: h.Epoch, Digest: h.Digest, AppliedAt: h.AppliedAt, LimitSeconds: int64(h.Limit / time.Second),
		Content: h.Content, Unrepresentable: h.Unrepresentable})
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o750); err != nil {
		return core.Errorf(core.CodeInternal, "access state %s: %w", f.Path, err)
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return core.Errorf(core.CodeInternal, "access state %s: %w", f.Path, err)
	}
	if err := os.Rename(tmp, f.Path); err != nil {
		return core.Errorf(core.CodeInternal, "access state %s: %w", f.Path, err)
	}
	return nil
}
