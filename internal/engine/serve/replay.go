package serve

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/runrec"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// ReplaySchemaVersion is the version of the ReplayResult document.
const ReplaySchemaVersion = "1"

// ReplayRequest asks for a replay of a recorded run.
type ReplayRequest struct {
	Run core.RunID
	// Caller is the profile that replays; it must be allowed to use the
	// flow's run_as, as for a run.
	Caller string
	// Trigger, Vars and State replace the recorded trigger payload, vars and
	// starting state; they are needed for what a node's redact paths dropped
	// from the recording. Nil takes the recorded one.
	Trigger, Vars, State map[string]any
	// Timeout bounds the replay; zero is DefaultTestTimeout.
	Timeout time.Duration
}

// ReplayResult is what a replay of a recorded run did.
type ReplayResult struct {
	Schema  string `json:"schema" yaml:"schema"`
	Run     string `json:"run" yaml:"run"`
	Flow    string `json:"flow" yaml:"flow"`
	Version string `json:"version" yaml:"version"`
	Hash    string `json:"hash" yaml:"hash"`
	// Replay is the id of the run in the sandbox, which is gone.
	Replay         string    `json:"replay" yaml:"replay"`
	At             time.Time `json:"at" yaml:"at"`
	OriginalStatus string    `json:"original_status" yaml:"original_status"`
	Status         string    `json:"status" yaml:"status"`
	// Matches is true when the replay ended as the run did and no step differs.
	Matches bool `json:"matches" yaml:"matches"`
	// Diffs lists where the replay differs from the run. A redacted path is
	// not in the recording, so a step that used it can differ.
	Diffs    []string           `json:"diffs" yaml:"diffs"`
	Steps    []flowtest.Step    `json:"steps" yaml:"steps"`
	Problems []flowtest.Problem `json:"problems" yaml:"problems"`
	// Calls is how many exec and HTTP calls the replay made, and how many a
	// cassette answered: all of them, or the replay failed.
	Calls    int `json:"calls" yaml:"calls"`
	Replayed int `json:"replayed" yaml:"replayed"`
}

// ReplayRun runs a recorded run again in a sandbox engine, offline: its flow
// version, its recorded trigger payload, vars and starting state, and every
// process and request answered from the run's cassettes. Nothing is made for
// real, and nothing reaches the engine's stores: the sandbox's run, state,
// outputs and approvals are deleted afterwards, as a test's are. Nodes that
// write shared state or cannot be undone are not executed (their plan is
// recorded), and an approval is auto-approved, exactly as in `flo flow test`.
//
// The error is for refusals and faults: no such run, no recording or an
// unfinished or partial one, a caller who may not use run_as. A replay that
// ends differently from the run is a result with Matches false.
func (e *Engine) ReplayRun(ctx context.Context, tenant core.TenantID, req ReplayRequest) (ReplayResult, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	run, err := e.Services.Runs.GetRun(ctx, tenant, req.Run)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	rs := e.Recordings
	meta, found, err := e.loadClosed(ctx, tenant, run)
	if err != nil {
		return ReplayResult{}, err
	}
	if !found {
		return ReplayResult{}, core.Errorf(core.CodeNotFound, "run %s has no recording", run.ID).
			WithSuggestion("record a run with `flo run create --flow " + run.Flow.Name + " --record`")
	}
	if err := meta.Replayable(); err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	cassettes, err := rs.Cassettes(tenant, run.ID)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	if _, err := os.Stat(cassettes); err != nil {
		return ReplayResult{}, core.Errorf(core.CodeNotFound, "the cassettes of run %s are missing", run.ID).
			WithSuggestion("record the run again with `flo run create --flow " + run.Flow.Name + " --record`")
	}
	v, err := e.Services.Flows.GetVersion(ctx, tenant, meta.Flow, meta.Hash)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	f, err := cel.ParseFlow(bytes.NewReader(v.Source), v.Name+".yaml")
	if err != nil {
		return ReplayResult{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return ReplayResult{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	runAs := f.RunAs
	if runAs == "" {
		runAs = req.Caller
	}
	ok, err := e.Services.Grants.CanUseProfile(ctx, tenant, req.Caller, runAs)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	if !ok {
		return ReplayResult{}, core.Errorf(core.CodeGrantDenied, "%s may not replay this run as %s", req.Caller, runAs).
			WithSuggestion("flo grant set " + req.Caller + " profile.use:" + runAs)
	}

	for _, doc := range []struct {
		label string
		set   bool
	}{{"trigger", req.Trigger != nil}, {"vars", req.Vars != nil}, {"state", req.State != nil}} {
		if doc.set {
			continue
		}
		for _, d := range meta.Dropped {
			if strings.HasPrefix(d, doc.label+".") {
				return ReplayResult{}, core.Errorf(core.CodeUsage, "recorded input redacted: %s", d).
					WithDetail("dropped", meta.Dropped).
					WithSuggestion("supply it again with --trigger-file, --var/--var-file or --state-file")
			}
		}
	}
	trigger, _, err := rs.ReadDoc(tenant, run.ID, runrec.TriggerFile)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	vars, _, err := rs.ReadDoc(tenant, run.ID, runrec.VarsFile)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	state, _, err := rs.ReadDoc(tenant, run.ID, runrec.StateFile)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	if req.Trigger != nil {
		trigger = req.Trigger
	}
	if req.Vars != nil {
		vars = req.Vars
	}
	if req.State != nil {
		state = req.State
	}
	seed := map[string]any{}
	for k, val := range state { // keys an older version declared are not this one's
		if _, ok := set.State[k]; ok {
			seed[k] = val
		}
	}
	if err := flowtest.ValidateSeed(set.State, seed, "the recorded state"); err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}
	orig, err := e.Services.Runs.ListSteps(ctx, tenant, run.ID)
	if err != nil {
		return ReplayResult{}, err //nolint:wrapcheck // *core.Error
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scratch, err := os.MkdirTemp("", "flo-replay-")
	if err != nil {
		return ReplayResult{}, core.Wrap(core.CodeInternal, err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // the sandbox's throwaway root

	tape, err := flowtest.NewTape(flowtest.ModeReplay, filepath.Clean(cassettes))
	if err != nil {
		return ReplayResult{}, core.Wrap(core.CodeInternal, err)
	}
	sb, err := e.openSandbox(ctx, scratch, tape)
	if err != nil {
		return ReplayResult{}, err
	}
	defer func() { _ = sb.Close(context.WithoutCancel(ctx)) }()
	rep, recs, err := sb.runOnce(ctx, tenant, req.Caller, v, vars, trigger, seed)
	if err != nil {
		return ReplayResult{}, err
	}

	steps, _ := flowtest.Evaluate(rep, recs)
	res := ReplayResult{
		Schema: ReplaySchemaVersion, Run: string(run.ID), Flow: v.Name, Version: v.Version, Hash: v.Hash,
		Replay: string(rep.ID), At: e.Config.Clock.Now().UTC(), OriginalStatus: string(run.Status), Status: string(rep.Status),
		Steps: steps, Problems: []flowtest.Problem{}, Diffs: diffSteps(orig, recs),
	}
	if run.Status != rep.Status {
		res.Diffs = append([]string{fmt.Sprintf("the run ended %s, the replay ended %s", run.Status, rep.Status)}, res.Diffs...)
	}
	for _, m := range tape.Misses() {
		res.Problems = append(res.Problems, flowtest.Problem{Kind: flowtest.ProblemCassette, Message: "no recording for " + m})
	}
	res.Calls, res.Replayed = tape.Stats()
	res.Matches = len(res.Diffs) == 0 && len(res.Problems) == 0
	return res, nil
}

// diffSteps lists where the replay's step records differ from the run's: a
// step on one side only, another status, another output. A step the replay
// planned instead of running (a write) is not compared.
func diffSteps(orig, replay []core.StepRun) []string {
	key := func(r core.StepRun) string {
		if r.Element != nil {
			return fmt.Sprintf("%s[%d]", r.StepID, r.Element.Index)
		}
		return r.StepID
	}
	byKey := map[string]core.StepRun{}
	for _, r := range replay {
		byKey[key(r)] = r
	}
	seen := map[string]bool{}
	out := []string{}
	for _, o := range orig {
		k := key(o)
		seen[k] = true
		r, ok := byKey[k]
		switch {
		case !ok:
			out = append(out, "step "+k+" ran, and the replay did not reach it")
		case r.Plan != nil:
		case o.Status != r.Status:
			out = append(out, fmt.Sprintf("step %s was %s, the replay made it %s", k, o.Status, r.Status))
		case !sameJSON(o.Output, r.Output):
			out = append(out, "step "+k+" has another output in the replay")
		}
	}
	for _, r := range replay {
		if k := key(r); !seen[k] {
			out = append(out, "step "+k+" is new in the replay")
		}
	}
	return out
}

// sameJSON compares two outputs as JSON, with the ids the engine mints
// (out_01..., which differ in every run) taken as equal.
func sameJSON(a, b any) bool {
	na, ea := core.NormalizeJSON(a)
	nb, eb := core.NormalizeJSON(b)
	if ea != nil || eb != nil {
		return false
	}
	return reflect.DeepEqual(maskIDs(na), maskIDs(nb))
}

var mintedID = regexp.MustCompile(`^[a-z]{2,5}_[0-9a-z]{20,}$`)

func maskIDs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = maskIDs(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = maskIDs(val)
		}
		return out
	case string:
		if mintedID.MatchString(x) {
			return "<id>"
		}
	}
	return v
}

// closeWait is how long a replay of a run that has just ended waits for the
// engine to close its recording: the run is finished a moment before.
const closeWait = 5 * time.Second

func (e *Engine) loadClosed(ctx context.Context, tenant core.TenantID, run core.Run) (runrec.Meta, bool, error) {
	end := time.Now().Add(closeWait)
	for {
		meta, found, err := e.Recordings.Load(tenant, run.ID)
		if err != nil || !found || meta.Status == runrec.StatusComplete || !run.Status.Terminal() || time.Now().After(end) {
			return meta, found, err //nolint:wrapcheck // *core.Error
		}
		select {
		case <-ctx.Done():
			return meta, found, nil
		case <-time.After(25 * time.Millisecond):
		}
	}
}
