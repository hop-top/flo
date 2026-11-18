package runrec

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// Prune deletes the recordings started before cutoff (a directory without a
// meta.json goes by its own time) and reports how many.
func (s *Store) Prune(tenant core.TenantID, cutoff time.Time) (int, error) {
	return s.pruneWhere(tenant, func(_ string, at time.Time) bool { return at.Before(cutoff) })
}

// PruneBy deletes the recordings that outlived their flow's retention: keep
// says how long a recording of the named flow is kept, counted from when it
// was made. A directory without a meta.json has no flow, so keep gets "" and
// answers the default; it goes by its own time.
func (s *Store) PruneBy(tenant core.TenantID, now time.Time, keep func(flow string) time.Duration) (int, error) {
	return s.pruneWhere(tenant, func(flow string, at time.Time) bool { return at.Before(now.Add(-keep(flow))) })
}

func (s *Store) pruneWhere(tenant core.TenantID, expired func(flow string, at time.Time) bool) (int, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return 0, err //nolint:wrapcheck // *core.Error
	}
	root := s.runs(tenant)
	ents, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, core.Wrap(core.CodeInternal, err)
	}
	n := 0
	for _, e := range ents {
		run, ok := strings.CutSuffix(e.Name(), dirSuffix)
		if !e.IsDir() || !ok || run == "" {
			continue
		}
		var m Meta
		at := time.Time{}
		flow := ""
		if found, err := s.ReadJSON(tenant, core.RunID(run), MetaFile, &m); err == nil && found {
			at, flow = m.CreatedAt, m.Flow
		}
		if at.IsZero() {
			info, err := e.Info()
			if err != nil {
				continue
			}
			at = info.ModTime()
		}
		if !expired(flow, at) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return n, core.Wrap(core.CodeInternal, err)
		}
		n++
	}
	return n, nil
}
