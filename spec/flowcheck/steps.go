package flowcheck

import (
	"slices"
	"strings"

	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
	"hop.top/flo/spec/run"
)

// step applies the per-step rules.
func (c *checker) step(st *spec.Step) {
	switch {
	case !st.IsUses():
		if st.EffectiveKind() == spec.KindAgent && !c.opts.AllowAgent {
			c.err("D5", CodeEngineFeature, st.ID, "kind", "an agent step is not run by the phase-1 engine")
		}
	case st.IsNode():
		c.nodeStep(st)
	default:
		c.subFlowStep(st)
	}
}

func (c *checker) nodeStep(st *spec.Step) {
	if c.opts.Nodes == nil {
		return
	}
	e, err := c.opts.Nodes.Locate(st.Uses)
	if err != nil {
		code := node.CodeInvalidNode
		if node.IsNotFound(err) {
			code = CodeNodeNotFound
		}
		c.err("V12", code, st.ID, "uses", "%v", err)
		return
	}
	n := e.Spec
	if n.EffectiveRole() == node.RoleTrigger {
		c.err("V12", CodeInvalidFlow, st.ID, "uses", "%s is a trigger node; it feeds on:, not a step", n.Node)
		return
	}
	c.withInputs(st, n)
	c.privacy(st, n)
	c.guard(st, n)
	c.retry(st, n)
	c.runAs(st, n)
}

// withInputs is V11 for a node step: every with key is an input, required
// inputs are present, and literal values have the input's type.
func (c *checker) withInputs(st *spec.Step, n *node.Spec) {
	keys := sortedKeys(st.With)
	for _, k := range keys {
		def, ok := n.Inputs[k]
		if !ok {
			c.err("V11", CodeInvalidFlow, st.ID, "with."+k, "%q is not an input of %s", k, n.Node)
			continue
		}
		if msgs := literalErrors(def, st.With[k]); len(msgs) > 0 {
			for _, m := range msgs {
				c.err("V11", CodeInvalidFlow, st.ID, "with."+k, "%s", m)
			}
		}
	}
	names := make([]string, 0, len(n.Inputs))
	for name := range n.Inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		def := n.Inputs[name]
		if _, given := st.With[name]; !given && def.Required.Flag && !def.HasDefault() {
			c.err("V11", CodeInvalidFlow, st.ID, "with", "required input %q of %s is missing", name, n.Node)
		}
	}
}

// literalErrors checks a with value whose type is known: a literal, or a
// string with text around a placeholder (which renders to a string). A
// whole placeholder, or a collection holding one, has a type only the
// engine knows.
func literalErrors(def node.InputDef, v any) []string {
	if hasPlaceholder(v) {
		if s, ok := v.(string); ok && !wholePlaceholder(s) {
			return node.CheckInput(node.InputDef{Type: def.Type}, "")
		}
		return nil
	}
	return node.CheckInput(def, v)
}

func hasPlaceholder(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, "{{")
	case []any:
		return slices.ContainsFunc(t, hasPlaceholder)
	case map[string]any:
		for _, e := range t {
			if hasPlaceholder(e) {
				return true
			}
		}
	}
	return false
}

func wholePlaceholder(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{{") || !strings.HasSuffix(t, "}}") {
		return false
	}
	// One placeholder only: no other "{{" inside.
	return strings.Count(t, "{{") == 1
}

// privacy is V19: no cloud node under a local-only ceiling. For a node that
// takes a `model`, a literal model URI decides the level (node.md §9).
func (c *checker) privacy(st *spec.Step, n *node.Spec) {
	eff := n.Privacy
	how := "declared " + string(n.Privacy)
	if _, takesModel := n.Inputs["model"]; takesModel {
		if m, ok := st.With["model"].(string); ok && !strings.Contains(m, "{{") {
			if p, ok := node.ModelPrivacy(m); ok {
				eff, how = p, "its model "+m+" is "+string(p)
				if node.AmbiguousOllama(m) {
					how += "; " + node.LocalOllamaHint
				}
			}
		}
	}
	if ceil := c.ceiling(st); eff == node.PrivacyCloud && ceil == run.PrivacyLocalOnly {
		c.err("V19", CodePrivacyViolation, st.ID, "privacy", "%s is cloud (%s) under a local-only ceiling", n.Node, how)
	}
}

// guard is V20: an irreversible step has an ancestor flo/approval@1 step,
// or a trust provider named on the step for a node that declares a trust
// class. A core-kind step is never accepted as guarded by trust, and an
// exec step is never an approval, so neither can guard.
func (c *checker) guard(st *spec.Step, n *node.Spec) {
	if n.SideEffect != node.SideEffectIrreversible {
		return
	}
	for _, a := range c.ancestors(st) {
		if t, err := node.ParseType(a.Uses); err == nil && t.Ref() == "flo/approval" {
			return
		}
	}
	if n.Approval == node.ApprovalTrust && n.Trust != nil && n.Trust.Class != "" {
		if prov, ok := st.With["trust"].(string); ok && prov != "" && !strings.Contains(prov, "{{") {
			if c.opts.Nodes != nil {
				if _, err := c.opts.Nodes.Trust(prov); err == nil {
					return
				}
			}
			c.err("V20", CodeUnguardedIrreversible, st.ID, "with.trust", "trust provider %q does not resolve", prov)
			return
		}
	}
	c.err("V20", CodeUnguardedIrreversible, st.ID, "uses",
		"%s is irreversible: it needs an ancestor flo/approval@1 step, or a trust provider on the step", n.Node)
}

// retry is V22 (warning): more than one attempt on a node that is neither
// idempotent nor keyed; the engine clamps to one.
func (c *checker) retry(st *spec.Step, n *node.Spec) {
	if st.Retry == nil || st.Retry.MaxAttempts <= 1 {
		return
	}
	if !n.EffectiveIdempotent() && n.IdempotencyKey == "" {
		c.add(SeverityWarning, "V22", CodeInvalidFlow, st.ID, "retry.max_attempts",
			"%s is not idempotent and has no idempotency key; the engine clamps max_attempts %d to 1", n.Node, st.Retry.MaxAttempts)
	}
}

// runAs is the run_as half of V25: a step's profile may only hold, of the
// capabilities the step needs, ones the flow's profile holds too.
func (c *checker) runAs(st *spec.Step, n *node.Spec) {
	if c.opts.Grants == nil || st.RunAs == "" || c.f.RunAs == "" || st.RunAs == c.f.RunAs {
		return
	}
	stepCaps, ok1 := c.opts.Grants.Capabilities(st.RunAs)
	flowCaps, ok2 := c.opts.Grants.Capabilities(c.f.RunAs)
	if !ok1 || !ok2 {
		return
	}
	for _, cp := range n.Capabilities {
		if slices.Contains(stepCaps, cp) && !slices.Contains(flowCaps, cp) {
			c.err("V25", CodeGrantDenied, st.ID, "run_as",
				"run_as %q holds %s, which the flow's run_as %q does not; a step may only narrow", st.RunAs, cp, c.f.RunAs)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
