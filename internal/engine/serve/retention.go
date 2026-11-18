package serve

import (
	"bytes"
	"context"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// pruneRecordings is the Pruner's second duty: it deletes the recordings
// that outlived the retention of their flow (`retention.recordings`, default
// core.OutputRetention). A recording of a flow the engine does not know
// (deleted, or its document no longer decodes) keeps the default.
func (e *Engine) pruneRecordings(ctx context.Context, tenant core.TenantID, _ time.Time) error {
	keep, err := e.recordingRetention(ctx, tenant)
	if err != nil {
		return err
	}
	_, err = e.Recordings.PruneBy(tenant, e.Config.Clock.Now(), func(flow string) time.Duration {
		if d, ok := keep[flow]; ok {
			return d
		}
		return core.OutputRetention
	})
	return err //nolint:wrapcheck // *core.Error
}

// recordingRetention maps each flow to the retention its current version
// (the active one, else the newest) asks for. Flows that say nothing are
// left out.
func (e *Engine) recordingRetention(ctx context.Context, tenant core.TenantID) (map[string]time.Duration, error) {
	flows, err := e.Services.Flows.ListFlows(ctx, tenant)
	if err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	out := map[string]time.Duration{}
	for _, s := range flows {
		v := s.Latest
		if s.Active != nil {
			v = *s.Active
		}
		f, err := cel.ParseFlow(bytes.NewReader(v.Source), v.Name+".yaml")
		if err != nil {
			continue
		}
		set, err := specrun.Decode(f)
		if err != nil || set.Retention.Recordings <= 0 {
			continue
		}
		out[s.Name] = set.Retention.Recordings
	}
	return out, nil
}
