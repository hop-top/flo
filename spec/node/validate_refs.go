package node

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// scope is the set of namespaces a field's placeholders may read.
type scope map[string]bool

var (
	scopeDeclared = scope{"in": true, "config": true, "run": true}
	scopeOutput   = scope{"out": true, "in": true, "config": true, "run": true}
	scopeInOnly   = scope{"in": true}
)

func (sc scope) names() string {
	out := make([]string, 0, len(sc))
	for k := range sc {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// field is one templatable string of a node spec and where it lives.
type field struct {
	path string
	val  string
}

// walkStrings appends every string leaf of v, at any depth, with its path.
func walkStrings(path string, v any, out *[]field) {
	switch t := v.(type) {
	case string:
		*out = append(*out, field{path, t})
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkStrings(path+"."+k, t[k], out)
		}
	case []any:
		for i, e := range t {
			walkStrings(fmt.Sprintf("%s[%d]", path, i), e, out)
		}
	}
}

func stringMapFields(prefix string, m map[string]string, out *[]field) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		*out = append(*out, field{prefix + "." + k, m[k]})
	}
}

// declaredFields lists every templatable string of a declared node.
func declaredFields(s *Spec) []field {
	var fs []field
	for i, a := range s.Argv {
		fs = append(fs, field{fmt.Sprintf("argv[%d]", i), a})
	}
	stringMapFields("env", s.Env, &fs)
	fs = append(fs, field{"cwd", s.Cwd})
	if s.Stdin != nil {
		walkStrings("stdin.json", s.Stdin.JSON, &fs)
		fs = append(fs, field{"stdin.text", s.Stdin.Text})
	}
	if r := s.Request; r != nil {
		fs = append(fs, field{"request.url", r.URL})
		stringMapFields("request.headers", r.Headers, &fs)
		walkStrings("request.query", map[string]any(r.Query), &fs)
		walkStrings("request.json", r.JSON, &fs)
		walkStrings("request.form", map[string]any(r.Form), &fs)
		fs = append(fs, field{"request.text", r.Text})
	}
	fs = append(fs, field{"outputs.from", s.Outputs.From})
	return fs
}

// checkScope reports every placeholder of the fields that reads outside sc.
func (c *checker) checkScope(rule string, fs []field, sc scope) {
	for _, f := range fs {
		if f.val == "" || !strings.Contains(f.val, "{{") {
			continue
		}
		refs, err := TemplateRefs(f.val)
		if err != nil {
			c.invalid(rule, f.path, "%v", err)
			continue
		}
		for _, root := range roots(refs) {
			if !sc[root] {
				c.add(rule, CodeOutOfScope, f.path, "reads %q; here only %s are bound", root, sc.names())
			}
		}
	}
}

// n04: a declared node reads only in, config and run.
func (c *checker) n04() {
	s := c.s
	if c.declared() {
		c.checkScope(RuleN04, declaredFields(s), scopeDeclared)
	}
	c.checkScope(RuleN04, []field{{"idempotency_key", s.IdempotencyKey}}, scopeInOnly)
	if s.Trust != nil {
		c.checkScope(RuleN04, []field{{"trust.class", s.Trust.Class}}, scopeInOnly)
	}
}

// n14: map and fail_when read out, in, config and run.
func (c *checker) n14() {
	o := c.s.Outputs
	c.checkScope(RuleN14, []field{{"outputs.map", o.Map}, {"outputs.fail_when", o.FailWhen}}, scopeOutput)
}

var (
	secretQueryRe  = regexp.MustCompile(`(?i)[?&](access_token|token|api[_-]?key|apikey|key|secret|password|passwd|signature|auth)=`)
	secretHeaderRe = regexp.MustCompile(`(?i)^(authorization|proxy-authorization|cookie|x-api-key|api-key|x-auth-token|x-access-token)$|token|secret|password`)
	secretNameRe   = regexp.MustCompile(`(?i)token|secret|password|passwd|api_?key|credential`)
	secretFlagRe   = regexp.MustCompile(`(?i)^--?(token|password|passwd|secret|api-?key|access-token)=.+`)
	userinfoRe     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/?#]*@`)
	headerSchemeRe = regexp.MustCompile(`^header:[A-Za-z][A-Za-z0-9-]*$`)
)

// stripPlaceholders replaces every {{ }} placeholder with a plain
// character so URL-shape checks see only the literal text.
func stripPlaceholders(s string) string {
	var b strings.Builder
	pos := 0
	for {
		i := strings.Index(s[pos:], "{{")
		if i < 0 {
			b.WriteString(s[pos:])
			return b.String()
		}
		open := pos + i
		b.WriteString(s[pos:open])
		end, ok := closePlaceholder(s, open+2)
		if !ok {
			b.WriteString(s[open:])
			return b.String()
		}
		b.WriteByte('X')
		pos = end + 2
	}
}

// n12: secrets travel by reference, never in a url, header or argv.
func (c *checker) n12() {
	s := c.s
	if r := s.Request; r != nil {
		c.secretRequest(r)
	}
	for i, a := range s.Argv {
		f := fmt.Sprintf("argv[%d]", i)
		if secretFlagRe.MatchString(a) {
			c.invalid(RuleN12, f, "a secret in argv shows in process listings; pass it through env by reference")
		}
		refs, err := TemplateRefs(a)
		if err != nil {
			continue // N04 reports it
		}
		for _, ref := range refs {
			if (ref.Root == "in" || ref.Root == "config") && len(ref.Path) > 0 && secretNameRe.MatchString(ref.Path[len(ref.Path)-1]) {
				c.invalid(RuleN12, f, "%s looks like a secret; argv would expose it", ref)
			}
		}
	}
}

func (c *checker) secretRequest(r *Request) {
	lit := stripPlaceholders(r.URL)
	if userinfoRe.MatchString(lit) {
		c.invalid(RuleN12, "request.url", "the url embeds credentials; use request.auth")
	}
	if secretQueryRe.MatchString(lit) {
		c.invalid(RuleN12, "request.url", "the url carries a secret query parameter; use request.auth")
	}
	for name := range r.Headers {
		if secretHeaderRe.MatchString(name) {
			c.invalid(RuleN12, "request.headers."+name, "header %q may carry a secret; use request.auth", name)
		}
	}
	for name := range r.Query {
		if secretQueryRe.MatchString("?" + name + "=") {
			c.invalid(RuleN12, "request.query."+name, "query parameter %q may carry a secret; use request.auth", name)
		}
	}
	a := r.Auth
	if a == nil {
		return
	}
	if !secretRe.MatchString(a.Secret) {
		c.invalid(RuleN12, "request.auth.secret", "secret %q: want a plain secret name, no placeholder or value", a.Secret)
	}
	if a.Scheme != "bearer" && a.Scheme != "basic" && !headerSchemeRe.MatchString(a.Scheme) {
		c.invalid(RuleN12, "request.auth.scheme", "scheme %q: want bearer, basic or header:<Name>", a.Scheme)
	}
}

// n13: examples are valid calls of the node.
func (c *checker) n13() {
	s := c.s
	outSchema, _ := s.Outputs.Schema.(map[string]any)
	for i, ex := range s.Examples {
		f := fmt.Sprintf("examples[%d]", i)
		in := map[string]any{}
		switch t := ex.In.(type) {
		case nil:
		case map[string]any:
			in = t
		default:
			c.invalid(RuleN13, f+".in", "in is a mapping of inputs")
			continue
		}
		for _, k := range sortedAny(in) {
			def, ok := s.Inputs[k]
			if !ok {
				c.invalid(RuleN13, f+".in."+k, "%q is not an input of the node", k)
				continue
			}
			for _, msg := range CheckValue(schemaOf(def), in[k]) {
				c.invalid(RuleN13, f+".in."+k, "%s", msg)
			}
		}
		for _, name := range sortedDefs(s.Inputs) {
			def := s.Inputs[name]
			if _, ok := in[name]; !ok && def.Required.Flag && !def.HasDefault() {
				c.invalid(RuleN13, f+".in", "required input %q is missing", name)
			}
		}
		if ex.Out != nil && outSchema != nil {
			for _, msg := range CheckValue(outSchema, ex.Out) {
				c.invalid(RuleN13, f+".out", "%s", msg)
			}
		}
	}
}

func sortedAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedDefs(m map[string]InputDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
