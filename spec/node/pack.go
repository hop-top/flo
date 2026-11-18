package node

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"hop.top/flo/spec"
)

// Pack is a pack manifest (node.md §17): what one tool or domain needs,
// bundled and version-pinned. The engine core imports nothing from a pack;
// a pack reaches its tool through a declared command, an HTTP API or a
// plugin.
type Pack struct {
	Pack        string       `yaml:"pack" json:"pack"`
	Version     string       `yaml:"version" json:"version"`
	Description string       `yaml:"description,omitempty" json:"description,omitempty"`
	Requires    PackRequires `yaml:"requires,omitempty" json:"requires,omitempty"`

	// Config is what the operator sets, read in node specs as
	// config.<ns>.<key>: namespace, then key, then its definition.
	Config map[string]map[string]InputDef `yaml:"config,omitempty" json:"config,omitempty"`

	// The file lists are paths relative to the manifest.
	Nodes   []string `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	Trusts  []string `yaml:"trusts,omitempty" json:"trusts,omitempty"`
	Prompts []string `yaml:"prompts,omitempty" json:"prompts,omitempty"`
	// Plugins are code entries; in phase 1 they run in process.
	Plugins []string `yaml:"plugins,omitempty" json:"plugins,omitempty"`
	// Flows are template flows the pack ships.
	Flows []string `yaml:"flows,omitempty" json:"flows,omitempty"`

	Ext  map[string]any `yaml:"-" json:"-"`
	Path string         `yaml:"-" json:"-"`
	Hash string         `yaml:"-" json:"-"`
}

// PackRequires is what a pack needs of its host.
type PackRequires struct {
	// Flo is a version constraint on the engine: ">=0.1", ">=0.1, <0.3".
	Flo string `yaml:"flo,omitempty" json:"flo,omitempty"`
}

var (
	packNameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	packVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

// ParsePack decodes a pack manifest strictly (unknown keys are errors,
// except x-* at the root) without applying CheckPack.
func ParsePack(r io.Reader, filename string) (*Pack, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read pack %s: %w", filename, err)
	}
	var p Pack
	ext, errs := decodeDoc(data, filename, "pack", &p, []string{"pack", "version"})
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", filename, errs)
	}
	sum := sha256.Sum256(data)
	p.Ext, p.Path, p.Hash = ext, filename, hex.EncodeToString(sum[:])
	return &p, nil
}

// CheckPack validates a manifest on its own: name and version grammar, the
// engine constraint, config definitions, and that every listed path is
// relative, clean and inside the pack.
func CheckPack(p *Pack) Errors {
	c := &checker{}
	if !packNameRe.MatchString(p.Pack) {
		c.invalid("", "pack", "name %q must match %s", p.Pack, packNameRe)
	}
	if !packVersionRe.MatchString(p.Version) {
		c.invalid("", "version", "version %q: want MAJOR.MINOR.PATCH", p.Version)
	}
	if p.Requires.Flo != "" {
		if _, err := ParseConstraint(p.Requires.Flo); err != nil {
			c.invalid("", "requires.flo", "%v", err)
		}
	}
	for ns, defs := range p.Config {
		if !nsRe.MatchString(ns) {
			c.invalid("", "config."+ns, "config namespace %q must match %s", ns, nsRe)
		}
		c.defs("config."+ns, defs)
	}
	for _, l := range []struct {
		name  string
		paths []string
		exts  []string
	}{
		{"nodes", p.Nodes, docExts}, {"trusts", p.Trusts, docExts},
		{"prompts", p.Prompts, nil}, {"plugins", p.Plugins, nil}, {"flows", p.Flows, nil},
	} {
		c.paths(l.name, l.paths, l.exts)
	}
	return c.errs
}

var docExts = []string{".yaml", ".yml", ".json"}

func (c *checker) paths(name string, paths, exts []string) {
	seen := map[string]bool{}
	for i, p := range paths {
		f := fmt.Sprintf("%s[%d]", name, i)
		switch {
		case !fs.ValidPath(p) || p == ".":
			c.invalid("", f, "path %q must be relative and stay inside the pack", p)
		case seen[p]:
			c.invalid("", f, "path %q listed twice", p)
		case exts != nil && !hasExt(p, exts):
			c.invalid("", f, "path %q: want a %s file", p, strings.Join(exts, ", "))
		}
		seen[p] = true
	}
}

func hasExt(p string, exts []string) bool {
	e := strings.ToLower(path.Ext(p))
	for _, x := range exts {
		if e == x {
			return true
		}
	}
	return false
}

// Constraint is a version constraint: comma-separated clauses, each an
// operator (>=, >, <=, <, =) and a version.
type Constraint struct{ clauses []clause }

type clause struct {
	op  string
	ver string
}

var clauseRe = regexp.MustCompile(`^(>=|<=|>|<|==|=)?\s*v?([0-9]+(\.[0-9]+){0,2}(-[0-9A-Za-z.-]+)?)$`)

// ParseConstraint parses ">=0.1" or ">=0.1, <0.3". A bare version means =.
func ParseConstraint(s string) (Constraint, error) {
	var out Constraint
	for _, part := range strings.Split(s, ",") {
		m := clauseRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return Constraint{}, fmt.Errorf("constraint %q: want clauses like >=0.1, <0.3", s)
		}
		op := m[1]
		if op == "" || op == "==" {
			op = "="
		}
		out.clauses = append(out.clauses, clause{op, m[2]})
	}
	return out, nil
}

// Satisfied reports whether version meets every clause.
func (c Constraint) Satisfied(version string) bool {
	for _, cl := range c.clauses {
		cmp := spec.CompareVersions(version, cl.ver)
		ok := false
		switch cl.op {
		case ">=":
			ok = cmp >= 0
		case ">":
			ok = cmp > 0
		case "<=":
			ok = cmp <= 0
		case "<":
			ok = cmp < 0
		case "=":
			ok = cmp == 0
		}
		if !ok {
			return false
		}
	}
	return true
}

// Satisfies reports whether the engine version meets requires.flo; a pack
// with no constraint is satisfied by any engine.
func (p *Pack) Satisfies(floVersion string) bool {
	if p.Requires.Flo == "" {
		return true
	}
	c, err := ParseConstraint(p.Requires.Flo)
	return err == nil && c.Satisfied(floVersion)
}

// LoadedPack is a manifest with its node specs and trust providers read
// and validated.
type LoadedPack struct {
	Pack   *Pack
	Nodes  []*Spec
	Trusts []*TrustProvider
	// Dir is the manifest's directory inside the FS it was loaded from.
	Dir string
}

// Namespaces lists the namespaces the pack's nodes declare: the ones the
// pack owns. Sorted.
func (lp *LoadedPack) Namespaces() []string {
	seen := map[string]bool{}
	for _, n := range lp.Nodes {
		if t, err := n.Type(); err == nil {
			seen[t.NS] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Entries returns the pack's nodes as locator entries, attributed to the
// pack and its exact version.
func (lp *LoadedPack) Entries() []Entry {
	out := make([]Entry, len(lp.Nodes))
	for i, n := range lp.Nodes {
		out[i] = Entry{Spec: n, Source: Source{Layer: LayerPack, Pack: lp.Pack.Pack, PackVersion: lp.Pack.Version, Path: n.Path}}
	}
	return out
}

// TrustEntries returns the pack's trust providers as locator entries.
func (lp *LoadedPack) TrustEntries() []TrustEntry {
	out := make([]TrustEntry, len(lp.Trusts))
	for i, t := range lp.Trusts {
		out[i] = TrustEntry{Provider: t, Source: Source{Layer: LayerPack, Pack: lp.Pack.Pack, PackVersion: lp.Pack.Version, Path: t.Path}}
	}
	return out
}

// LoadPack reads the manifest at manifest in fsys, then every node spec and
// trust provider it lists (paths relative to the manifest), validating each
// one. On top of the per-document rules it checks the pack as a whole: no
// two nodes of one type, no two providers of one name, and every
// config.<ns>.<key> a template reads is declared by the pack (or by the
// node itself, for its own namespace).
func LoadPack(fsys fs.FS, manifest string) (*LoadedPack, error) {
	data, err := fs.ReadFile(fsys, manifest)
	if err != nil {
		return nil, fmt.Errorf("read pack manifest: %w", err)
	}
	p, err := ParsePack(strings.NewReader(string(data)), manifest)
	if err != nil {
		return nil, err
	}
	if errs := CheckPack(p); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", manifest, errs)
	}
	lp := &LoadedPack{Pack: p, Dir: path.Dir(manifest)}
	var errs Errors
	for _, rel := range p.Nodes {
		file := path.Join(lp.Dir, rel)
		b, err := fs.ReadFile(fsys, file)
		if err != nil {
			errs = append(errs, &Error{Code: CodeInvalidNode, Field: "nodes", Msg: fmt.Sprintf("read %s: %v", rel, err)})
			continue
		}
		n, err := Load(strings.NewReader(string(b)), file)
		if err != nil {
			errs = append(errs, flatten(err)...)
			continue
		}
		lp.Nodes = append(lp.Nodes, n)
	}
	for _, rel := range p.Trusts {
		file := path.Join(lp.Dir, rel)
		b, err := fs.ReadFile(fsys, file)
		if err != nil {
			errs = append(errs, &Error{Code: CodeInvalidNode, Field: "trusts", Msg: fmt.Sprintf("read %s: %v", rel, err)})
			continue
		}
		t, err := LoadTrust(strings.NewReader(string(b)), file)
		if err != nil {
			errs = append(errs, flatten(err)...)
			continue
		}
		lp.Trusts = append(lp.Trusts, t)
	}
	errs = append(errs, lp.crossCheck()...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("pack %s: %w", p.Pack, errs)
	}
	return lp, nil
}

// flatten turns any error into Errors, keeping typed members.
func flatten(err error) Errors {
	var es Errors
	if asErrors(err, &es) {
		return es
	}
	return Errors{{Code: CodeInvalidNode, Msg: err.Error()}}
}

func (lp *LoadedPack) crossCheck() Errors {
	var errs Errors
	seenType := map[string]string{}
	for _, n := range lp.Nodes {
		if first, dup := seenType[n.Node]; dup {
			errs = append(errs, &Error{Code: CodeNodeConflict, Field: "nodes",
				Msg: fmt.Sprintf("%s is declared by %s and %s", n.Node, first, n.Path)})
		}
		seenType[n.Node] = n.Path
		errs = append(errs, lp.configErrors(n.Path, n.Node, n.Config, n.ConfigRefs())...)
	}
	seenTrust := map[string]string{}
	for _, t := range lp.Trusts {
		if first, dup := seenTrust[t.Trust]; dup {
			errs = append(errs, &Error{Code: CodeNodeConflict, Field: "trusts",
				Msg: fmt.Sprintf("trust provider %s is declared by %s and %s", t.Trust, first, t.Path)})
		}
		seenTrust[t.Trust] = t.Path
		errs = append(errs, lp.configErrors(t.Path, "", nil, t.ConfigRefs())...)
	}
	return errs
}

// CodeNodeConflict is two documents declaring the same node type or trust
// provider name.
const CodeNodeConflict = "node_conflict"

// configErrors checks references into config against the pack's config and
// the node's own settings (which live in the node's namespace).
func (lp *LoadedPack) configErrors(file, nodeType string, own map[string]InputDef, refs []Ref) Errors {
	var errs Errors
	ownNS := ""
	if t, err := ParseType(nodeType); err == nil {
		ownNS = t.NS
	}
	for _, r := range refs {
		if len(r.Path) == 0 {
			continue
		}
		ns := r.Path[0]
		var key string
		if len(r.Path) > 1 {
			key = r.Path[1]
		}
		_, inPack := lp.Pack.Config[ns]
		inOwn := ns == ownNS && len(own) > 0
		switch {
		case !inPack && !inOwn:
			errs = append(errs, &Error{Code: CodeInvalidNode, Field: file,
				Msg: fmt.Sprintf("%s reads config namespace %q, which the pack does not declare", r, ns)})
		case key != "":
			_, okPack := lp.Pack.Config[ns][key]
			_, okOwn := own[key]
			if !okPack && !(inOwn && okOwn) {
				errs = append(errs, &Error{Code: CodeInvalidNode, Field: file,
					Msg: fmt.Sprintf("%s reads config key %q, which the pack does not declare", r, ns+"."+key)})
			}
		}
	}
	return errs
}

func asErrors(err error, target *Errors) bool { return errors.As(err, target) }

// ConfigRefs lists the config.<ns>.<key> paths the node's templates read,
// sorted and without duplicates.
func (s *Spec) ConfigRefs() []Ref { return configRefs(s.allFields()) }

// allFields lists every templatable string of the spec: the declared
// fields, the output expressions, the idempotency key and the trust class.
func (s *Spec) allFields() []field {
	fs := declaredFields(s)
	fs = append(fs, field{"outputs.map", s.Outputs.Map}, field{"outputs.fail_when", s.Outputs.FailWhen},
		field{"idempotency_key", s.IdempotencyKey})
	if s.Trust != nil {
		fs = append(fs, field{"trust.class", s.Trust.Class})
	}
	return fs
}
