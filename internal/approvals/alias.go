package approvals

import "strings"

// inNamespace is where the provider's `in` is bound while its templates
// render. `in` is a reserved word of CEL (the membership operator), so
// `in.context.run` does not parse as written, and the spec module's
// environment declares no other name for it. Every placeholder is
// rewritten to read this declared namespace instead; it is otherwise
// unused by a provider.
const inNamespace = "result"

// aliasIn rewrites, inside the {{ }} placeholders of s, an `in` that starts
// a selection (`in.x`) to inNamespace. Text outside placeholders, string
// literals, `x in y` and `.in` are left alone.
func aliasIn(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	var b strings.Builder
	for {
		open := strings.Index(s, "{{")
		if open < 0 {
			b.WriteString(s)
			return b.String()
		}
		closeAt := strings.Index(s[open:], "}}")
		if closeAt < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:open+2])
		b.WriteString(aliasExpr(s[open+2 : open+closeAt]))
		b.WriteString("}}")
		s = s[open+closeAt+2:]
	}
}

// aliasInValue applies aliasIn to every string of a decoded YAML/JSON value.
func aliasInValue(v any) any {
	switch t := v.(type) {
	case string:
		return aliasIn(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = aliasInValue(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = aliasInValue(x)
		}
		return out
	}
	return v
}

func aliasExpr(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\'' || c == '"' {
			j := i + 1
			for j < len(s) && s[j] != c {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:min(j, len(s))])
			i = j
			continue
		}
		if isWord(c) {
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			word := s[i:j]
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if word == "in" && (i == 0 || s[i-1] != '.') && k < len(s) && s[k] == '.' {
				word = inNamespace
			}
			b.WriteString(word)
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isWord(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
