package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/render"
	"hop.top/flo/spec/node"
)

// maxFileBytes bounds an output file read from the step directory.
const maxFileBytes = 16 << 20

// stderrTail is how much of stderr a failure message carries.
const stderrTail = 2048

// planStepDir stands for `run.step_dir` in a plan: no directory is made
// for a call that does not execute.
const planStepDir = "<step_dir>"

// Executor runs declared command nodes (node.md §4).
type Executor struct {
	// Commands runs the process; Runner is the real one.
	Commands core.CommandRunner
	// StepRoot is where per-attempt scratch directories are made; empty is
	// the system temp directory.
	StepRoot string
}

// Request is a command node's rendered call.
type Request struct {
	Argv    []string
	Env     map[string]string
	Cwd     string
	Stdin   []byte
	Profile string
}

// Plan renders the call without running it (dry run): the argv, env keys,
// cwd and profile. Env values are shown: a command node takes no secrets
// through env of its own (node.md §4.2).
func (x *Executor) Plan(inv core.Invocation) (map[string]any, *core.Error) {
	req, err := x.render(inv, planStepDir)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"argv": req.Argv, "profile": req.Profile}
	if len(req.Env) > 0 {
		out["env"] = req.Env
	}
	if req.Cwd != "" {
		out["cwd"] = req.Cwd
	}
	if len(req.Stdin) > 0 {
		out["stdin"] = string(req.Stdin)
	}
	return out, nil
}

func (x *Executor) render(inv core.Invocation, stepDir string) (Request, *core.Error) {
	env, err := render.Shared()
	if err != nil {
		return Request{}, core.Wrap(core.CodeInternal, err)
	}
	spec := inv.Node
	sc := render.Scope{In: inv.In, Config: inv.Config, Run: inv.Run, StepDir: stepDir, Admission: inv.Admission}
	act := sc.Activation()
	var req Request
	if req.Argv, err = env.Argv(spec.Argv, act); err != nil {
		return Request{}, render.Fail("argv", err)
	}
	if req.Env, err = env.StringMap(spec.Env, act); err != nil {
		return Request{}, render.Fail("env", err)
	}
	if inv.IdempotencyKey != "" {
		if req.Env == nil {
			req.Env = map[string]string{}
		}
		req.Env["FLO_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}
	if req.Cwd, err = env.String(spec.Cwd, act); err != nil {
		return Request{}, render.Fail("cwd", err)
	}
	if req.Stdin, err = stdin(env, spec.Stdin, act); err != nil {
		return Request{}, render.Fail("stdin", err)
	}
	req.Profile = inv.RunAs
	return req, nil
}

func stdin(env *render.Env, s *node.Stdin, act map[string]any) ([]byte, error) {
	switch {
	case s == nil:
		return nil, nil
	case s.JSON != nil:
		v, err := env.Value(s.JSON, act)
		if err != nil {
			return nil, err //nolint:wrapcheck // classified by the caller
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("stdin.json: %w", err)
		}
		return b, nil
	case s.Text != "":
		t, err := env.String(s.Text, act)
		if err != nil {
			return nil, err
		}
		return []byte(t), nil
	}
	return nil, nil
}

// Execute runs the node once. A failure of the step is a Result with
// OutcomeFailed; the error return is for faults of the engine.
func (x *Executor) Execute(ctx context.Context, inv core.Invocation) (core.Result, error) {
	root := x.StepRoot
	stepDir, err := os.MkdirTemp(root, "flo-step-")
	if err != nil {
		return core.Result{}, fmt.Errorf("step dir: %w", err)
	}
	defer os.RemoveAll(stepDir) //nolint:errcheck // scratch directory of one attempt

	spec := inv.Node
	req, rerr := x.render(inv, stepDir)
	if rerr != nil {
		return fail(rerr, core.EffectNone, nil), nil
	}
	res, err := x.Commands.Run(ctx, core.CommandSpec{
		Argv: req.Argv, Env: req.Env, Cwd: req.Cwd, Stdin: req.Stdin, Profile: req.Profile,
		Timeout: render.Timeout(inv), MaxOutput: spec.StdoutMax,
	})
	if err != nil {
		e := core.Errorf(core.CodeRunFailed, "cannot start %s: %v", firstOr(req.Argv, "command"), err)
		return fail(e, core.EffectNone, nil), nil
	}
	raw := Raw(res)
	out := core.Result{Raw: raw, Truncated: res.Truncated, Calls: 1}

	switch {
	case ctx.Err() != nil:
		e := core.Errorf(core.CodeRunCancelled, "%s was cancelled; partial output kept", firstOr(req.Argv, "command"))
		return failWith(out, e, core.EffectUnknown), nil
	case res.TimedOut:
		e := core.Errorf(core.CodeTimeout, "%s did not finish in %s; partial output kept", firstOr(req.Argv, "command"), render.Timeout(inv))
		return failWith(out, e, core.EffectUnknown), nil
	case slices.Contains(retryCodes(spec), res.ExitCode):
		e := core.Errorf(core.CodeRunFailed, "%s exited %d (retry later)", firstOr(req.Argv, "command"), res.ExitCode).
			WithTransience(core.TransienceTransient).WithDetail("exit_code", res.ExitCode)
		return failWith(out, e, core.EffectNone), nil
	case !slices.Contains(okCodes(spec), res.ExitCode):
		e := core.Errorf(core.CodeRunFailed, "%s exited %d: %s", firstOr(req.Argv, "command"), res.ExitCode, tail(res.Stderr)).
			WithDetail("exit_code", res.ExitCode)
		return failWith(out, e, core.EffectDone), nil
	}

	parsed, perr := x.parse(spec, res, raw, stepDir, inv)
	if perr != nil {
		return failWith(out, perr, core.EffectDone), nil
	}
	env, _ := render.Shared()
	value, failed, rerr := env.Reshape(spec.Outputs, parsed, render.Scope{In: inv.In, Config: inv.Config, Run: inv.Run, StepDir: stepDir, Admission: inv.Admission})
	if rerr != nil {
		return failWith(out, rerr, core.EffectDone), nil
	}
	out.Output = value
	if failed {
		e := core.Errorf(core.CodeNodeReportedFailure, "%s reported a failure (outputs.fail_when)", spec.Node)
		return failWith(out, e, core.EffectDone), nil
	}
	out.Outcome = core.OutcomeOK
	out.Effect = core.EffectDone
	return out, nil
}

func okCodes(s *node.Spec) []int {
	if s.ExitCodes != nil && len(s.ExitCodes.OK) > 0 {
		return s.ExitCodes.OK
	}
	return []int{0}
}

func retryCodes(s *node.Spec) []int {
	if s.ExitCodes == nil {
		return nil
	}
	return s.ExitCodes.Retry
}

// Raw is the raw exec result kept in a step record: exit_code, stdout, stderr,
// duration_ms and truncated (flow.md §5.3), plus timed_out when the process
// was killed at its timeout.
func Raw(res core.CommandResult) map[string]any {
	raw := map[string]any{
		"exit_code":   int64(res.ExitCode),
		"stdout":      string(res.Stdout),
		"stderr":      string(res.Stderr),
		"duration_ms": res.Duration.Milliseconds(),
		"truncated":   res.Truncated,
	}
	if res.TimedOut {
		raw["timed_out"] = true
	}
	return raw
}

// parse reads the value the node's outputs.from names (node.md §4.3).
func (x *Executor) parse(spec *node.Spec, res core.CommandResult, raw map[string]any, stepDir string, inv core.Invocation) (any, *core.Error) {
	from := spec.Outputs.From
	if from == "" {
		from = "stdout"
	}
	var data []byte
	switch {
	case from == "stdout":
		data = res.Stdout
	case from == "stderr":
		data = res.Stderr
	case from == "exit_code":
		return int64(res.ExitCode), nil
	case from == "exec":
		return raw, nil
	case strings.HasPrefix(from, "file:"):
		b, err := x.readFile(strings.TrimPrefix(from, "file:"), stepDir, inv)
		if err != nil {
			return nil, err
		}
		data = b
	default:
		return nil, core.Errorf(core.CodeInvalidNode, "%s: outputs.from %q is not stdout, stderr, exit_code, exec or file:<path>", spec.Node, from)
	}
	return decode(data, spec.Outputs.Format, spec.Node)
}

func (x *Executor) readFile(path, stepDir string, inv core.Invocation) ([]byte, *core.Error) {
	env, err := render.Shared()
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	rendered, err := env.String(path, render.Scope{In: inv.In, Config: inv.Config, Run: inv.Run, StepDir: stepDir, Admission: inv.Admission}.Activation())
	if err != nil {
		return nil, render.Fail("outputs.from", err)
	}
	rel := rendered
	if filepath.IsAbs(rel) {
		r, rerr := filepath.Rel(stepDir, rel)
		if rerr != nil {
			return nil, core.Errorf(core.CodeOutputInvalid, "output file %q is outside the step directory", rendered)
		}
		rel = r
	}
	if !filepath.IsLocal(rel) {
		return nil, core.Errorf(core.CodeOutputInvalid, "output file %q is outside the step directory", rendered)
	}
	f, err := os.Open(filepath.Join(stepDir, rel)) //nolint:gosec // confined to the step directory above
	if err != nil {
		return nil, core.Errorf(core.CodeOutputInvalid, "output file %q was not written: %v", rel, err)
	}
	defer f.Close() //nolint:errcheck // read only
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(f, maxFileBytes+1)); err != nil {
		return nil, core.Errorf(core.CodeOutputInvalid, "output file %q: %v", rel, err)
	}
	if buf.Len() > maxFileBytes {
		return nil, core.Errorf(core.CodeOutputInvalid, "output file %q is over %d bytes", rel, maxFileBytes)
	}
	return buf.Bytes(), nil
}

func decode(data []byte, format, nodeType string) (any, *core.Error) {
	switch format {
	case "json":
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, core.Errorf(core.CodeOutputInvalid, "%s: output is not one JSON document: %v", nodeType, err)
		}
		return v, nil
	case "lines":
		lines := []any{}
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		return lines, nil
	case "text", "":
		return string(data), nil
	}
	return nil, core.Errorf(core.CodeInvalidNode, "%s: outputs.format %q is not json, lines or text", nodeType, format)
}

func fail(e *core.Error, eff core.Effect, raw any) core.Result {
	return core.Result{Outcome: core.OutcomeFailed, Err: e, Effect: eff, Raw: raw}
}

func failWith(r core.Result, e *core.Error, eff core.Effect) core.Result {
	r.Outcome, r.Err, r.Effect = core.OutcomeFailed, e, eff
	return r
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > stderrTail {
		s = "..." + s[len(s)-stderrTail:]
	}
	if s == "" {
		return "(no stderr)"
	}
	return s
}

func firstOr(s []string, d string) string {
	if len(s) > 0 {
		return s[0]
	}
	return d
}
