// Package provision applies a bos access set (bos.access/v1) to flo's grants.
// bos pushes the operations each bound principal may run; flo writes them into
// its own grants as provisioned capabilities (capability = operation name),
// denies what it cannot represent, and stops covering when the set it holds is
// older than its staleness limit.
package provision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ContentDigest is the digest of the content of a set, as the contract defines
// it: "sha256:" and the hex SHA-256 of the canonical JSON (keys sorted,
// compact, no HTML escaping) of {audiences, grants, operations, principals,
// restricted_types, scopes}, every list sorted by id (the members of an
// audience by principal, restricted_types and excluded_fields by value), null
// valid_until, direction and excluded_fields left out of a grant, null
// valid_until left out of an operation grant and of a membership, and
// operations only when there are some. The digest of a delta is the one of
// the result.
func ContentDigest(content map[string]any) string {
	c := map[string]any{
		"audiences":        sorted(list(content["audiences"]), "id", audience),
		"grants":           sorted(list(content["grants"]), "id", grant),
		"principals":       sorted(list(content["principals"]), "id", nil),
		"restricted_types": sortedStrings(list(content["restricted_types"])),
		"scopes":           sorted(list(content["scopes"]), "id", nil),
	}
	if ops := list(content["operations"]); len(ops) > 0 {
		c["operations"] = sorted(ops, "id", dropNull("valid_until"))
	}
	canon := canonical(c)
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func list(v any) []any {
	l, _ := v.([]any)
	if l == nil {
		return []any{}
	}
	return l
}

// sorted returns the items copied, each shaped by shape, ordered by key.
func sorted(items []any, key string, shape func(map[string]any) map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		if shape != nil {
			cp = shape(cp)
		}
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)[key].(string)
		b, _ := out[j].(map[string]any)[key].(string)
		return a < b
	})
	return out
}

func sortedStrings(items []any) []any {
	out := make([]any, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := out[i].(string)
		b, _ := out[j].(string)
		return a < b
	})
	return out
}

func dropNull(keys ...string) func(map[string]any) map[string]any {
	return func(m map[string]any) map[string]any {
		for _, k := range keys {
			if m[k] == nil {
				delete(m, k)
			}
		}
		return m
	}
}

// grant leaves out the null (and empty) optional members and orders the
// excluded fields.
func grant(m map[string]any) map[string]any {
	m = dropNull("valid_until", "direction", "excluded_fields")(m)
	if ex, ok := m["excluded_fields"].([]any); ok {
		if len(ex) == 0 {
			delete(m, "excluded_fields")
		} else {
			m["excluded_fields"] = sortedStrings(ex)
		}
	}
	if d, ok := m["direction"].(string); ok && d == "" {
		delete(m, "direction")
	}
	return m
}

// audience orders the members and drops a null membership end.
func audience(m map[string]any) map[string]any {
	m["members"] = sorted(list(m["members"]), "principal", dropNull("valid_until"))
	return m
}

// canonical renders v with sorted keys and no insignificant white space.
func canonical(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // maps and slices of JSON values always encode
	return bytes.TrimRight(b.Bytes(), "\n")
}
