package builtin

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/kit/go/ai/llm"
)

// Names of the model policy settings: `ai.models.allow` and `ai.models.deny`
// (comma-separated in the environment). Both hold model patterns (see
// ModelPermitted).
const (
	EnvModelsAllow = "FLO_AI_MODELS_ALLOW"
	EnvModelsDeny  = "FLO_AI_MODELS_DENY"
)

// globMeta are the characters of a glob: path.Match's.
const globMeta = `*?[\`

// queryStart is where an entry's query params begin: a `?` followed by a
// `key=`. Any other `?` of an entry is a glob.
var queryStart = regexp.MustCompile(`\?[A-Za-z_][A-Za-z0-9_]*=`)

// qmark stands for a glob `?` while the entry goes through the URI parser,
// which reads every `?` as the start of the query.
const qmark = "\x00"

// modelPattern is one entry of a model list, read.
type modelPattern struct {
	// any is the entry `*`: every model.
	any bool
	// scheme, host and params are the endpoint: matched exactly, never as
	// globs. name is the model name: a glob when it has glob characters.
	scheme, host string
	params       map[string]string
	name         string
}

// parsePattern reads one entry: `*`, `jev`, or a model URI whose model name
// may be a glob. Entries and models are read by kit's llm.ParseURI, the parser
// the client uses, never compared as strings.
func parsePattern(entry string) (modelPattern, error) {
	e := canonModel(entry)
	if e == "*" {
		return modelPattern{any: true}, nil
	}
	scheme, rest, ok := strings.Cut(e, "://")
	if !ok || scheme == "" {
		return modelPattern{}, fmt.Errorf("%q is not a model URI (write scheme://<model>, jev or *)", llm.RedactURI(e))
	}
	var query string
	if loc := queryStart.FindStringIndex(rest); loc != nil {
		rest, query = rest[:loc[0]], rest[loc[0]+1:]
	}
	u, err := llm.ParseURI(scheme + "://" + strings.ReplaceAll(rest, "?", qmark))
	if err != nil {
		return modelPattern{}, fmt.Errorf("%q is not a model URI", llm.RedactURI(e))
	}
	p := modelPattern{
		scheme: u.Scheme, host: strings.ReplaceAll(u.Host, qmark, "?"), name: strings.ReplaceAll(u.Model, qmark, "?"),
	}
	if query != "" {
		q, err := llm.ParseURI(scheme + "://x?" + query)
		if err != nil {
			return modelPattern{}, fmt.Errorf("%q is not a model URI", llm.RedactURI(e))
		}
		p.params = q.Params
	}
	return p, nil
}

// hostGlob is whether a host has glob characters; the brackets of an IPv6
// literal are not.
func hostGlob(h string) bool {
	if hp, _, err := net.SplitHostPort(h); err == nil && net.ParseIP(hp) != nil {
		return false
	}
	return hasGlob(h)
}

// slashInClass is whether a `/` sits inside a [...] class of the glob.
func slashInClass(g string) bool {
	in := false
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case c == '\\':
			i++
		case c == '[':
			in = true
		case c == ']':
			in = false
		case c == '/' && in:
			return true
		}
	}
	return false
}

func hasGlob(s string) bool { return strings.ContainsAny(s, globMeta) }

// check says whether the entry is usable: globs are the model name's only.
func (p modelPattern) check(entry string) error {
	if p.any {
		return nil
	}
	if hasGlob(p.scheme) || hostGlob(p.host) {
		return fmt.Errorf("%q: globs are matched in the model name only, not in the scheme or host", llm.RedactURI(entry))
	}
	for k, v := range p.params {
		if hasGlob(k) || hasGlob(v) {
			return fmt.Errorf("%q: globs are matched in the model name only, not in query params", llm.RedactURI(entry))
		}
	}
	if slashInClass(p.name) {
		return fmt.Errorf("%q: a [...] class may not hold a /", llm.RedactURI(entry))
	}
	if _, err := path.Match(p.name, ""); err != nil {
		return fmt.Errorf("%q: bad glob in the model name: %v", llm.RedactURI(entry), err)
	}
	return nil
}

// ValidateModelPatterns checks a model list setting (allow or deny): every
// entry must be `*`, `jev` or a model URI, with globs (`*`, `?`, `[...]`)
// in the model name only. The error names the entry.
func ValidateModelPatterns(list []string) error {
	for _, e := range list {
		if strings.TrimSpace(e) == "" {
			continue
		}
		p, err := parsePattern(e)
		if err == nil {
			err = p.check(e)
		}
		if err != nil {
			return core.New(core.CodeUsage, err.Error()).
				WithSuggestion("write *, jev, or scheme://[host:port/]<model> with *, ? and [...] in the model name, e.g. ollama://qwen3*")
		}
	}
	return nil
}

// matches reports whether the model, parsed as mu, is the entry's. The
// endpoint (scheme as written, host and port, query params: `base_url` moves
// it) is equal, never globbed; the glob stands for the model name only, and
// path.Match's `*` and `?` do not cross a `/`; a class (`[^a]`) can, so the model has as many `/` as the pattern. A glob never reaches a name
// that holds `@`, `?`, `#` or `..`: the model name stays a name, not a
// userinfo, a query, a fragment or a path.
func (p modelPattern) matches(mu llm.URI) bool {
	switch {
	case p.any:
		return true
	case p.scheme != mu.Scheme || p.host != mu.Host || len(p.params) != len(mu.Params):
		return false
	}
	for k, v := range p.params {
		if w, ok := mu.Params[k]; !ok || w != v {
			return false
		}
	}
	if !hasGlob(p.name) {
		return p.name == mu.Model
	}
	if strings.ContainsAny(mu.Model, "@?#") || strings.Contains(mu.Model, "..") ||
		strings.Count(mu.Model, "/") != strings.Count(p.name, "/") {
		return false
	}
	ok, err := path.Match(p.name, mu.Model)
	return err == nil && ok
}

// listed reports whether model matches any entry of list. Entries that cannot
// be read match nothing; ValidateModelPatterns refuses them at load.
func listed(list []string, model string) bool {
	m := canonModel(model)
	mu, merr := llm.ParseURI(m)
	for _, e := range list {
		if strings.TrimSpace(e) == "" {
			continue
		}
		p, err := parsePattern(e)
		if err != nil || p.check(e) != nil {
			continue
		}
		if p.any || (merr == nil && p.matches(mu)) {
			return true
		}
	}
	return false
}

// ModelAllowed reports whether model is on the list: an exact model URI, or a
// model URI whose name is a glob, or `*` for every model. See
// ModelPermitted for the policy.
func ModelAllowed(list []string, model string) bool { return listed(list, model) }

// ModelDenied reports whether model is on the deny list; the same matcher as
// ModelAllowed.
func ModelDenied(list []string, model string) bool { return listed(list, model) }

// ModelPermitted is the model policy of `ai.models.allow` and
// `ai.models.deny`. Deny wins. With neither list set every model is
// permitted; with only allow, the listed ones; with only deny, all but the
// denied; with both, the allowed and not denied. The matcher is that of
// ModelAllowed: entries and models are compared as parsed URIs, the endpoint
// (scheme, host and port, params) exactly, and the entry's globs (`*`, `?`,
// `[...]`, as path.Match) match the model name only. `jev` stands for the
// default Jev model on both sides.
func ModelPermitted(allow, deny []string, model string) bool {
	if ModelDenied(deny, model) {
		return false
	}
	for _, e := range allow {
		if strings.TrimSpace(e) != "" {
			return ModelAllowed(allow, model)
		}
	}
	return true
}
