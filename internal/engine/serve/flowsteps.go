package serve

import (
	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
)

// flowStep is a step of a flow that runs a node (Spec) or a core exec.
type flowStep struct {
	ID   string
	Spec *node.Spec // the node's spec, for a node step
	Exec bool       // a core exec step
}

// flowSteps lists the node and exec steps of a flow version, sub-flows
// expanded, in document order. A node that does not resolve is left out: the
// flow's validation reports it.
func (e *Engine) flowSteps(v core.FlowVersion, f *spec.Flow) ([]flowStep, error) {
	loc, err := e.Nodes.Locator(v.Bundle)
	if err != nil {
		return nil, err
	}
	steps := f.Steps
	if v.Bundle != "" {
		if exp, err := spec.Expand(f, &spec.DirLocator{Dirs: []string{v.Bundle}}); err == nil {
			steps = exp
		}
	}
	var out []flowStep
	var walk func([]spec.Step)
	walk = func(ss []spec.Step) {
		for i := range ss {
			switch {
			case ss[i].IsNode():
				if ent, err := loc.Locate(ss[i].Uses); err == nil {
					out = append(out, flowStep{ID: ss[i].ID, Spec: ent.Spec})
				}
			case ss[i].EffectiveKind() == spec.KindExec:
				out = append(out, flowStep{ID: ss[i].ID, Exec: true})
			}
			walk(ss[i].Steps)
		}
	}
	walk(steps)
	return out, nil
}

// redactPaths is the union of the redact paths of the nodes of a flow.
func (e *Engine) redactPaths(v core.FlowVersion, f *spec.Flow) ([]string, error) {
	uses, err := e.flowSteps(v, f)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range uses {
		if u.Spec != nil {
			out = append(out, u.Spec.Redact...)
		}
	}
	return out, nil
}
