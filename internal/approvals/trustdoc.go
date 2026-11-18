package approvals

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"hop.top/flo/internal/core"
)

// Provider is a trust provider document (node.md §10.3): where to read the
// levels, where to report decisions, how to promote or demote. Only the
// command kind runs in phase 1.
//
//	trust: inbox
//	kind: command
//	read:   { argv: [inbox, trust, list, --format, json] }
//	decide: { argv: [inbox, approve, "{{ in.context.run }}", --decisions, "-"],
//	          stdin: { json: "{{ in.decisions }}" } }
//	set:    { argv: [inbox, trust, "{{ in.level == 'auto' ? 'promote' : 'reset' }}", "{{ in.class }}"] }
type Provider struct {
	Trust string `yaml:"trust"`
	Kind  string `yaml:"kind"`
	// Description and SideEffect are read for the spec's sake (it validates
	// them at install); the engine uses neither.
	Description string `yaml:"description,omitempty"`
	SideEffect  string `yaml:"side_effect,omitempty"`
	// Read, Decide and Set are the three operations.
	Read   *Action `yaml:"read,omitempty"`
	Decide *Action `yaml:"decide,omitempty"`
	Set    *Action `yaml:"set,omitempty"`
	// Env and Cwd apply to every action that sets none of its own.
	Env map[string]string `yaml:"env,omitempty"`
	Cwd string            `yaml:"cwd,omitempty"`
	// Timeout bounds each call, a duration like "30s"; empty is 30s.
	Timeout string `yaml:"timeout,omitempty"`
}

// Action is one of the provider's three commands.
type Action struct {
	Argv  []string          `yaml:"argv"`
	Stdin *Stdin            `yaml:"stdin,omitempty"`
	Env   map[string]string `yaml:"env,omitempty"`
	Cwd   string            `yaml:"cwd,omitempty"`
}

// Stdin is JSON text or plain text, rendered over `in`.
type Stdin struct {
	JSON any    `yaml:"json,omitempty"`
	Text string `yaml:"text,omitempty"`
}

// ParseProvider decodes a provider document strictly.
func ParseProvider(data []byte) (Provider, error) {
	var p Provider
	data, err := withoutExtensions(data)
	if err != nil {
		return Provider{}, core.Errorf(core.CodeInvalidNode, "trust provider: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Provider{}, core.Errorf(core.CodeInvalidNode, "trust provider: %w", err)
	}
	if p.Trust == "" {
		return Provider{}, core.New(core.CodeInvalidNode, "trust provider: `trust` (the name) is required")
	}
	switch p.Kind {
	case "command":
	case "http":
		return Provider{}, core.New(core.CodeEngineFeature, "trust provider "+p.Trust+": kind http is not supported in phase 1")
	default:
		return Provider{}, core.Errorf(core.CodeInvalidNode, "trust provider %s: kind %q (want command)", p.Trust, p.Kind)
	}
	for name, a := range map[string]*Action{"read": p.Read, "decide": p.Decide, "set": p.Set} {
		if a != nil && len(a.Argv) == 0 {
			return Provider{}, core.Errorf(core.CodeInvalidNode, "trust provider %s: %s needs argv", p.Trust, name)
		}
	}
	return p, nil
}

// withoutExtensions drops the x-* keys of the document's root: the spec lets
// a document carry them, and the engine has no use for them.
func withoutExtensions(data []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err //nolint:wrapcheck // named by the caller
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return data, nil
	}
	m := root.Content[0]
	kept := m.Content[:0:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if strings.HasPrefix(m.Content[i].Value, "x-") {
			continue
		}
		kept = append(kept, m.Content[i], m.Content[i+1])
	}
	m.Content = kept
	return yaml.Marshal(&root) //nolint:wrapcheck // named by the caller
}

// Locator finds the provider document of a name. Where providers live
// (a flow bundle's trusts/, an installed pack's, the user config) is the
// assembly's choice; a Locator hides it.
type Locator interface {
	// Find returns the provider, or not_found.
	Find(ctx context.Context, tenant core.TenantID, name string) (Provider, error)
}

var providerName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// DirLocator looks for `<name>.yaml` or `<name>.yml` in each directory, in
// order: the first hit wins, so a flow-local provider shadows a pack's.
type DirLocator struct {
	Dirs []string
}

// Find implements Locator.
func (l DirLocator) Find(_ context.Context, _ core.TenantID, name string) (Provider, error) {
	if !providerName.MatchString(name) {
		return Provider{}, core.Errorf(core.CodeUsage, "trust provider name %q: want lowercase letters, digits, - and _", name)
	}
	for _, d := range l.Dirs {
		for _, ext := range []string{".yaml", ".yml"} {
			data, err := os.ReadFile(filepath.Join(d, name+ext)) //nolint:gosec // name is validated, dirs are configured
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return Provider{}, core.Errorf(core.CodeInternal, "trust provider %s: %w", name, err)
			}
			p, err := ParseProvider(data)
			if err != nil {
				return Provider{}, err
			}
			if p.Trust != name {
				return Provider{}, core.Errorf(core.CodeInvalidNode, "trust provider file %s names %q", filepath.Join(d, name+ext), p.Trust)
			}
			return p, nil
		}
	}
	return Provider{}, core.Errorf(core.CodeNotFound, "trust provider %q not found", name).
		WithSuggestion("declare it in a pack's trusts/ or the flow bundle's trusts/")
}

// StaticLocator serves providers from memory, for tests and for assemblies
// that load them elsewhere.
type StaticLocator map[string]Provider

// Find implements Locator.
func (l StaticLocator) Find(_ context.Context, _ core.TenantID, name string) (Provider, error) {
	if p, ok := l[name]; ok {
		return p, nil
	}
	return Provider{}, core.Errorf(core.CodeNotFound, "trust provider %q not found", name)
}
