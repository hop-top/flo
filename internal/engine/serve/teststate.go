package serve

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/nodes/dispatch"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

// seededState gives a test run the state it is seeded with, whatever the
// sandbox store holds: the run reads the seed, and what it stages and commits
// goes to the sandbox store, which is deleted.
type seededState struct {
	core.StateStore
	seed map[string]any
}

func (s seededState) Snapshot(context.Context, core.TenantID, string) (map[string]any, error) {
	return maps.Clone(s.seed), nil
}

// seedChoice is the state a test starts from. seed is the overlay on the
// declared defaults; nil is the defaults themselves.
type seedChoice struct {
	source string
	seed   map[string]any
}

// resolveSeed picks the state of the base run (flowtest.State*): --state if
// given; else, in replay, record/state.json when there is one; else the
// declared defaults. The seed is validated against the declared state.
func (e *Engine) resolveSeed(ctx context.Context, tenant core.TenantID, v core.FlowVersion, decls map[string]specrun.StateDecl, req TestRequest) (seedChoice, error) {
	switch req.State {
	case "default":
		return seedChoice{source: flowtest.StateDefault}, nil
	case "current":
		live, err := e.Services.State.Snapshot(ctx, tenant, v.Name)
		if err != nil {
			return seedChoice{}, err //nolint:wrapcheck // *core.Error
		}
		seed := map[string]any{}
		for k, val := range live { // keys an older version declared are not this one's
			if _, ok := decls[k]; ok {
				seed[k] = val
			}
		}
		if err := flowtest.ValidateSeed(decls, seed, "the current state"); err != nil {
			return seedChoice{}, err //nolint:wrapcheck // *core.Error
		}
		return seedChoice{source: flowtest.StateCurrent, seed: seed}, nil
	case "":
	default:
		seed, err := flowtest.ReadSeedFile(req.State)
		if err != nil {
			return seedChoice{}, err //nolint:wrapcheck // *core.Error
		}
		if err := flowtest.ValidateSeed(decls, seed, "--state "+req.State); err != nil {
			return seedChoice{}, err //nolint:wrapcheck // *core.Error
		}
		return seedChoice{source: flowtest.StateFile + req.State, seed: seed}, nil
	}
	if req.Mode != flowtest.ModeRecord && v.Bundle != "" {
		p := filepath.Join(v.Bundle, recordDir, stateRecord)
		b, err := os.ReadFile(p) //nolint:gosec // inside the bundle
		if err == nil {
			seed, err := flowtest.DecodeSeed(b, recordDir+"/"+stateRecord)
			if err != nil {
				return seedChoice{}, err //nolint:wrapcheck // *core.Error
			}
			if err := flowtest.ValidateSeed(decls, seed, recordDir+"/"+stateRecord); err != nil {
				return seedChoice{}, err //nolint:wrapcheck // *core.Error
			}
			return seedChoice{source: flowtest.StateRecorded, seed: seed}, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return seedChoice{}, core.Errorf(core.CodeUsage, "%s/%s: %v", recordDir, stateRecord, err)
		}
	}
	return seedChoice{source: flowtest.StateDefault}, nil
}

// fullState is the state a run starts with: the declared defaults under the
// seed.
func fullState(decls map[string]specrun.StateDecl, seed map[string]any) map[string]any {
	out := map[string]any{}
	for k, d := range decls {
		if n, err := core.NormalizeJSON(d.Default); err == nil {
			out[k] = n
		} else {
			out[k] = d.Default
		}
	}
	maps.Copy(out, seed)
	return out
}

// baseExpects are the expectations of the documents that run on the base seed.
func baseExpects(cases []flowtest.Case) []flowtest.Expect {
	var out []flowtest.Expect
	for _, c := range cases {
		if c.State == nil {
			out = append(out, c.Expects...)
		}
	}
	return out
}

// PlannedWrite is a step a test would plan instead of run.
type PlannedWrite struct {
	Step       string `json:"step"`
	Node       string `json:"node"`
	SideEffect string `json:"side_effect"`
}

// TestPlan is what a test would do, found without running anything.
type TestPlan struct {
	Flow        string `json:"flow"`
	Version     string `json:"version"`
	Hash        string `json:"hash"`
	Mode        string `json:"mode"`
	StateSource string `json:"state_source"`
	// Seed is the state the run would start from (declared defaults under the
	// seed); values of keys a node lists under redact are not shown.
	Seed map[string]any `json:"seed"`
	// Cassettes says whether the tape would be read or written, where, and
	// the cassette files there now.
	CassetteAction string         `json:"cassette_action"`
	CassetteDir    string         `json:"cassette_dir,omitempty"`
	Cassettes      []string       `json:"cassettes"`
	Writes         []PlannedWrite `json:"planned_writes"`
	// Cases are the tests/ documents that run on a state of their own.
	Cases []string `json:"cases,omitempty"`
}

// PlanTest resolves a test as TestFlow does, with the same refusals, and
// reports what it would do. It starts no sandbox and writes nothing.
func (e *Engine) PlanTest(ctx context.Context, tenant core.TenantID, req TestRequest) (TestPlan, error) {
	p, err := e.prepareTest(ctx, tenant, req)
	if err != nil {
		return TestPlan{}, err
	}
	plan := TestPlan{
		Flow: p.v.Name, Version: p.v.Version, Hash: p.v.Hash, Mode: string(p.req.Mode), StateSource: p.choice.source,
		Seed: fullState(p.set.State, p.choice.seed), Cassettes: []string{}, Writes: []PlannedWrite{},
	}
	if p.req.Mode == flowtest.ModeRecord {
		plan.CassetteAction = "record"
	} else {
		plan.CassetteAction = "replay"
	}
	if p.v.Bundle != "" {
		plan.CassetteDir = filepath.Join(p.v.Bundle, recordDir, cassetteDir)
		if ents, err := os.ReadDir(plan.CassetteDir); err == nil {
			for _, ent := range ents {
				plan.Cassettes = append(plan.Cassettes, ent.Name())
			}
		}
	}
	for _, c := range p.cases {
		if c.State != nil {
			plan.Cases = append(plan.Cases, c.Name)
		}
	}
	uses, err := e.flowSteps(p.v, p.flow)
	if err != nil {
		return TestPlan{}, err
	}
	redact := map[string]bool{}
	for _, u := range uses {
		switch {
		case u.Spec != nil:
			for _, r := range u.Spec.Redact {
				redact[lastSegment(r)] = true
			}
			if !dispatch.ExecutesInDryRun(u.Spec.SideEffect) {
				plan.Writes = append(plan.Writes, PlannedWrite{Step: u.ID, Node: u.Spec.Node, SideEffect: string(u.Spec.SideEffect)})
			}
		case u.Exec:
			plan.Writes = append(plan.Writes, PlannedWrite{Step: u.ID, Node: "exec", SideEffect: string(node.SideEffectWriteShared)})
		}
	}
	for k := range plan.Seed {
		if redact[k] {
			plan.Seed[k] = "[redacted]"
		}
	}
	return plan, nil
}

func lastSegment(path string) string {
	if i := strings.LastIndexAny(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}
