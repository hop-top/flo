package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

const runSchemaVersion = "1.0"

// waitPoll is how often `run create --wait` looks at the run.
const waitPoll = 100 * time.Millisecond

// runRow is one run as `flo run list` prints it.
type runRow struct {
	ID      string `table:"ID" json:"id" yaml:"id"`
	Flow    string `table:"FLOW" json:"flow" yaml:"flow"`
	Version string `table:"VERSION" json:"version" yaml:"version"`
	Status  string `table:"STATUS" json:"status" yaml:"status"`
	Trigger string `table:"TRIGGER" json:"trigger" yaml:"trigger"`
	Created string `table:"CREATED" json:"created_at" yaml:"created_at"`
	Ended   string `table:"ENDED" json:"finished_at,omitempty" yaml:"finished_at,omitempty"`
}

func runRows(rs []core.Run) []runRow {
	rows := make([]runRow, len(rs))
	for i, r := range rs {
		rows[i] = runRow{
			ID: string(r.ID), Flow: r.Flow.Name, Version: r.Flow.Version, Status: string(r.Status),
			Trigger: string(r.Trigger.Kind), Created: stampTime(r.CreatedAt), Ended: stamp(r.FinishedAt),
		}
	}
	return rows
}

// runCreated is what `flo run create` reports.
type runCreated struct {
	Run     string `json:"run,omitempty" yaml:"run,omitempty"`
	Flow    string `json:"flow" yaml:"flow"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	Hash    string `json:"hash,omitempty" yaml:"hash,omitempty"`
	Status  string `json:"status,omitempty" yaml:"status,omitempty"`
	// Created is false when the idempotency key already had a run: Run is that run.
	Created bool `json:"created" yaml:"created"`
	// Queued: the run waits for a slot of its concurrency group. Skipped: the
	// group was full and on_overlap is skip, so no run was made.
	Queued  bool     `json:"queued,omitempty" yaml:"queued,omitempty"`
	Skipped bool     `json:"skipped,omitempty" yaml:"skipped,omitempty"`
	Cancels []string `json:"cancelled,omitempty" yaml:"cancelled,omitempty"`
	// Recording is true when the run's exec and HTTP traffic is recorded
	// (--record); `flo run replay` replays it once the run has ended.
	Recording bool `json:"recording,omitempty" yaml:"recording,omitempty"`
	// Waiting lists the approvals an awaiting run waits on (--wait).
	Waiting []string      `json:"waiting_on,omitempty" yaml:"waiting_on,omitempty"`
	Result  any           `json:"result,omitempty" yaml:"result,omitempty"`
	Failure *core.Failure `json:"failure,omitempty" yaml:"failure,omitempty"`
}

// stepView is one step record of a run.
type stepView struct {
	ID       string        `json:"id" yaml:"id"`
	Step     string        `json:"step" yaml:"step"`
	Index    *int          `json:"index,omitempty" yaml:"index,omitempty"`
	Kind     string        `json:"kind" yaml:"kind"`
	Node     string        `json:"node,omitempty" yaml:"node,omitempty"`
	Status   string        `json:"status" yaml:"status"`
	Reason   string        `json:"reason,omitempty" yaml:"reason,omitempty"`
	Attempts int           `json:"attempts" yaml:"attempts"`
	Cost     core.Cost     `json:"cost" yaml:"cost"`
	Output   any           `json:"output,omitempty" yaml:"output,omitempty"`
	Approval string        `json:"approval,omitempty" yaml:"approval,omitempty"`
	Receipts []string      `json:"receipts,omitempty" yaml:"receipts,omitempty"`
	Failure  *core.Failure `json:"failure,omitempty" yaml:"failure,omitempty"`
	Started  string        `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	Finished string        `json:"finished_at,omitempty" yaml:"finished_at,omitempty"`
}

// runApprovalView is an approval of the run, as the run lists it.
type runApprovalView struct {
	ID      string `json:"id" yaml:"id"`
	Step    string `json:"step" yaml:"step"`
	Status  string `json:"status" yaml:"status"`
	Waiting int    `json:"waiting" yaml:"waiting"`
}

// runOutputView is an output the run published.
type runOutputView struct {
	ID    string `json:"id" yaml:"id"`
	Step  string `json:"step" yaml:"step"`
	Kind  string `json:"kind" yaml:"kind"`
	Title string `json:"title,omitempty" yaml:"title,omitempty"`
	Items int    `json:"items" yaml:"items"`
}

// runView is the run tree: the run, its step records, approvals and outputs.
type runView struct {
	ID         string            `json:"id" yaml:"id"`
	Flow       string            `json:"flow" yaml:"flow"`
	Version    string            `json:"version" yaml:"version"`
	Hash       string            `json:"hash" yaml:"hash"`
	Status     string            `json:"status" yaml:"status"`
	Trigger    string            `json:"trigger" yaml:"trigger"`
	IdemKey    string            `json:"idempotency_key,omitempty" yaml:"idempotency_key,omitempty"`
	StartedBy  string            `json:"started_by,omitempty" yaml:"started_by,omitempty"`
	RunAs      string            `json:"run_as,omitempty" yaml:"run_as,omitempty"`
	Vars       map[string]any    `json:"vars,omitempty" yaml:"vars,omitempty"`
	Usage      core.Usage        `json:"usage" yaml:"usage"`
	WaitingFor string            `json:"waiting_for,omitempty" yaml:"waiting_for,omitempty"`
	TaskRef    string            `json:"task,omitempty" yaml:"task,omitempty"`
	Cancelling bool              `json:"cancel_requested,omitempty" yaml:"cancel_requested,omitempty"`
	Result     any               `json:"result,omitempty" yaml:"result,omitempty"`
	Failure    *core.Failure     `json:"failure,omitempty" yaml:"failure,omitempty"`
	Created    string            `json:"created_at" yaml:"created_at"`
	Started    string            `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	Finished   string            `json:"finished_at,omitempty" yaml:"finished_at,omitempty"`
	Steps      []stepView        `json:"steps" yaml:"steps"`
	Approvals  []runApprovalView `json:"approvals" yaml:"approvals"`
	Outputs    []runOutputView   `json:"outputs" yaml:"outputs"`
}

// runCancelled is what `flo run cancel` reports.
type runCancelled struct {
	Run    string `table:"RUN" json:"run" yaml:"run"`
	Status string `table:"STATUS" json:"status" yaml:"status"`
	// Changed is false when the run already was cancelled (or finished).
	Changed bool `table:"CHANGED" json:"changed" yaml:"changed"`
}

func stepViewOf(s core.StepRun) stepView {
	v := stepView{
		ID: string(s.ID), Step: s.StepID, Kind: string(s.Kind), Node: s.Node, Status: string(s.Status), Reason: string(s.Reason),
		Attempts: len(s.Attempts), Cost: s.Cost, Output: s.Output, Approval: string(s.Approval), Failure: s.Failure,
		Started: stamp(s.StartedAt), Finished: stamp(s.FinishedAt),
	}
	if s.Element != nil {
		i := s.Element.Index
		v.Index = &i
	}
	for _, r := range s.Receipts {
		v.Receipts = append(v.Receipts, string(r))
	}
	return v
}

func runViewOf(r core.Run, steps []core.StepRun, aps []core.Approval, outs []core.Output) runView {
	v := runView{
		ID: string(r.ID), Flow: r.Flow.Name, Version: r.Flow.Version, Hash: r.Flow.Hash, Status: string(r.Status),
		Trigger: string(r.Trigger.Kind), IdemKey: r.IdemKey, StartedBy: r.StartedBy, RunAs: r.RunAs, Vars: r.Vars,
		Usage: r.Usage, WaitingFor: r.WaitingFor, TaskRef: r.TaskRef, Cancelling: r.CancelRequested, Result: r.Result,
		Failure: r.Failure, Created: stampTime(r.CreatedAt), Started: stamp(r.StartedAt), Finished: stamp(r.FinishedAt),
		Steps: []stepView{}, Approvals: []runApprovalView{}, Outputs: []runOutputView{},
	}
	for _, s := range steps {
		v.Steps = append(v.Steps, stepViewOf(s))
	}
	for _, a := range aps {
		v.Approvals = append(v.Approvals, runApprovalView{ID: string(a.ID), Step: a.StepID, Status: string(a.Status), Waiting: len(a.Undecided())})
	}
	for _, o := range outs {
		v.Outputs = append(v.Outputs, runOutputView{ID: string(o.ID), Step: o.StepID, Kind: string(o.Kind), Title: o.Title, Items: len(o.Items)})
	}
	return v
}

// addRun registers the `flo run` noun: create, list, show, logs, cancel, replay.
// alive, when set, reports whether an engine serves (run create --wait).
func addRun(root *kitcli.Root, svc ServicesFunc, eng EngineFunc, alive func() error) {
	r := &cobra.Command{Use: "run", Short: "runs: start, inspect, follow, cancel and replay"}
	r.AddCommand(runCreateCmd(root, svc, alive), runListCmd(root, svc), runShowCmd(root, svc), runLogsCmd(root, svc), runCancelCmd(root, svc), runReplayCmd(root, eng))
	root.Cmd.AddCommand(r)
}

func runCreateCmd(root *kitcli.Root, svc ServicesFunc, alive func() error) *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Start a run of a flow",
		Long: "Start a run of the active version of a flow (--version pins another): bind --var values " +
			"to the declared types, check that you may use the flow's run_as, store the run and queue it " +
			"for the engine. The same --idempotency-key returns the first run, not a second. With --wait " +
			"it waits for the run to settle: exit 0 when it succeeded, 7 (awaiting_approval) when it " +
			"parked on an approval, else the failure's exit code. --wait needs an engine serving.\n\n" +
			"--record keeps the run's exec and HTTP calls (and its trigger payload, vars and starting state) " +
			"for `flo run replay`: the engine makes the calls for real, as for any run, and writes a " +
			"recording beside the run's log, readable by you only, kept as long as outputs (90 days). The " +
			"redact paths of the flow's nodes are dropped from it, env values and credential headers are " +
			"never stored, and a recording of a local-only flow stays on this machine. Only reads and pure " +
			"nodes are recorded: a replay does not execute writes.",
		Args: cobra.NoArgs,
	}
	flow := c.Flags().String("flow", "", "the flow to run")
	ver := c.Flags().String("version", "", "run this version (hash or document version) instead of the active one")
	vars := c.Flags().StringArray("var", nil, "a flow var: name=value, typed per its declaration (repeatable)")
	varFile := c.Flags().String("var-file", "", "a JSON object of vars; --var wins over it")
	trigFile := c.Flags().String("trigger-file", "", "a JSON object read as the run's trigger payload (trigger.<key> in expressions); - reads standard input")
	record := c.Flags().Bool("record", false, "record the run's exec and HTTP calls for `flo run replay`")
	wait := c.Flags().Bool("wait", false, "wait until the run succeeds, fails, is cancelled or parks on an approval")
	timeout := c.Flags().Duration("timeout", 0, "with --wait, give up after this long (exit 6); 0 waits for ever")
	_ = c.MarkFlagRequired("flow")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		// --idempotency-key is kit's flag, registered on a conditional-idempotent
		// write; here it is the run key.
		key, _ := cmd.Flags().GetString("idempotency-key")
		return runCreate(cmd, root, svc, alive, createArgs{
			flow: *flow, version: *ver, vars: *vars, varFile: *varFile, key: key, trigFile: *trigFile, record: *record, wait: *wait, timeout: *timeout,
		})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteShared)
	kitcli.SetIdempotency(c, kitcli.IdempotencyConditional)
	kitcli.SetRetryable(c, false)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &runCreated{}, Version: runSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "start a run", Command: "flo run create --flow dependency-watch"},
		{Title: "start it once per day, wait for the outcome", Command: "flo run create --flow dependency-watch --idempotency-key 2026-11-18 --wait"},
		{Title: "record the run, to replay it offline", Command: "flo run create --flow dependency-watch --record --wait"},
		{Title: "bind vars", Command: "flo run create --flow dependency-watch --var lookback=7d --var repos='[\"example-org/api\"]'"},
		{Title: "pass a trigger payload on standard input", Command: "echo '{\"event\":{\"id\":\"e1\"}}' | flo run create --flow dependency-watch --trigger-file - --idempotency-key e1"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the run was created", Suggest: "flo run logs <run> --follow", Reason: "follow its events"},
		{When: "the run was recorded and has ended", Suggest: "flo run replay <run>", Reason: "run it again offline, in a sandbox"},
		{When: "exit 7", Suggest: "flo approval list --status open", Reason: "decide what the run waits for"},
	})
	return c
}

type createArgs struct {
	flow, version, varFile, key, trigFile string
	vars                                  []string
	wait, record                          bool
	timeout                               time.Duration
}

func runCreate(cmd *cobra.Command, root *kitcli.Root, svc ServicesFunc, alive func() error, a createArgs) error {
	provided, err := parseVars(a.vars, a.varFile)
	if err != nil {
		return err
	}
	payload, err := readTriggerFile(cmd.InOrStdin(), a.trigFile)
	if err != nil {
		return err
	}
	s, tenant, err := accessServices(cmd, root, svc)
	if err != nil {
		return err
	}
	caller, err := accessCaller(root)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if a.wait && alive != nil {
		if err := alive(); err != nil {
			return err
		}
	}
	var res core.StartResult
	if existing, ok, err := lookupKey(ctx, s, tenant, a, caller); err != nil {
		return err
	} else if ok {
		res = core.StartResult{Run: existing}
	} else {
		req, err := startRequest(ctx, s, tenant, caller, a, provided, payload)
		if err != nil {
			return err
		}
		if kitcli.IsDryRun(cmd) {
			return kitcli.RenderPlan(cmd, kitcli.Plan{
				Args:    map[string]any{"flow": req.Flow, "version": req.Version, "vars": req.Vars, "idempotency_key": req.IdemKey, "by": caller, "record": req.Record},
				Effects: dryRunEffects(req),
			})
		}
		if res, err = s.Starter.Start(ctx, req); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
	}
	out := runCreated{Flow: a.flow, Created: res.Created, Queued: res.Queued, Skipped: res.Skipped, Recording: res.Run.Record}
	for _, id := range res.Cancelled {
		out.Cancels = append(out.Cancels, string(id))
	}
	if res.Skipped {
		return output.Dispatch(cmd, root.Viper, out)
	}
	fillRun(&out, res.Run)
	if !a.wait {
		return output.Dispatch(cmd, root.Viper, out)
	}
	final, err := waitRun(ctx, s, tenant, res.Run.ID, a.timeout)
	if err != nil {
		return err
	}
	fillRun(&out, final)
	return settle(cmd, root, s, tenant, final, out)
}

func fillRun(out *runCreated, r core.Run) {
	out.Run, out.Flow, out.Version, out.Hash, out.Status = string(r.ID), r.Flow.Name, r.Flow.Version, r.Flow.Hash, string(r.Status)
	out.Result, out.Failure = r.Result, r.Failure
}

// settle prints a run that --wait saw settle or park, then returns the error
// that carries its exit code: awaiting_approval (7), the failure's code, or
// none for a success.
func settle(cmd *cobra.Command, root *kitcli.Root, s *core.Services, tenant core.TenantID, r core.Run, out runCreated) error {
	var waitErr error
	switch r.Status {
	case core.RunAwaitingApproval:
		aps, err := s.ApprovalSvc.List(cmd.Context(), tenant, core.ApprovalFilter{Run: r.ID, Status: core.ApprovalOpen})
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		for _, a := range aps {
			out.Waiting = append(out.Waiting, string(a.ID))
		}
		waitErr = core.Errorf(core.CodeAwaitingApproval, "run %s waits for %d approval(s): %s", r.ID, len(out.Waiting), strings.Join(out.Waiting, ", ")).
			WithDetail("run", string(r.ID)).WithDetail("approvals", out.Waiting).
			WithSuggestion("flo approval show " + firstOr(out.Waiting, "<id>"))
	case core.RunFailed:
		waitErr = failureErr(r)
	case core.RunCancelled:
		waitErr = core.Errorf(core.CodeRunCancelled, "run %s was cancelled", r.ID)
	}
	if err := output.Dispatch(cmd, root.Viper, out); err != nil {
		return err //nolint:wrapcheck // kit output error
	}
	return waitErr
}

func firstOr(xs []string, def string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return def
}

// failureErr is the run's failure as an error: its code decides the exit.
func failureErr(r core.Run) error {
	if r.Failure == nil {
		return core.Errorf(core.CodeRunFailed, "run %s failed", r.ID)
	}
	e := r.Failure.Err()
	return e.WithDetail("run", string(r.ID)).WithSuggestion("flo run show " + string(r.ID))
}

// lookupKey finds the run an idempotency key already made, before anything
// else is checked: a repeat of a request that succeeded returns its run.
func lookupKey(ctx context.Context, s *core.Services, tenant core.TenantID, a createArgs, _ string) (core.Run, bool, error) {
	if a.key == "" {
		return core.Run{}, false, nil
	}
	return s.Runs.LookupIdem(ctx, tenant, a.flow, a.key) //nolint:wrapcheck // *core.Error
}

// startRequest resolves the flow version, binds the vars to their declared
// types and checks that the caller may use the flow's run_as.
func startRequest(ctx context.Context, s *core.Services, tenant core.TenantID, caller string, a createArgs, provided map[string]string, payload map[string]any) (core.StartRequest, error) {
	var fv core.FlowVersion
	var err error
	if a.version == "" {
		fv, err = s.Flows.Active(ctx, tenant, a.flow)
	} else {
		fv, err = s.Flows.GetVersion(ctx, tenant, a.flow, a.version)
	}
	if err != nil {
		return core.StartRequest{}, err //nolint:wrapcheck // *core.Error
	}
	f, err := cel.ParseFlow(bytes.NewReader(fv.Source), fv.Name+".yaml")
	if err != nil {
		return core.StartRequest{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	vars, err := specrun.ResolveVars(f, provided)
	if err != nil {
		return core.StartRequest{}, core.Wrap(core.CodeInvalidVar, err).WithSuggestion("--var name=value, typed per the flow's vars")
	}
	runAs := f.RunAs
	if runAs == "" {
		runAs = caller
	}
	ok, err := s.Grants.CanUseProfile(ctx, tenant, caller, runAs)
	if err != nil {
		return core.StartRequest{}, err //nolint:wrapcheck // *core.Error
	}
	if !ok {
		return core.StartRequest{}, core.Errorf(core.CodeGrantDenied, "%s may not run this flow as %s", caller, runAs).
			WithSuggestion("flo grant set " + caller + " profile.use:" + runAs)
	}
	return core.StartRequest{
		Tenant: tenant, Flow: a.flow, Version: fv.Hash, Vars: vars,
		Trigger: core.TriggerInfo{Kind: specrun.TriggerManual, Payload: payload}, IdemKey: a.key, StartedBy: caller, Record: a.record,
	}, nil
}

func dryRunEffects(req core.StartRequest) []kitcli.Effect {
	effects := []kitcli.Effect{{Kind: "create", Target: "run:" + req.Flow, Reversible: true, Detail: "a queued run the engine advances"}}
	if req.Record {
		effects = append(effects, kitcli.Effect{Kind: "write", Target: "recording:" + req.Flow, Reversible: true, Detail: "the run's calls, kept beside its log for `flo run replay`"})
	}
	return effects
}

// parseVars reads --var-file, then --var over it: name -> raw text, which
// ResolveVars coerces to the declared type. A JSON string stays its text; any
// other JSON value is its JSON literal (a list or map var is one).
func parseVars(flags []string, file string) (map[string]string, error) {
	out := map[string]string{}
	if file != "" {
		data, err := os.ReadFile(file) //nolint:gosec // the file the caller named
		if err != nil {
			return nil, core.Errorf(core.CodeUsage, "--var-file: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, core.Errorf(core.CodeUsage, "--var-file %s: want a JSON object: %v", file, err)
		}
		for k, raw := range m {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				out[k] = s
			} else {
				out[k] = string(raw)
			}
		}
	}
	for _, f := range flags {
		k, v, ok := strings.Cut(f, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, core.Errorf(core.CodeUsage, "--var %q: want name=value", f)
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

// readTriggerFile reads --trigger-file: a JSON object that becomes the run's
// trigger payload. "-" reads it from in (standard input); a file literally
// named "-" is reached as ./-.
func readTriggerFile(in io.Reader, path string) (map[string]any, error) {
	if path == "" {
		return nil, nil //nolint:nilnil // no file, no payload
	}
	var (
		data  []byte
		err   error
		label = path
	)
	if path == "-" {
		label = "stdin"
		data, err = io.ReadAll(in)
		if err != nil {
			return nil, core.Errorf(core.CodeUsage, "--trigger-file: read stdin: %v", err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, core.New(core.CodeUsage, "--trigger-file: stdin is empty: want a JSON object")
		}
	} else if data, err = os.ReadFile(path); err != nil { //nolint:gosec // the file the caller named
		return nil, core.Errorf(core.CodeUsage, "--trigger-file: %v", err)
	}
	v, err := core.DecodeJSONValue(data)
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "--trigger-file %s: not JSON: %v", label, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, core.Errorf(core.CodeUsage, "--trigger-file %s: want a JSON object", label)
	}
	return m, nil
}

// waitRun polls until the run is terminal or parked on an approval. A parked
// run is returned as it stands; a run that cannot advance because no engine
// serves is the caller's to notice (run create checked one was there).
func waitRun(ctx context.Context, s *core.Services, tenant core.TenantID, id core.RunID, timeout time.Duration) (core.Run, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		r, err := s.Runs.GetRun(ctx, tenant, id)
		if err != nil {
			if ctx.Err() != nil {
				return core.Run{}, core.Errorf(core.CodeTimeout, "run %s did not settle in time; it keeps running", id).
					WithSuggestion("flo run show " + string(id))
			}
			return core.Run{}, err //nolint:wrapcheck // *core.Error
		}
		if r.Status.Terminal() || r.Status == core.RunAwaitingApproval {
			return r, nil
		}
		select { // real time: a caller waits on the wall clock whatever clock the engine keeps
		case <-ctx.Done():
			return core.Run{}, core.Errorf(core.CodeTimeout, "run %s did not settle in time; it keeps running", id).
				WithSuggestion("flo run show " + string(id))
		case <-time.After(waitPoll):
		}
	}
}

func runListCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List runs, newest first",
		Long:  "List runs newest first. --flow and --status (queued, running, awaiting_approval, succeeded, failed, cancelled; repeatable) narrow it; --limit bounds it.",
		Args:  cobra.NoArgs,
	}
	flow := c.Flags().String("flow", "", "only runs of this flow")
	statuses := c.Flags().StringSlice("status", nil, "only runs in these states (repeatable)")
	limit := c.Flags().Int("limit", 0, "at most this many; 0 is the store default")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		f := core.RunFilter{Flow: *flow, Limit: *limit}
		for _, st := range *statuses {
			rs := core.RunStatus(st)
			if !rs.Valid() {
				return core.Errorf(core.CodeUsage, "--status %q: want queued, running, awaiting_approval, succeeded, failed or cancelled", st)
			}
			f.Statuses = append(f.Statuses, rs)
		}
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		rs, err := s.Runs.ListRuns(cmd.Context(), tenant, f)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, runRows(rs))
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &[]runRow{}, Version: runSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "what waits", Command: "flo run list --status awaiting_approval"},
		{Title: "one flow, as JSON", Command: "flo run list --flow dependency-watch --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "a run is listed", Suggest: "flo run show <run>", Reason: "see its steps"}})
	return c
}

func runShowCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "show <run>",
		Short: "Show a run: its steps, approvals and outputs",
		Long: "Show the run tree: the run with its status, vars, usage and outcome, every step record " +
			"(a for_each element has its own, with its index), the approvals it opened and the outputs it published.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := core.ParseRunID(args[0])
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			s, tenant, err := accessServices(cmd, root, svc)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			r, err := s.Runs.GetRun(ctx, tenant, id)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			steps, err := s.Runs.ListSteps(ctx, tenant, id)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			aps, err := s.ApprovalSvc.List(ctx, tenant, core.ApprovalFilter{Run: id})
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			outs, err := s.Outputs.ListOutputs(ctx, tenant, core.OutputFilter{Run: id})
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return output.Dispatch(cmd, root.Viper, runViewOf(r, steps, aps, outs))
		},
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &runView{}, Version: runSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "show a run", Command: "flo run show run_01 --format json"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the run awaits approval", Suggest: "flo approval show <id>", Reason: "see what it waits for"},
		{When: "the run failed", Suggest: "flo run logs <run>", Reason: "read its events"},
	})
	return c
}

func runLogsCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "logs <run>",
		Short: "Print a run's events as JSON Lines",
		Long: "Print the run's CloudEvents in order, one JSON object per line: the run's creation and " +
			"start, each step's start and end with its outputs and cost, approvals, and the terminal event. " +
			"--follow keeps printing as events arrive and stops after the run's terminal event.",
		Args: cobra.ExactArgs(1),
	}
	follow := c.Flags().BoolP("follow", "F", false, "keep printing new events until the run ends")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := core.ParseRunID(args[0])
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		if _, err := s.Runs.GetRun(cmd.Context(), tenant, id); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		rd, err := s.RunLog.Open(cmd.Context(), tenant, id, *follow)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		defer rd.Close() //nolint:errcheck // read only
		return streamEvents(cmd.Context(), rd, cmd.OutOrStdout())
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &map[string]any{}, Version: runSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "a run's events", Command: "flo run logs run_01"},
		{Title: "follow a run", Command: "flo run logs run_01 --follow"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "the run ended", Suggest: "flo run show <run>", Reason: "see its steps and outputs"}})
	return c
}

// streamEvents writes each event as one CloudEvents JSON line.
func streamEvents(ctx context.Context, rd core.EventReader, w io.Writer) error {
	for {
		ev, err := rd.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // the reader stopped because the caller did
			}
			return err //nolint:wrapcheck // *core.Error
		}
		line, err := bus.MarshalCloudEvent(ev)
		if err != nil {
			return core.Wrap(core.CodeInternal, err)
		}
		if _, err := fmt.Fprintf(w, "%s\n", bytes.TrimRight(line, "\n")); err != nil {
			return core.Wrap(core.CodeInternal, err)
		}
	}
}

func runCancelCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "cancel <run>",
		Short: "Cancel a run",
		Long: "Ask the engine to cancel the run: the request is recorded and the run gets a turn, which " +
			"stops its steps, rejects the undecided items of its approvals with reason run_cancelled and " +
			"finishes it cancelled. Cancelling a cancelled or finished run is success and changes nothing.",
		Args: cobra.ExactArgs(1),
	}
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := core.ParseRunID(args[0])
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		r, err := s.Runs.GetRun(ctx, tenant, id)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if kitcli.IsDryRun(cmd) {
			plan := kitcli.Plan{Args: map[string]any{"run": string(id), "status": string(r.Status)}, Effects: []kitcli.Effect{}}
			if !r.Status.Terminal() {
				plan.Effects = append(plan.Effects, kitcli.Effect{Kind: "cancel", Target: "run:" + string(id), Reversible: false})
			}
			return kitcli.RenderPlan(cmd, plan)
		}
		if r.Status.Terminal() {
			return output.Dispatch(cmd, root.Viper, runCancelled{Run: string(id), Status: string(r.Status)})
		}
		if err := confirmWrite(*yes, fmt.Sprintf("cancel run %s?", id)); err != nil {
			return err
		}
		upd, err := s.Runs.RequestCancel(ctx, tenant, id)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if !upd.Status.Terminal() {
			if err := s.Resumer.Resume(ctx, tenant, id); err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
		}
		return output.Dispatch(cmd, root.Viper, runCancelled{Run: string(id), Status: string(upd.Status), Changed: !r.CancelRequested})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteShared)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &runCancelled{}, Version: runSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "cancel a run", Command: "flo run cancel run_01 --yes"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "the run was cancelled", Suggest: "flo run show <run>", Reason: "see how it ended"}})
	return c
}

func runReplayCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "replay <run>",
		Short: "Replay a recorded run offline, in a sandbox",
		Long: "Run a run started with `flo run create --record` again: the flow version it ran, its recorded " +
			"trigger payload, vars and starting state, with every process and request answered from its " +
			"recording. Nothing is made for real and nothing reaches the live engine: the replay's run, " +
			"state, outputs and approvals live in a sandbox that is deleted afterwards. Nodes that write " +
			"shared state or cannot be undone are not executed (their plan is recorded), and an approval " +
			"is auto-approved, as in `flo flow test`.\n\n" +
			"It says where the replay differs from the run (status, step statuses, outputs). Paths a node " +
			"lists under redact are not in the recording, so a step that used one can differ. Exits 1 when " +
			"the replay differs. Inputs a node's redact paths dropped from the recording (trigger payload, " +
			"vars, state) are not stored: the replay is refused with the path until you supply them with " +
			"--trigger-file, --vars-file or --state-file. A run with no recording, an unfinished one, or a partial one is refused " +
			"with the reason.",
		Args: cobra.ExactArgs(1),
	}
	timeout := c.Flags().Duration("timeout", serve.DefaultTestTimeout, "give up after this long")
	trigFile := c.Flags().String("trigger-file", "", "a JSON object replacing the recorded trigger payload (needed when a redact path dropped part of it); - reads standard input")
	varsFile := c.Flags().String("vars-file", "", "a JSON object replacing the recorded vars (typed values)")
	stateFile := c.Flags().String("state-file", "", "a JSON object replacing the recorded starting state")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		trig, err := readTriggerFile(cmd.InOrStdin(), *trigFile)
		if err != nil {
			return err
		}
		vars, err := readJSONObject("--vars-file", *varsFile)
		if err != nil {
			return err
		}
		state, err := readJSONObject("--state-file", *stateFile)
		if err != nil {
			return err
		}
		id, err := core.ParseRunID(args[0])
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		caller, err := accessCaller(root)
		if err != nil {
			return err
		}
		res, err := e.ReplayRun(cmd.Context(), tenant, serve.ReplayRequest{Run: id, Caller: caller, Timeout: *timeout, Trigger: trig, Vars: vars, State: state})
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if err := output.Dispatch(cmd, root.Viper, res); err != nil {
			return err //nolint:wrapcheck // kit output error
		}
		if res.Matches {
			return nil
		}
		msg := "the replay differs from the run"
		switch {
		case len(res.Problems) > 0:
			msg = res.Problems[0].Message
		case len(res.Diffs) > 0:
			msg = res.Diffs[0]
		}
		return core.Errorf(core.CodeRunFailed, "replay of run %s: %s", id, msg).
			WithDetail("diffs", res.Diffs).WithDetail("problems", res.Problems)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &serve.ReplayResult{}, Version: runSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "replay a recorded run", Command: "flo run replay run_01"},
		{Title: "as JSON", Command: "flo run replay run_01 --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the run has no recording", Suggest: "flo run create --flow <flow> --record", Reason: "record a run"},
		{When: "the replay differs", Suggest: "flo run show <run>", Reason: "compare with the run's steps"},
	})
	return c
}

// readJSONObject reads a flag's JSON object file; nil when the flag is unset.
func readJSONObject(flag, path string) (map[string]any, error) {
	if path == "" {
		return nil, nil //nolint:nilnil // no file, no override
	}
	data, err := os.ReadFile(path) //nolint:gosec // the file the caller named
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "%s: %v", flag, err)
	}
	v, err := core.DecodeJSONValue(data)
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "%s %s: not JSON: %v", flag, path, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, core.Errorf(core.CodeUsage, "%s %s: want a JSON object", flag, path)
	}
	return m, nil
}
