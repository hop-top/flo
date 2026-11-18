package flowcheck

import (
	"slices"
	"strings"

	"hop.top/flo/spec"
	"hop.top/flo/spec/run"
)

// subFlowStep checks a `uses: name@semver` step: the reference is pinned
// (V12), phase 1 refuses it (D5) unless allowed, the child resolves, its
// vars take the with (V11), no loop or excess depth (V12), and it stays
// within the parent (V24).
func (c *checker) subFlowStep(st *spec.Step) {
	name, ver, ok := parseSubRef(st.Uses)
	if !ok {
		c.err("V12", CodeInvalidFlow, st.ID, "uses", "sub-flow reference %q must be name@semver: an exact version is required", st.Uses)
	}
	if !c.opts.AllowSubFlow {
		c.err("D5", CodeEngineFeature, st.ID, "uses", "a sub-flow step is not run by the phase-1 engine")
	}
	if !ok || c.opts.Flows == nil {
		return
	}
	child, err := c.opts.Flows.Locate(name + "@" + ver)
	if err != nil {
		code := CodeInvalidFlow
		if spec.IsFlowNotFound(err) {
			code = CodeFlowNotFound
		}
		c.err("V12", code, st.ID, "uses", "%v", err)
		return
	}
	c.withVars(st, child)
	c.descend(st.ID, child, []string{c.f.Key()})
	c.within(st, child)
}

// withVars is V11 for a sub-flow step.
func (c *checker) withVars(st *spec.Step, child *spec.Flow) {
	for _, k := range sortedKeys(st.With) {
		def, ok := child.Vars[k]
		if !ok {
			c.err("V11", CodeInvalidFlow, st.ID, "with."+k, "%q is not a var of sub-flow %s", k, child.Key())
			continue
		}
		if v := st.With[k]; !hasPlaceholder(v) && def.Type != "" {
			if err := run.CheckVarType(def.Type, v); err != nil {
				c.err("V11", CodeInvalidFlow, st.ID, "with."+k, "%v", err)
			}
		}
	}
	names := make([]string, 0, len(child.Vars))
	for n := range child.Vars {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if _, given := st.With[n]; !given && child.Vars[n].Required && child.Vars[n].Default == nil {
			c.err("V11", CodeInvalidFlow, st.ID, "with", "required var %q of sub-flow %s is missing", n, child.Key())
		}
	}
}

// descend follows the sub-flows a child runs, refusing a flow reachable
// from itself and nesting beyond MaxSubFlowDepth.
func (c *checker) descend(stepID string, flow *spec.Flow, stack []string) {
	key := flow.Key()
	if slices.Contains(stack, key) {
		c.err("V12", CodeCycle, stepID, "uses", "sub-flow loop: %s", strings.Join(append(slices.Clone(stack), key), " -> "))
		return
	}
	if len(stack) >= MaxSubFlowDepth {
		c.err("V12", CodeInvalidFlow, stepID, "uses", "sub-flows nest deeper than %d: %s", MaxSubFlowDepth, strings.Join(append(slices.Clone(stack), key), " -> "))
		return
	}
	next := append(slices.Clone(stack), key)
	for _, st := range walk(flow.Steps, nil) {
		if !st.IsUses() || st.IsNode() {
			continue
		}
		name, ver, ok := parseSubRef(st.Uses)
		if !ok {
			continue
		}
		child, err := c.opts.Flows.Locate(name + "@" + ver)
		if err != nil {
			continue // reported where the step is checked
		}
		c.descend(stepID, child, next)
	}
}

// within is V24: the child's privacy ceiling is no wider than the parent's
// in force at the step, and its nodes' capabilities are held by the
// profile the parent runs as.
func (c *checker) within(st *spec.Step, child *spec.Flow) {
	cs, err := run.Decode(child)
	if err == nil {
		if ceil := c.ceiling(st); !cs.Privacy.Narrows(ceil) {
			c.err("V24", CodePrivacyViolation, st.ID, "uses", "sub-flow %s is %s inside a %s parent", child.Key(), cs.Privacy, ceil)
		}
	}
	profile := st.RunAs
	if profile == "" {
		profile = c.f.RunAs
	}
	if c.opts.Grants == nil || c.opts.Nodes == nil || profile == "" {
		return
	}
	held, ok := c.opts.Grants.Capabilities(profile)
	if !ok {
		return
	}
	for _, cp := range c.capabilitiesOf(child, nil) {
		if !slices.Contains(held, cp) {
			c.err("V24", CodeGrantDenied, st.ID, "uses", "sub-flow %s needs %s, which the parent's run_as %q does not hold", child.Key(), cp, profile)
		}
	}
}

// capabilitiesOf unions the capabilities of every node a flow uses,
// following its sub-flows once each. Sorted.
func (c *checker) capabilitiesOf(flow *spec.Flow, seen map[string]bool) []string {
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[flow.Key()] {
		return nil
	}
	seen[flow.Key()] = true
	set := map[string]bool{}
	for _, st := range walk(flow.Steps, nil) {
		switch {
		case st.IsNode():
			if e, err := c.opts.Nodes.Locate(st.Uses); err == nil {
				for _, cp := range e.Spec.Capabilities {
					set[cp] = true
				}
			}
		case st.IsUses() && c.opts.Flows != nil:
			if name, ver, ok := parseSubRef(st.Uses); ok {
				if child, err := c.opts.Flows.Locate(name + "@" + ver); err == nil {
					for _, cp := range c.capabilitiesOf(child, seen) {
						set[cp] = true
					}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for cp := range set {
		out = append(out, cp)
	}
	slices.Sort(out)
	return out
}
