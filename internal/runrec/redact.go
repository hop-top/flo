package runrec

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Redacted replaces a value scrubbed out of text.
const Redacted = "[redacted]"

// minLiteral is the shortest value scrubbed out of text by value: a shorter
// one would also take innocent words with it.
const minLiteral = 4

// Redactor drops what node specs list under `redact:` (node.md §9), and the
// values of secrets, from what a recording keeps.
//
// A redact path is dotted keys (`in.token`, `out.items.body`); a leading in,
// inputs, out, outputs or output says which side it is on and is not part of
// the path, and `[]` or `*` segments (list elements) are skipped. A key of a
// JSON document is dropped when the keys down to it end with the path: `token`
// matches `{"token": 1}` and `{"auth": {"token": 1}}`. Text is not parsed
// unless it is a JSON document (a process's stdin and stdout, an HTTP body, a
// file a process wrote); a value known to be secret is replaced wherever it
// occurs in any text.
type Redactor struct {
	paths    [][]string
	literals []string
}

// NewRedactor returns a Redactor for the redact paths of nodes and the secret
// values (values shorter than four bytes are not scrubbed from text).
func NewRedactor(paths []string, literals ...string) *Redactor {
	r := &Redactor{}
	for _, p := range paths {
		if segs := splitPath(p); len(segs) > 0 && !containsPath(r.paths, segs) {
			r.paths = append(r.paths, segs)
		}
	}
	return r.With(literals...)
}

// With returns a Redactor that also scrubs literals.
func (r *Redactor) With(literals ...string) *Redactor {
	out := &Redactor{paths: r.paths, literals: slices.Clone(r.literals)}
	for _, l := range literals {
		if len(l) >= minLiteral && !slices.Contains(out.literals, l) {
			out.literals = append(out.literals, l)
		}
	}
	// longest first, so a value holding another is replaced whole
	slices.SortStableFunc(out.literals, func(a, b string) int { return len(b) - len(a) })
	return out
}

func containsPath(all [][]string, p []string) bool {
	return slices.ContainsFunc(all, func(q []string) bool { return slices.Equal(p, q) })
}

func splitPath(p string) []string {
	var segs []string
	for _, s := range strings.Split(p, ".") {
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "[]")
		s = strings.TrimSuffix(s, "[*]")
		if s == "" || s == "*" {
			continue
		}
		segs = append(segs, s)
	}
	if len(segs) > 1 {
		switch segs[0] {
		case "in", "inputs", "out", "outputs", "output":
			segs = segs[1:]
		}
	}
	if len(segs) == 1 {
		switch segs[0] {
		case "in", "inputs", "out", "outputs", "output":
			return nil // the whole side: not a path of a key
		}
	}
	return segs
}

func (r *Redactor) matches(chain []string) bool {
	for _, p := range r.paths {
		if len(chain) >= len(p) && slices.Equal(chain[len(chain)-len(p):], p) {
			return true
		}
	}
	return false
}

// Doc returns a copy of a JSON-like document (maps, lists, scalars) with the
// redacted keys dropped and the secret values replaced. v is not changed.
func (r *Redactor) Doc(v any) any {
	out, _ := r.walk(v, nil, true)
	return out
}

// Payload returns a generic copy of a cassette payload (a request or a
// response struct): its structure is kept, the JSON documents inside its
// strings are redacted by path, and every string is scrubbed of the secret
// values.
func (r *Redactor) Payload(v any) any {
	b, err := yaml.Marshal(v)
	if err != nil {
		return map[string]any{"redaction_error": "payload could not be copied"}
	}
	var generic any
	if err := yaml.Unmarshal(b, &generic); err != nil {
		return map[string]any{"redaction_error": "payload could not be copied"}
	}
	out, _ := r.walk(generic, nil, false)
	return out
}

// Text scrubs the secret values out of s and redacts it by path when it is a
// JSON document.
func (r *Redactor) Text(s string) string {
	out, _ := r.text(s)
	return out
}

// walk copies v. doc is true inside a JSON document, where keys are matched.
func (r *Redactor) walk(v any, chain []string, doc bool) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		changed := false
		for k, val := range x {
			next := chain
			if doc {
				next = append(slices.Clone(chain), k)
				if r.matches(next) {
					changed = true
					continue
				}
			}
			nv, c := r.walk(val, next, doc)
			out[k] = nv
			changed = changed || c
		}
		return out, changed
	case []any:
		out := make([]any, len(x))
		changed := false
		for i, val := range x {
			nv, c := r.walk(val, chain, doc)
			out[i] = nv
			changed = changed || c
		}
		return out, changed
	case string:
		if doc {
			return r.literal(x)
		}
		return r.text(x)
	}
	return v, false
}

func (r *Redactor) literal(s string) (string, bool) {
	out := s
	for _, l := range r.literals {
		out = strings.ReplaceAll(out, l, Redacted)
	}
	return out, out != s
}

// text redacts a string: as a JSON document when it is one, then by value.
func (r *Redactor) text(s string) (string, bool) {
	changed := false
	if t := strings.TrimSpace(s); len(r.paths) > 0 && (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) {
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		var doc any
		if err := dec.Decode(&doc); err == nil && !dec.More() {
			if red, c := r.walk(doc, nil, true); c {
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				if err := enc.Encode(red); err == nil {
					s, changed = strings.TrimRight(buf.String(), "\n"), true
				}
			}
		}
	}
	out, c := r.literal(s)
	return out, changed || c
}

// ValuesAt returns the strings found at the redacted paths of in: the values
// a node was given that must not show elsewhere (in an argv, a URL, a body).
func (r *Redactor) ValuesAt(in any) []string {
	var out []string
	var walk func(v any, chain []string)
	walk = func(v any, chain []string) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				next := append(slices.Clone(chain), k)
				if r.matches(next) {
					collect(val, &out)
					continue
				}
				walk(val, next)
			}
		case []any:
			for _, val := range x {
				walk(val, chain)
			}
		}
	}
	walk(in, nil)
	return out
}

func collect(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case map[string]any:
		for _, val := range x {
			collect(val, out)
		}
	case []any:
		for _, val := range x {
			collect(val, out)
		}
	}
}
