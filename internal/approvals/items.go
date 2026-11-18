package approvals

import (
	"fmt"
	"slices"

	"hop.top/flo/internal/access/digest"
	"hop.top/flo/internal/core"
)

// itemFields are the keys of an ApprovalItem that are not Extra.
var itemFields = []string{"id", "action", "why", "class", "capability", "args", "digest"}

// parseItems turns the `items` input into approval items: the known keys
// into fields, every other key into Extra so it travels to the provider and
// the output. Ids are required and unique.
func parseItems(v any) ([]core.ApprovalItem, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, core.Errorf(core.CodeInputInvalid, "items: want a list, got %T", v)
	}
	if len(list) == 0 {
		return nil, core.New(core.CodeInputInvalid, "items: the list is empty")
	}
	out := make([]core.ApprovalItem, 0, len(list))
	seen := map[string]bool{}
	for i, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, core.Errorf(core.CodeInputInvalid, "items[%d]: want an object, got %T", i, raw)
		}
		it := core.ApprovalItem{}
		id, _ := m["id"].(string)
		if id == "" {
			return nil, core.Errorf(core.CodeInputInvalid, "items[%d]: id is required", i)
		}
		if seen[id] {
			return nil, core.Errorf(core.CodeInputInvalid, "items[%d]: id %q is repeated", i, id)
		}
		seen[id] = true
		it.ID = id
		it.Action, _ = m["action"].(string)
		it.Why, _ = m["why"].(string)
		it.Capability, _ = m["capability"].(string)
		it.Args = m["args"]
		it.Digest, _ = m["digest"].(string)
		var err error
		if it.Class, err = parseClass(m["class"]); err != nil {
			return nil, core.Errorf(core.CodeInputInvalid, "items[%d] %q: %w", i, id, err)
		}
		for k, x := range m {
			if slices.Contains(itemFields, k) {
				continue
			}
			if it.Extra == nil {
				it.Extra = map[string]any{}
			}
			it.Extra[k] = x
		}
		out = append(out, it)
	}
	return out, nil
}

// parseClass reads a class: a string, or a list of strings.
func parseClass(v any) ([]string, error) {
	switch c := v.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []string{c}, nil
	case []any:
		out := make([]string, 0, len(c))
		for _, x := range c {
			s, ok := x.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("class: want strings, got %v", x)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("class: want a string or a list of strings, got %T", v)
}

// itemDigest sets the request digest of an item that names a capability
// (operation is the item's capability, input its args, scope
// the tenant, actor the run_as, acting_for the principal). An item without
// a capability has no exact operation and no digest.
func itemDigest(tenant core.TenantID, actor, principal string, it *core.ApprovalItem) error {
	if it.Capability == "" || it.Digest != "" {
		return nil
	}
	r, err := digest.ForRun(tenant, digest.Operation(it.Capability, ""), actor, principal, it.Args)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	d, err := r.Digest()
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	it.Digest = d
	return nil
}

// itemValue is an item as the output and the trust provider see it: the
// extra fields, then the item's own.
func itemValue(it core.ApprovalItem) map[string]any {
	m := make(map[string]any, len(it.Extra)+7)
	for k, v := range it.Extra {
		m[k] = v
	}
	m["id"] = it.ID
	if it.Action != "" {
		m["action"] = it.Action
	}
	if it.Why != "" {
		m["why"] = it.Why
	}
	if len(it.Class) > 0 {
		class := make([]any, len(it.Class))
		for i, c := range it.Class {
			class[i] = c
		}
		m["class"] = class
	}
	if it.Capability != "" {
		m["capability"] = it.Capability
	}
	if it.Args != nil {
		m["args"] = it.Args
	}
	if it.Digest != "" {
		m["digest"] = it.Digest
	}
	return m
}

// output builds the node's output (node.md §6.6): an item is in exactly one
// list, by its decision; corrected items carry the person's replacement as
// `correction`; `reason` maps an item id to the reason of its decision.
func output(a core.Approval) map[string]any {
	approved, rejected, corrected, auto := []any{}, []any{}, []any{}, []any{}
	reason := map[string]any{}
	for _, it := range a.Items {
		v := itemValue(it)
		d := it.Decision
		if d == nil {
			// not reachable for a complete approval; an undecided item is not approved
			rejected = append(rejected, v)
			continue
		}
		if d.Reason != "" {
			reason[it.ID] = d.Reason
		}
		switch d.Verdict {
		case core.VerdictApprove:
			approved = append(approved, v)
		case core.VerdictCorrect:
			v["correction"] = d.Correction
			corrected = append(corrected, v)
		case core.VerdictAuto:
			auto = append(auto, v)
		case core.VerdictReject:
			rejected = append(rejected, v)
		default:
			rejected = append(rejected, v)
		}
	}
	return map[string]any{"approved": approved, "rejected": rejected, "corrected": corrected, "auto": auto, "reason": reason}
}
