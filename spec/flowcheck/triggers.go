package flowcheck

import (
	"fmt"
	"slices"
	"strings"

	"hop.top/flo/spec/node"
	"hop.top/flo/spec/run"
)

// triggers is V27: a calendar or event trigger's source is a trigger node,
// its options are inputs of that node, and `when` reads only `trigger`.
func (c *checker) triggers() {
	for i, t := range c.settings.Triggers {
		if t.Kind != run.TriggerCalendar && t.Kind != run.TriggerEvent {
			continue
		}
		field := fmt.Sprintf("on[%d].%s", i, t.Kind)
		c.triggerWhen(field+".when", t.When)
		if t.Source == "" || c.opts.Nodes == nil {
			continue
		}
		e, err := c.opts.Nodes.Locate(t.Source)
		if err != nil {
			code := node.CodeInvalidNode
			if node.IsNotFound(err) {
				code = CodeNodeNotFound
			}
			c.err("V27", code, "", field+".source", "%v", err)
			continue
		}
		n := e.Spec
		if n.EffectiveRole() != node.RoleTrigger {
			c.err("V27", CodeInvalidFlow, "", field+".source", "%s is not a trigger node (role: trigger)", n.Node)
			continue
		}
		keys := make([]string, 0, len(t.Options))
		for k := range t.Options {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			def, ok := n.Inputs[k]
			if !ok {
				c.err("V27", CodeInvalidFlow, "", field+"."+k, "%q is not an option of trigger node %s", k, n.Node)
				continue
			}
			if !hasPlaceholder(t.Options[k]) {
				for _, m := range node.CheckInput(def, t.Options[k]) {
					c.err("V27", CodeInvalidFlow, "", field+"."+k, "%s", m)
				}
			}
		}
		for name, def := range n.Inputs {
			if _, ok := t.Options[name]; !ok && def.Required.Flag && !def.HasDefault() {
				c.err("V27", CodeInvalidFlow, "", field, "required option %q of trigger node %s is missing", name, n.Node)
			}
		}
	}
}

// triggerWhen checks that a trigger's `when` reads only `trigger`.
func (c *checker) triggerWhen(field, when string) {
	if strings.TrimSpace(when) == "" {
		return
	}
	var refs []node.Ref
	var err error
	if strings.Contains(when, "{{") {
		refs, err = node.TemplateRefs(when)
	} else {
		refs, err = node.ExprRefs(when)
	}
	if err != nil {
		c.err("V27", CodeInvalidFlow, "", field, "%v", err)
		return
	}
	for _, root := range node.Roots(refs) {
		if root != "trigger" {
			c.err("V27", CodeInvalidFlow, "", field, "reads %q; a trigger's when reads only trigger", root)
		}
	}
}
