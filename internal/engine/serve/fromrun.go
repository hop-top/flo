package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/runrec"
)

// FromRunSchemaVersion is the version of the FromRunResult and FromRunPlan
// documents.
const FromRunSchemaVersion = "1"

// FromRunRequest asks for a regression test made from a recorded run.
type FromRunRequest struct {
	// Flow is the flow whose bundle receives the test; the run must be one of
	// its runs.
	Flow string
	Run  core.RunID
	// Ref names the version whose bundle is written (hash or document
	// version); empty is the newest. It must be the version the run executed.
	Ref string
	// Caller is the profile that tests; it must be allowed to use the flow's
	// run_as, as for a test.
	Caller string
	// Trigger, Vars and State replace the recorded trigger payload, vars and
	// starting state; they are how what a node's redact paths dropped from the
	// recording is supplied again. Vars are text, bound to their declared
	// types. Nil takes the recorded one.
	Trigger map[string]any
	Vars    map[string]string
	State   map[string]any
	// WriteExpect also writes tests/from-<run>.yaml: an expect: entry for each
	// step of the run, with the status it ended with.
	WriteExpect bool
	// AllowLocalData lets the recording of a local-only flow be copied into
	// the bundle.
	AllowLocalData bool
	// Timeout bounds the replay; zero is DefaultTestTimeout.
	Timeout time.Duration
}

// FromRunFile is one file the request writes into the bundle.
type FromRunFile struct {
	// Path is relative to the bundle.
	Path string `json:"path" yaml:"path"`
	// Action is create, or overwrite when the file is there already.
	Action string `json:"action" yaml:"action"`
}

// FromRunPlan is what a request would write: nothing has been.
type FromRunPlan struct {
	Schema  string `json:"schema" yaml:"schema"`
	Run     string `json:"run" yaml:"run"`
	Flow    string `json:"flow" yaml:"flow"`
	Version string `json:"version" yaml:"version"`
	Hash    string `json:"hash" yaml:"hash"`
	Bundle  string `json:"bundle" yaml:"bundle"`
	// Privacy is the flow's egress ceiling.
	Privacy string        `json:"privacy" yaml:"privacy"`
	Files   []FromRunFile `json:"files" yaml:"files"`
	// Cassettes is how many cassette files Files holds.
	Cassettes int `json:"cassettes" yaml:"cassettes"`
	// Warnings are what the owner should know before the bundle changes.
	Warnings []string `json:"warnings" yaml:"warnings"`
}

// FromRunResult is a request carried out: what was written, and the replay.
type FromRunResult struct {
	Schema   string        `json:"schema" yaml:"schema"`
	Run      string        `json:"run" yaml:"run"`
	Files    []FromRunFile `json:"files" yaml:"files"`
	Warnings []string      `json:"warnings" yaml:"warnings"`
	// Test is the replay of the bundle, whose result activation reads.
	Test flowtest.Result `json:"test" yaml:"test"`
}

// fromRunPrep is a request, checked: the files and their contents.
type fromRunPrep struct {
	plan     FromRunPlan
	bundle   string
	cassDir  string
	cassRels []string
	trigger  map[string]any
	state    map[string]any
	expect   []byte
	version  string
}

// PlanFromRun checks a request and says what it would write. It writes and
// runs nothing.
func (e *Engine) PlanFromRun(ctx context.Context, tenant core.TenantID, req FromRunRequest) (FromRunPlan, error) {
	p, err := e.prepareFromRun(ctx, tenant, req)
	if err != nil {
		return FromRunPlan{}, err
	}
	return p.plan, nil
}

// FromRun copies a recorded run into the bundle of its flow as a test: its
// cassettes, trigger payload and starting state into record/ (and, when asked,
// an expect: skeleton into tests/), then replays the bundle. The bundle's
// digest moves with the new files, so the result of the replay is the one
// activation looks for.
//
// It refuses a recording that is unfinished or partial, of a run that did not
// succeed, of another version of the flow than the bundle's, or of a
// local-only flow unless the owner allows it; and inputs a node's redact paths
// dropped must be supplied again. Nothing is written when it refuses.
//
// The error is for refusals and faults; a replay that fails the test is a
// Result with Passed false.
func (e *Engine) FromRun(ctx context.Context, tenant core.TenantID, req FromRunRequest) (FromRunResult, error) {
	p, err := e.prepareFromRun(ctx, tenant, req)
	if err != nil {
		return FromRunResult{}, err
	}
	rec := filepath.Join(p.bundle, recordDir)
	for _, rel := range p.cassRels {
		data, err := os.ReadFile(filepath.Join(p.cassDir, filepath.FromSlash(rel))) //nolint:gosec // inside the run's recording
		if err != nil {
			return FromRunResult{}, core.Wrap(core.CodeInternal, err)
		}
		if err := writeBundleFile(filepath.Join(rec, cassetteDir, filepath.FromSlash(rel)), data); err != nil {
			return FromRunResult{}, err
		}
	}
	for name, doc := range map[string]map[string]any{triggerRecord: p.trigger, stateRecord: p.state} {
		b, err := marshalJSONDoc(doc)
		if err != nil {
			return FromRunResult{}, err
		}
		if err := writeBundleFile(filepath.Join(rec, name), b); err != nil {
			return FromRunResult{}, err
		}
	}
	if p.expect != nil {
		if err := writeBundleFile(filepath.Join(p.bundle, expectDir, expectName(req.Run)), p.expect); err != nil {
			return FromRunResult{}, err
		}
	}
	res, err := e.TestFlow(ctx, tenant, TestRequest{
		Flow: req.Flow, Ref: p.version, Mode: flowtest.ModeReplay, Caller: req.Caller, Vars: req.Vars, Timeout: req.Timeout,
	})
	if err != nil {
		return FromRunResult{}, err
	}
	return FromRunResult{Schema: FromRunSchemaVersion, Run: string(req.Run), Files: p.plan.Files, Warnings: p.plan.Warnings, Test: res}, nil
}

const expectDir = "tests"

func expectName(run core.RunID) string { return "from-" + string(run) + ".yaml" }

func (e *Engine) prepareFromRun(ctx context.Context, tenant core.TenantID, req FromRunRequest) (fromRunPrep, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	run, err := e.Services.Runs.GetRun(ctx, tenant, req.Run)
	if err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	rs := e.Recordings
	meta, found, err := e.loadClosed(ctx, tenant, run)
	if err != nil {
		return fromRunPrep{}, err
	}
	if !found {
		return fromRunPrep{}, core.Errorf(core.CodeNotFound, "run %s has no recording", run.ID).
			WithSuggestion("record a run with `flo run create --flow " + run.Flow.Name + " --record`")
	}
	if err := meta.Replayable(); err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	if meta.RunStatus != string(core.RunSucceeded) {
		return fromRunPrep{}, core.Errorf(core.CodeUsage, "run %s ended %s: a test made from it could not pass", run.ID, meta.RunStatus).
			WithSuggestion("make a test from a run that succeeded")
	}
	if meta.Flow != req.Flow {
		return fromRunPrep{}, core.Errorf(core.CodeUsage, "run %s is a run of flow %s, not %s", run.ID, meta.Flow, req.Flow)
	}
	if !meta.MayLeave() && !req.AllowLocalData {
		return fromRunPrep{}, core.Errorf(core.CodeUsage, "flow %s is local-only: the recording of run %s holds data that stays on this machine, and a bundle may be committed or shared", meta.Flow, run.ID).
			WithSuggestion("--allow-local-data copies it into the bundle anyway (keep record/ out of version control), or make the flow cloud-allowed")
	}
	v, err := e.Services.Flows.GetVersion(ctx, tenant, meta.Flow, req.Ref)
	if err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	if v.Hash != meta.Hash {
		err := core.Errorf(core.CodeUsage, "run %s ran version %s of flow %s (%s); the version to write is %s (%s)", run.ID, meta.Version, meta.Flow, short(meta.Hash), v.Version, short(v.Hash)).
			WithDetail("recorded_hash", meta.Hash).WithDetail("bundle_hash", v.Hash)
		if _, gerr := e.Services.Flows.GetVersion(ctx, tenant, meta.Flow, meta.Hash); gerr == nil {
			return fromRunPrep{}, err.WithSuggestion("--version " + meta.Hash + " writes the version the run executed")
		}
		return fromRunPrep{}, err.WithSuggestion("record the flow again with `flo run create --flow " + meta.Flow + " --record`")
	}
	cassDir, err := rs.Cassettes(tenant, run.ID)
	if err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	cassRels, err := listFiles(cassDir)
	if err != nil {
		return fromRunPrep{}, core.Errorf(core.CodeNotFound, "the cassettes of run %s are missing", run.ID).
			WithSuggestion("record the run again with `flo run create --flow " + run.Flow.Name + " --record`")
	}
	if err := requireDropped(meta, req.Trigger != nil, req.Vars != nil, req.State != nil); err != nil {
		return fromRunPrep{}, err
	}
	tp, err := e.prepareTest(ctx, tenant, TestRequest{Flow: req.Flow, Ref: v.Hash, Mode: flowtest.ModeReplay, Caller: req.Caller, Vars: req.Vars})
	if err != nil {
		return fromRunPrep{}, err
	}
	if tp.v.Bundle == "" {
		return fromRunPrep{}, core.Errorf(core.CodeUsage, "flow %s was applied from a single file: a test needs a bundle directory to keep record/ in", v.Name).
			WithSuggestion("move the flow to <dir>/flow.yaml and apply the directory")
	}

	trigger, _, err := rs.ReadDoc(tenant, run.ID, runrec.TriggerFile)
	if err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	recVars, _, err := rs.ReadDoc(tenant, run.ID, runrec.VarsFile)
	if err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	state, _, err := rs.ReadDoc(tenant, run.ID, runrec.StateFile)
	if err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}
	if req.Trigger != nil {
		trigger = req.Trigger
	}
	if req.State != nil {
		state = req.State
	}
	seed := map[string]any{}
	for k, val := range state { // keys an older version declared are not this one's
		if _, ok := tp.set.State[k]; ok {
			seed[k] = val
		}
	}
	if err := flowtest.ValidateSeed(tp.set.State, seed, "the recorded state"); err != nil {
		return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
	}

	p := fromRunPrep{
		bundle: tp.v.Bundle, cassDir: cassDir, cassRels: cassRels, trigger: orEmpty(trigger),
		state: fullState(tp.set.State, seed), version: tp.v.Hash,
		plan: FromRunPlan{
			Schema: FromRunSchemaVersion, Run: string(run.ID), Flow: v.Name, Version: v.Version, Hash: v.Hash, Bundle: tp.v.Bundle,
			Privacy: meta.Privacy, Files: []FromRunFile{}, Cassettes: len(cassRels), Warnings: []string{},
		},
	}
	add := func(rel string) {
		act := "create"
		if _, err := os.Stat(filepath.Join(p.bundle, filepath.FromSlash(rel))); err == nil {
			act = "overwrite"
		}
		p.plan.Files = append(p.plan.Files, FromRunFile{Path: rel, Action: act})
	}
	for _, rel := range cassRels {
		add(slashJoin(recordDir, cassetteDir, rel))
	}
	add(slashJoin(recordDir, triggerRecord))
	add(slashJoin(recordDir, stateRecord))
	if req.WriteExpect {
		orig, err := e.Services.Runs.ListSteps(ctx, tenant, run.ID)
		if err != nil {
			return fromRunPrep{}, err //nolint:wrapcheck // *core.Error
		}
		if p.expect, err = expectSkeleton(run.ID, orig); err != nil {
			return fromRunPrep{}, err
		}
		add(slashJoin(expectDir, expectName(run.ID)))
	}

	if !meta.MayLeave() {
		p.plan.Warnings = append(p.plan.Warnings, "flow is local-only: its recorded data is copied into the bundle at your request; keep record/ out of version control")
	}
	if req.Trigger != nil || req.State != nil {
		p.plan.Warnings = append(p.plan.Warnings, "the trigger payload and state you supplied are written to record/ as given")
	}
	if names := differingVars(recVars, tp.vars, req.Vars); len(names) > 0 {
		p.plan.Warnings = append(p.plan.Warnings, "the run used other values for vars "+strings.Join(names, ", ")+
			": a bundle test binds the declared defaults, so calls that depend on them may find no cassette; pass --var to replay with other values")
	}
	return p, nil
}

// requireDropped refuses a recording whose dropped inputs (the ones a node's
// redact paths removed) were not supplied again.
func requireDropped(meta runrec.Meta, trigger, vars, state bool) error {
	for _, doc := range []struct {
		label string
		set   bool
	}{{"trigger", trigger}, {"vars", vars}, {"state", state}} {
		if doc.set {
			continue
		}
		for _, d := range meta.Dropped {
			if strings.HasPrefix(d, doc.label+".") {
				return core.Errorf(core.CodeUsage, "recorded input redacted: %s", d).
					WithDetail("dropped", meta.Dropped).
					WithSuggestion("supply it again with --trigger-file, --var/--var-file or --state-file")
			}
		}
	}
	return nil
}

// differingVars names the vars the run was made with that the bundle test will
// not bind to the same value: those the caller did not supply and whose
// recorded value is not the declared default.
func differingVars(recorded, bound map[string]any, supplied map[string]string) []string {
	var out []string
	for k, rv := range recorded {
		if _, ok := supplied[k]; ok {
			continue
		}
		if bv, ok := bound[k]; !ok || !sameJSON(rv, bv) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// expectSkeleton is the tests/ document of a run: one expect for each step of
// the run, with the status it ended with. What a step produced is left for the
// owner to pin.
func expectSkeleton(run core.RunID, steps []core.StepRun) ([]byte, error) {
	type entry struct {
		Step   string `yaml:"step"`
		Status string `yaml:"status"`
	}
	doc := struct {
		Expect []entry `yaml:"expect"`
	}{Expect: []entry{}}
	seen := map[string]bool{}
	for _, s := range steps {
		if s.Element != nil || seen[s.StepID] {
			continue
		}
		seen[s.StepID] = true
		doc.Expect = append(doc.Expect, entry{Step: s.StepID, Status: string(s.Status)})
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	head := fmt.Sprintf("# From run %s: the status each step ended with.\n# Add `output:` to a step to pin what it produced.\n", run)
	return append([]byte(head), body...), nil
}

// listFiles lists the regular files under dir, relative and slash-separated.
func listFiles(dir string) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err //nolint:wrapcheck // the caller says what is missing
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return rerr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

func writeBundleFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

func slashJoin(parts ...string) string { return strings.Join(parts, "/") }

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func marshalJSONDoc(doc map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	return append(b, '\n'), nil
}
