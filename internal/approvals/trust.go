package approvals

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
)

// DefaultProviderTimeout bounds a provider call that sets no timeout.
const DefaultProviderTimeout = 30 * time.Second

// CommandTrust is core.Trust over trust providers of kind command
// (node.md §10.3): the provider's tool owns the counters, the engine only
// reads levels, reports decisions and asks for promotions.
type CommandTrust struct {
	run core.CommandRunner
	loc Locator
	env *cel.Env
}

var _ core.Trust = (*CommandTrust)(nil)

// NewCommandTrust returns the trust port over run, finding provider
// documents with loc.
func NewCommandTrust(run core.CommandRunner, loc Locator) (*CommandTrust, error) {
	env, err := cel.NewEnv()
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	return &CommandTrust{run: run, loc: loc, env: env}, nil
}

// Levels implements core.Trust: runs `read` and returns the levels it
// prints, a JSON list of {class, level, streak, threshold, resets,
// last_reset}. A level other than ask or auto is output_invalid.
func (t *CommandTrust) Levels(ctx context.Context, tenant core.TenantID, provider string, params map[string]any) ([]core.TrustLevel, error) {
	p, err := t.loc.Find(ctx, tenant, provider)
	if err != nil {
		return nil, err //nolint:wrapcheck // *core.Error
	}
	if p.Read == nil {
		return nil, core.Errorf(core.CodeInvalidNode, "trust provider %s declares no read", provider)
	}
	res, err := t.call(ctx, p, "read", p.Read, map[string]any{"context": params})
	if err != nil {
		return nil, err
	}
	var levels []core.TrustLevel
	if err := json.Unmarshal(res.Stdout, &levels); err != nil {
		return nil, core.Errorf(core.CodeOutputInvalid, "trust provider %s read: want a JSON list of levels: %w", provider, err)
	}
	for _, l := range levels {
		if l.Class == "" || (l.Level != core.TrustAsk && l.Level != core.TrustAuto) {
			return nil, core.Errorf(core.CodeOutputInvalid, "trust provider %s read: level %q of class %q is not ask or auto", provider, l.Level, l.Class)
		}
	}
	return levels, nil
}

// Decide implements core.Trust: runs `decide` with in.decisions and
// in.context, and returns the {applied, skipped} it prints.
func (t *CommandTrust) Decide(ctx context.Context, tenant core.TenantID, provider string, params map[string]any, decisions []core.TrustDecision) (core.TrustApplied, error) {
	p, err := t.loc.Find(ctx, tenant, provider)
	if err != nil {
		return core.TrustApplied{}, err //nolint:wrapcheck // *core.Error
	}
	if p.Decide == nil {
		return core.TrustApplied{}, core.Errorf(core.CodeInvalidNode, "trust provider %s declares no decide", provider)
	}
	res, err := t.call(ctx, p, "decide", p.Decide, map[string]any{"context": params, "decisions": decisionsValue(decisions)})
	if err != nil {
		return core.TrustApplied{}, err
	}
	var applied core.TrustApplied
	if err := json.Unmarshal(res.Stdout, &applied); err != nil {
		return core.TrustApplied{}, core.Errorf(core.CodeOutputInvalid, "trust provider %s decide: want {applied, skipped}: %w", provider, err)
	}
	return applied, nil
}

// Set implements core.Trust: runs `set` with in.class, in.level and
// in.reason. A provider refuses a level by exiting non-zero and printing
// {"code": "trust_capped", "message": "..."}; that is trust_capped.
func (t *CommandTrust) Set(ctx context.Context, tenant core.TenantID, provider, class string, level core.TrustLevelName, reason string) error {
	p, err := t.loc.Find(ctx, tenant, provider)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if p.Set == nil {
		return core.Errorf(core.CodeInvalidNode, "trust provider %s declares no set", provider)
	}
	_, err = t.call(ctx, p, "set", p.Set, map[string]any{"class": class, "level": string(level), "reason": reason})
	return err
}

// decisionsValue is in.decisions: [{id, verdict, reason?, item}].
func decisionsValue(ds []core.TrustDecision) []any {
	out := make([]any, len(ds))
	for i, d := range ds {
		m := map[string]any{"id": d.ID, "verdict": string(d.Verdict)}
		if d.Reason != "" {
			m["reason"] = d.Reason
		}
		if d.Item != nil {
			m["item"] = d.Item
		}
		out[i] = m
	}
	return out
}

// call renders the action over `in` and runs it. The provider is not run
// under an aps profile: it is the engine's own tool, not a flow step.
func (t *CommandTrust) call(ctx context.Context, p Provider, what string, a *Action, in map[string]any) (core.CommandResult, error) {
	norm, err := core.NormalizeJSON(in)
	if err != nil {
		return core.CommandResult{}, core.Wrap(core.CodeInternal, err)
	}
	act := cel.Activation{inNamespace: norm, "config": map[string]any{}, "run": map[string]any{}}
	argv, err := t.renderArgv(a.Argv, act)
	if err != nil {
		return core.CommandResult{}, fmt.Errorf("trust provider %s %s: %w", p.Trust, what, err)
	}
	cs := core.CommandSpec{Argv: argv, Env: mergeEnv(p.Env, a.Env), Cwd: firstNonEmpty(a.Cwd, p.Cwd), Timeout: DefaultProviderTimeout}
	if p.Timeout != "" {
		d, err := spec.ParseDuration(p.Timeout)
		if err != nil {
			return core.CommandResult{}, core.Errorf(core.CodeInvalidNode, "trust provider %s: timeout: %w", p.Trust, err)
		}
		cs.Timeout = d
	}
	if a.Stdin != nil {
		if cs.Stdin, err = t.renderStdin(a.Stdin, act); err != nil {
			return core.CommandResult{}, fmt.Errorf("trust provider %s %s: stdin: %w", p.Trust, what, err)
		}
	}
	res, err := t.run.Run(ctx, cs)
	if err != nil {
		return res, core.Errorf(core.CodeEngineUnreachable, "trust provider %s %s: %w", p.Trust, what, err)
	}
	if res.TimedOut {
		return res, core.Errorf(core.CodeTimeout, "trust provider %s %s timed out after %s", p.Trust, what, cs.Timeout)
	}
	if res.ExitCode != 0 {
		var refusal struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(res.Stdout, &refusal) == nil && refusal.Code == string(core.CodeTrustCapped) {
			return res, core.Errorf(core.CodeTrustCapped, "trust provider %s refuses: %s", p.Trust, refusal.Message)
		}
		return res, core.Errorf(core.CodeNodeReportedFailure, "trust provider %s %s exited %d: %s", p.Trust, what, res.ExitCode, trim(res.Stderr)).
			WithDetail("provider", p.Trust).WithDetail("exit_code", res.ExitCode)
	}
	return res, nil
}

// renderArgv renders each element; a whole-element placeholder that is a
// list expands into several arguments (node.md §4.1), a null is "".
func (t *CommandTrust) renderArgv(argv []string, act cel.Activation) ([]string, error) {
	var out []string
	for _, el := range argv {
		v, err := t.env.RenderValue(aliasIn(el), act)
		if err != nil {
			return nil, err //nolint:wrapcheck // *cel.Error
		}
		if list, ok := v.([]any); ok {
			for _, x := range list {
				out = append(out, scalarString(x))
			}
			continue
		}
		out = append(out, scalarString(v))
	}
	return out, nil
}

func (t *CommandTrust) renderStdin(s *Stdin, act cel.Activation) ([]byte, error) {
	if s.JSON != nil {
		v, err := t.env.RenderValue(aliasInValue(s.JSON), act)
		if err != nil {
			return nil, err //nolint:wrapcheck // *cel.Error
		}
		return json.Marshal(v) //nolint:wrapcheck // plain values
	}
	v, err := t.env.RenderValue(aliasIn(s.Text), act)
	if err != nil {
		return nil, err //nolint:wrapcheck // *cel.Error
	}
	return []byte(scalarString(v)), nil
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func mergeEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func trim(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[:max]
	}
	return string(b)
}
