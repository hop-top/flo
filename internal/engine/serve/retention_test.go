//go:build !windows

package serve_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/runrec"
)

// applyRenamed stores dependency-watch as another flow, with extra top-level
// keys, and grants what its nodes declare.
func (r *rig) applyRenamed(t *testing.T, name, extra string) core.FlowVersion {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), name)
	copyTree(t, testdata("flows", "dependency-watch"), bundle)
	p := filepath.Join(bundle, "flow.yaml")
	b, err := os.ReadFile(p) //nolint:gosec // a temp copy
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(b), "flow: dependency-watch\n", "flow: "+name+"\n"+extra, 1)
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return r.applyBundle(t, bundle, "vcs.read")
}

// age moves the creation time of a recording back.
func (r *rig) age(t *testing.T, run core.RunID, by time.Duration) {
	t.Helper()
	meta, found, err := r.e.Recordings.Load(core.DefaultTenant, run)
	if err != nil || !found {
		t.Fatalf("load %s: %v %v", run, found, err)
	}
	meta.CreatedAt = time.Now().Add(-by)
	if err := r.e.Recordings.Save(core.DefaultTenant, meta); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingsAreKeptAsLongAsTheirFlowSays(t *testing.T) {
	r := newRig(t)
	day := 24 * time.Hour
	dflt := r.applyRenamed(t, "dw-default", "")
	short := r.applyRenamed(t, "dw-short", "retention: { recordings: 30d }\n")
	long := r.applyRenamed(t, "dw-long", "retention: { recordings: 365d }\n")

	type rec struct {
		name string
		flow string
		hash string
		age  time.Duration
		kept bool
	}
	recs := []*rec{
		{name: "default 40d", flow: "dw-default", hash: dflt.Hash, age: 40 * day, kept: true}, // under 90d
		{name: "default 100d", flow: "dw-default", hash: dflt.Hash, age: 100 * day},
		{name: "short 40d", flow: "dw-short", hash: short.Hash, age: 40 * day}, // over its 30d
		{name: "short 10d", flow: "dw-short", hash: short.Hash, age: 10 * day, kept: true},
		{name: "long 100d", flow: "dw-long", hash: long.Hash, age: 100 * day, kept: true}, // over 90d, under its 365d
		{name: "long 400d", flow: "dw-long", hash: long.Hash, age: 400 * day},
	}
	ids := make([]core.RunID, len(recs))
	for i, c := range recs {
		run := r.startRecorded(t, c.flow, c.hash)
		r.age(t, run.ID, c.age)
		ids[i] = run.ID
	}
	// A recording whose flow the engine does not know keeps the default.
	for name, age := range map[string]time.Duration{"gone-new": 40 * day, "gone-old": 100 * day} {
		if err := r.e.Recordings.Save(core.DefaultTenant, runrec.Meta{Run: name, Flow: "gone", CreatedAt: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := r.e.Pruner.Prune(bg, core.DefaultTenant); err != nil {
		t.Fatal(err)
	}
	for i, c := range recs {
		if got := r.e.Recordings.Exists(core.DefaultTenant, ids[i], runrec.MetaFile); got != c.kept {
			t.Errorf("%s: kept = %v, want %v", c.name, got, c.kept)
		}
	}
	if !r.e.Recordings.Exists(core.DefaultTenant, "gone-new", runrec.MetaFile) {
		t.Error("a recording of an unknown flow under the default was pruned")
	}
	if r.e.Recordings.Exists(core.DefaultTenant, "gone-old", runrec.MetaFile) {
		t.Error("a recording of an unknown flow over the default was kept")
	}
}
