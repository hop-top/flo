package node

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"

	"hop.top/flo/spec"
)

// TrustProvider is a trust-provider document (node.md §10.3): a pack- or
// bundle-declared object, referenced by name (`trust: inbox`), that wraps a
// tool owning the trust counters. The engine never writes those counters;
// the three operations are the only way it touches them.
type TrustProvider struct {
	Trust       string `yaml:"trust" json:"trust"` // the provider name
	Kind        Kind   `yaml:"kind" json:"kind"`   // command or http
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// SideEffect is the class of the provider's calls. Absent means
	// write-shared; a provider is refused above write-shared (§10.3).
	SideEffect SideEffect `yaml:"side_effect,omitempty" json:"side_effect,omitempty"`

	// Read lists the trust levels: [{class, level: ask|auto, streak,
	// threshold, resets, last_reset}]. Decide records the decisions of one
	// approval. Set backs `flo trust set`.
	Read   *TrustOp `yaml:"read,omitempty" json:"read,omitempty"`
	Decide *TrustOp `yaml:"decide,omitempty" json:"decide,omitempty"`
	Set    *TrustOp `yaml:"set,omitempty" json:"set,omitempty"`

	// Env and Cwd apply to every command operation.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Cwd string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`

	// Ext holds the x-* keys of the root.
	Ext map[string]any `yaml:"-" json:"-"`
	// Path and Hash are set by ParseTrust.
	Path string `yaml:"-" json:"-"`
	Hash string `yaml:"-" json:"-"`
}

// TrustOp is one operation of a provider, executed with the rules of
// node.md §4 (command) or §5 (http). Templates see `in`, `config` and `run`.
type TrustOp struct {
	Argv     []string  `yaml:"argv,omitempty" json:"argv,omitempty"`
	Stdin    *Stdin    `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	Request  *Request  `yaml:"request,omitempty" json:"request,omitempty"`
	Response *Response `yaml:"response,omitempty" json:"response,omitempty"`
	Timeout  string    `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Trust levels a provider reports (node.md §10.3).
const (
	TrustLevelAsk  = "ask"
	TrustLevelAuto = "auto"
)

var trustNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Ops lists the declared operations by name, in read, decide, set order.
func (t *TrustProvider) Ops() []NamedOp {
	var out []NamedOp
	for _, o := range []NamedOp{{"read", t.Read}, {"decide", t.Decide}, {"set", t.Set}} {
		if o.Op != nil {
			out = append(out, o)
		}
	}
	return out
}

// NamedOp is a TrustOp with its name.
type NamedOp struct {
	Name string
	Op   *TrustOp
}

// EffectiveSideEffect applies the default: write-shared.
func (t *TrustProvider) EffectiveSideEffect() SideEffect {
	if t.SideEffect == "" {
		return SideEffectWriteShared
	}
	return t.SideEffect
}

// ParseTrust decodes a trust-provider document strictly (unknown keys are
// errors, except x-* at the root) without applying CheckTrust.
func ParseTrust(r io.Reader, filename string) (*TrustProvider, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read trust provider %s: %w", filename, err)
	}
	var t TrustProvider
	ext, errs := decodeDoc(data, filename, "trust provider", &t, []string{"trust", "kind"})
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", filename, errs)
	}
	sum := sha256.Sum256(data)
	t.Ext, t.Path, t.Hash = ext, filename, hex.EncodeToString(sum[:])
	return &t, nil
}

// LoadTrust is ParseTrust followed by CheckTrust.
func LoadTrust(r io.Reader, filename string) (*TrustProvider, error) {
	t, err := ParseTrust(r, filename)
	if err != nil {
		return nil, err
	}
	if errs := CheckTrust(t); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", filename, errs)
	}
	return t, nil
}

// CheckTrust validates a provider: a name, a command or http kind, a read
// operation, each operation shaped for the kind, placeholders reading only
// in, config and run, secrets by reference, durations, and a side effect
// no stronger than write-shared (the install-time check of §10.3).
func CheckTrust(t *TrustProvider) Errors {
	c := &checker{}
	if !trustNameRe.MatchString(t.Trust) {
		c.invalid("", "trust", "name %q must match %s", t.Trust, trustNameRe)
	}
	if t.Kind != KindCommand && t.Kind != KindHTTP {
		c.invalid("", "kind", "kind %q: a trust provider is command or http", t.Kind)
	}
	if t.SideEffect != "" && !t.SideEffect.Valid() {
		c.invalid("", "side_effect", "side_effect %q is not on the ladder", t.SideEffect)
	}
	if t.EffectiveSideEffect().AtLeast(SideEffectIrreversible) {
		c.invalid("", "side_effect", "a trust provider is at most write-shared, not %s", t.EffectiveSideEffect())
	}
	if t.Read == nil {
		c.invalid("", "read", "a trust provider declares read: how levels are listed")
	}
	if t.Kind == KindHTTP && (t.Env != nil || t.Cwd != "") {
		c.invalid("", "env", "env and cwd apply to command providers")
	}
	var fs []field
	stringMapFields("env", t.Env, &fs)
	fs = append(fs, field{"cwd", t.Cwd})
	for _, o := range t.Ops() {
		c.trustOp(o.Name, o.Op, t.Kind, &fs)
	}
	c.checkScope("", fs, scopeDeclared)
	return c.errs
}

func (c *checker) trustOp(name string, op *TrustOp, kind Kind, fs *[]field) {
	f := func(k string) string { return name + "." + k }
	switch kind {
	case KindCommand:
		if len(op.Argv) == 0 {
			c.invalid("", f("argv"), "a command operation requires argv")
		}
		if op.Request != nil || op.Response != nil {
			c.invalid("", f("request"), "request and response belong to an http provider")
		}
	case KindHTTP:
		if op.Request == nil {
			c.invalid("", f("request"), "an http operation requires request")
		}
		if len(op.Argv) > 0 || op.Stdin != nil {
			c.invalid("", f("argv"), "argv and stdin belong to a command provider")
		}
	}
	if op.Timeout != "" {
		if _, err := spec.ParseDuration(op.Timeout); err != nil {
			c.invalid("", f("timeout"), "%v", err)
		}
	}
	for i, a := range op.Argv {
		*fs = append(*fs, field{fmt.Sprintf("%s[%d]", f("argv"), i), a})
	}
	if op.Stdin != nil {
		walkStrings(f("stdin.json"), op.Stdin.JSON, fs)
		*fs = append(*fs, field{f("stdin.text"), op.Stdin.Text})
	}
	if r := op.Request; r != nil {
		*fs = append(*fs, field{f("request.url"), r.URL})
		stringMapFields(f("request.headers"), r.Headers, fs)
		walkStrings(f("request.query"), r.Query, fs)
		walkStrings(f("request.json"), r.JSON, fs)
		walkStrings(f("request.form"), r.Form, fs)
		*fs = append(*fs, field{f("request.text"), r.Text})
		c.secretRequest(r)
	}
}

// ConfigRefs lists the config.<ns>.<key> paths the provider's templates
// read, sorted and without duplicates.
func (t *TrustProvider) ConfigRefs() []Ref {
	var fs []field
	stringMapFields("env", t.Env, &fs)
	fs = append(fs, field{"cwd", t.Cwd})
	for _, o := range t.Ops() {
		for i, a := range o.Op.Argv {
			fs = append(fs, field{fmt.Sprintf("argv[%d]", i), a})
		}
		if o.Op.Stdin != nil {
			walkStrings("stdin", o.Op.Stdin.JSON, &fs)
			fs = append(fs, field{"stdin.text", o.Op.Stdin.Text})
		}
		if r := o.Op.Request; r != nil {
			fs = append(fs, field{"url", r.URL})
			stringMapFields("headers", r.Headers, &fs)
		}
	}
	return configRefs(fs)
}

// configRefs picks the references into the config namespace out of fields.
func configRefs(fs []field) []Ref {
	seen := map[string]Ref{}
	for _, f := range fs {
		refs, err := TemplateRefs(f.val)
		if err != nil {
			continue
		}
		for _, r := range refs {
			if r.Root == "config" {
				seen[r.String()] = r
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Ref, 0, len(keys))
	for _, k := range keys {
		out = append(out, seen[k])
	}
	return out
}
