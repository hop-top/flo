// Package prompts loads the versioned prompt files a flow's AI nodes name
// (node.md §6.5): `name@N` resolves to `prompts/<name>@<N>.yaml` in the flow
// bundle, then in the installed packs, and renders as a Go text/template
// over a context.
package prompts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"hop.top/flo/internal/core"
)

// Ref is a prompt reference `name@N`.
type Ref struct {
	Name    string
	Version int
}

var refRe = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*)@([1-9][0-9]*)$`)

// ParseRef parses `name@N`. A name holds no path separator.
func ParseRef(s string) (Ref, error) {
	m := refRe.FindStringSubmatch(s)
	if m == nil {
		return Ref{}, core.Errorf(core.CodeInputInvalid, "prompt %q is not name@N (lowercase name, version from 1)", s)
	}
	v, _ := strconv.Atoi(m[2])
	return Ref{Name: m[1], Version: v}, nil
}

// String is `name@N`.
func (r Ref) String() string { return fmt.Sprintf("%s@%d", r.Name, r.Version) }

// File is the file name of the prompt: `name@N.yaml`.
func (r Ref) File() string { return r.String() + ".yaml" }

// Source layers of a resolved prompt.
const (
	SourceBundle = "bundle"
	SourcePack   = "pack"
)

// Prompt is a loaded prompt file.
type Prompt struct {
	Ref
	// System and Template are the file's text; Template is a text/template.
	System   string
	Template string
	// Hash is the sha256 of the file bytes, hex: the recorded run names the
	// prompt text it used by it.
	Hash string
	// Source is SourceBundle or SourcePack; Pack names the pack; Path is
	// the file.
	Source string
	Pack   string
	Path   string

	tpl *template.Template
}

// file is the YAML shape of a prompt file.
type file struct {
	System   string `yaml:"system"`
	Template string `yaml:"template"`
}

// Parse decodes a prompt file strictly and compiles its template. Both keys
// may be absent only for system; the template is required.
func Parse(ref Ref, data []byte) (*Prompt, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, core.Errorf(core.CodeInputInvalid, "prompt %s: %v", ref, err)
	}
	if strings.TrimSpace(f.Template) == "" {
		return nil, core.Errorf(core.CodeInputInvalid, "prompt %s: template is empty", ref)
	}
	tpl, err := template.New(ref.String()).Funcs(funcs).Option("missingkey=error").Parse(f.Template)
	if err != nil {
		return nil, core.Errorf(core.CodeInputInvalid, "prompt %s: %v", ref, err)
	}
	sum := sha256.Sum256(data)
	return &Prompt{Ref: ref, System: f.System, Template: f.Template, Hash: hex.EncodeToString(sum[:]), tpl: tpl}, nil
}

// Render executes the template over ctx. A key the template reads and ctx
// lacks is an error, not an empty string.
func (p *Prompt) Render(ctx map[string]any) (string, error) {
	if ctx == nil {
		ctx = map[string]any{}
	}
	var b bytes.Buffer
	if err := p.tpl.Execute(&b, ctx); err != nil {
		return "", core.Errorf(core.CodeInputInvalid, "prompt %s: %v", p.Ref, err)
	}
	return b.String(), nil
}

// funcs is the whole function set of a template: nothing that reads the
// machine, only text helpers.
var funcs = template.FuncMap{
	"join": func(sep string, v any) string {
		var parts []string
		switch t := v.(type) {
		case []string:
			parts = t
		case []any:
			for _, e := range t {
				parts = append(parts, fmt.Sprint(e))
			}
		case nil:
		default:
			parts = []string{fmt.Sprint(t)}
		}
		return strings.Join(parts, sep)
	},
	// trunc cuts s to at most n runes.
	"trunc": func(n int, s string) string {
		if n < 0 {
			n = 0
		}
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n])
	},
	// default returns d when v is nil, "" or an empty list or map.
	"default": func(d, v any) any {
		switch t := v.(type) {
		case nil:
			return d
		case string:
			if t == "" {
				return d
			}
		case []any:
			if len(t) == 0 {
				return d
			}
		case map[string]any:
			if len(t) == 0 {
				return d
			}
		}
		return v
	},
}

// PackFiles are the prompt files one installed pack lists.
type PackFiles struct {
	Pack  string
	Files []string // absolute paths
}

// Loader resolves prompt references: the bundle's prompts/ first, then the
// packs in order. The first layer with the file wins; layers are not merged.
type Loader struct {
	Packs []PackFiles
}

// Load resolves ref against bundle (a flow bundle directory, "" for none)
// and the loader's packs.
func (l Loader) Load(bundle, ref string) (*Prompt, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if bundle != "" {
		path := filepath.Join(bundle, "prompts", r.File())
		if p, err := read(r, path); err == nil {
			p.Source, p.Path = SourceBundle, path
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	for _, pk := range l.Packs {
		for _, path := range pk.Files {
			if filepath.Base(path) != r.File() {
				continue
			}
			p, err := read(r, path)
			if err != nil {
				return nil, err
			}
			p.Source, p.Pack, p.Path = SourcePack, pk.Pack, path
			return p, nil
		}
	}
	return nil, core.Errorf(core.CodeNotFound, "prompt %s: no prompts/%s in the flow bundle or an installed pack", r, r.File())
}

// read loads one file; a missing file is returned as fs.ErrNotExist.
func read(r Ref, path string) (*Prompt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // the caller tests for absence
		}
		return nil, core.Errorf(core.CodeInternal, "read prompt %s: %v", path, err)
	}
	return Parse(r, data)
}
