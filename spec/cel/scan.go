package cel

import (
	"strings"
	"unicode/utf8"
)

// tpart is a literal run or a placeholder of a template. For a
// placeholder, expr is the text between the braces and off its byte offset
// in the scanned string.
type tpart struct {
	lit    string
	expr   string
	off    int
	isExpr bool
}

// scanTemplate splits s into literals and placeholders. The scanner is
// CEL-aware (flow spec §4.1): it skips string literals and comments and
// tracks brace depth, so `}}` closes a placeholder only at depth zero.
func scanTemplate(s string) ([]tpart, *Error) {
	var parts []tpart
	pos := 0
	for {
		i := strings.Index(s[pos:], "{{")
		if i < 0 {
			break
		}
		open := pos + i
		if open > pos {
			parts = append(parts, tpart{lit: s[pos:open]})
		}
		end, ok := closePlaceholder(s, open+2)
		if !ok {
			line, col := lineCol(s, open)
			return nil, &Error{Code: CodeUnterminated, Msg: "placeholder {{ is never closed by }}", Line: line, Col: col}
		}
		parts = append(parts, tpart{expr: s[open+2 : end], off: open + 2, isExpr: true})
		pos = end + 2
	}
	if pos < len(s) {
		parts = append(parts, tpart{lit: s[pos:]})
	}
	return parts, nil
}

// closePlaceholder returns the index of the `}}` that closes a placeholder
// whose expression starts at from.
func closePlaceholder(s string, from int) (int, bool) {
	depth := 0
	for i := from; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			i = skipString(s, i) - 1
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			i = skipComment(s, i) - 1
		case c == '{':
			depth++
		case c == '}':
			if depth > 0 {
				depth--
			} else if i+1 < len(s) && s[i+1] == '}' {
				return i, true
			}
		}
	}
	return 0, false
}

// skipString returns the index just past the CEL string literal whose
// opening quote is at s[i]; an unterminated literal runs to the end.
func skipString(s string, i int) int {
	q := s[i]
	raw := (i >= 1 && (s[i-1] == 'r' || s[i-1] == 'R')) ||
		(i >= 2 && (s[i-1] == 'b' || s[i-1] == 'B') && (s[i-2] == 'r' || s[i-2] == 'R'))
	triple := strings.HasPrefix(s[i:], string([]byte{q, q, q}))
	j := i + 1
	if triple {
		j = i + 3
	}
	for j < len(s) {
		switch {
		case !raw && s[j] == '\\':
			j += 2
		case triple && strings.HasPrefix(s[j:], string([]byte{q, q, q})):
			return j + 3
		case !triple && s[j] == q:
			return j + 1
		default:
			j++
		}
	}
	return len(s)
}

func skipComment(s string, i int) int {
	if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
		return i + nl
	}
	return len(s)
}

// lineCol converts a byte offset in s to a 1-based line and rune column.
func lineCol(s string, off int) (line, col int) {
	if off > len(s) {
		off = len(s)
	}
	line = 1 + strings.Count(s[:off], "\n")
	start := strings.LastIndexByte(s[:off], '\n') + 1
	return line, 1 + utf8.RuneCountInString(s[start:off])
}

// offsetOf is the inverse of lineCol for a 1-based line and 0-based rune
// column, clamped to the text.
func offsetOf(s string, line, col int) int {
	off := 0
	for l := 1; l < line; l++ {
		nl := strings.IndexByte(s[off:], '\n')
		if nl < 0 {
			return len(s)
		}
		off += nl + 1
	}
	for ; col > 0 && off < len(s); col-- {
		_, n := utf8.DecodeRuneInString(s[off:])
		off += n
	}
	return off
}

// edit records a rewrite: s[origStart:origEnd] became n[newStart:newEnd].
type edit struct{ origStart, origEnd, newStart, newEnd int }

// mapBack translates a byte offset in the rewritten text to the original.
func mapBack(edits []edit, off int) int {
	delta := 0
	for _, e := range edits {
		switch {
		case off >= e.newEnd:
			delta += (e.origEnd - e.origStart) - (e.newEnd - e.newStart)
		case off >= e.newStart:
			return e.origStart
		}
	}
	return off + delta
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isRefByte(c byte) bool { return isIdentByte(c) || c == '/' || c == '-' }

// normalizeRefs rewrites step-id references that CEL cannot lex into index
// form (flow spec §4.1): results.sec/scan.stdout becomes
// results["sec/scan"].stdout. A dotted segment after results or steps
// that contains "/" is always a step id (expanded ids carry it); one that
// contains "-" is a step id only when known names it, so results.a-b read
// as a subtraction stays one. String literals and comments are untouched.
func normalizeRefs(s string, known map[string]bool) (string, []edit) {
	var b strings.Builder
	var edits []edit
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			j := skipString(s, i)
			b.WriteString(s[i:j])
			i = j
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			j := skipComment(s, i)
			b.WriteString(s[i:j])
			i = j
		case isIdentByte(c):
			j := i
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			word := s[i:j]
			prevDot := i > 0 && s[i-1] == '.'
			if word == "in" && !prevDot && isInNamespace(s, i, j) {
				start := b.Len()
				b.WriteString(inAlias)
				edits = append(edits, edit{origStart: i, origEnd: j, newStart: start, newEnd: b.Len()})
				i = j
				continue
			}
			b.WriteString(word)
			if (word == "results" || word == "steps") && !prevDot && j < len(s) && s[j] == '.' {
				if k, ok := stepIDRun(s, j+1, known); ok {
					start := b.Len()
					b.WriteString(`["` + s[j+1:k] + `"]`)
					edits = append(edits, edit{origStart: j, origEnd: k, newStart: start, newEnd: b.Len()})
					j = k
				}
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), edits
}

// stepIDRun reads the step id after "results." at s[from:] and reports its
// end when it must be written in index form.
func stepIDRun(s string, from int, known map[string]bool) (int, bool) {
	end := from
	for end < len(s) && isRefByte(s[end]) {
		end++
	}
	for end > from && (s[end-1] == '-' || s[end-1] == '/') {
		end--
	}
	run := s[from:end]
	if run == "" {
		return 0, false
	}
	if strings.Contains(run, "/") || (strings.Contains(run, "-") && known[run]) {
		return end, true
	}
	return 0, false
}

// inAlias stands for the `in` namespace while an expression is compiled:
// `in` is a CEL keyword (the membership operator), so `in.x` does not parse.
// normalizeRefs rewrites the namespace to the alias, Eval binds act["in"]
// to it, and everything user-visible (refs, messages) says `in` again.
const inAlias = "in__"

// isInNamespace reports whether the `in` at s[i:j] starts a selection of the
// namespace (`in.x`, `in['a']`) rather than being the membership operator
// (`a in b`, `"k" in m`): it must be followed by `.` or `[` and must not
// follow an operand.
func isInNamespace(s string, i, j int) bool {
	k := j
	for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
		k++
	}
	if k >= len(s) || (s[k] != '.' && s[k] != '[') {
		return false
	}
	p := i - 1
	for p >= 0 && (s[p] == ' ' || s[p] == '\t' || s[p] == '\n') {
		p--
	}
	if p < 0 {
		return true
	}
	switch c := s[p]; {
	case c == ')' || c == ']' || c == '}' || c == '\'' || c == '"':
		return false
	case isIdentByte(c):
		q := p
		for q >= 0 && isIdentByte(s[q]) {
			q--
		}
		return s[q+1:p+1] == "in" // `x in in.list`: the second `in` is the namespace
	}
	return true
}
