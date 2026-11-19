package builtin_test

import (
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
)

func TestModelPermitted(t *testing.T) {
	for _, c := range []struct {
		name        string
		allow, deny []string
		model       string
		want        bool
	}{
		{"neither set allows all", nil, nil, "anthropic://claude-haiku-5-5", true},
		{"blank lists are unset", []string{" "}, []string{""}, "ollama://m", true},
		{"only allow: listed", []string{"ollama://m"}, nil, "ollama://m", true},
		{"only allow: unlisted", []string{"ollama://m"}, nil, "ollama://n", false},
		{"only deny: denied", nil, []string{"ollama://m"}, "ollama://m", false},
		{"only deny: the rest", nil, []string{"ollama://m"}, "ollama://n", true},
		{"both: allowed and not denied", []string{"ollama://*"}, []string{"ollama://big*"}, "ollama://small", true},
		{"both: denied wins", []string{"ollama://*"}, []string{"ollama://big*"}, "ollama://big:70b", false},
		{"deny wins over the same entry", []string{"ollama://m"}, []string{"ollama://m"}, "ollama://m", false},
		{"both: not allowed", []string{"ollama://a*"}, []string{"ollama://big*"}, "ollama://small", false},
		{"deny star denies all", nil, []string{"*"}, "ollama://m", false},
		{"deny jev", nil, []string{"jev"}, "jev", false},
		{"deny jev leaves the rest", nil, []string{"jev"}, "ollama://m", true},
		{"deny jev glob", []string{"*"}, []string{"jev://typesafe/*"}, "jev", false},
		{"deny glob in the middle", nil, []string{"ollama://*-base-*"}, "ollama://qwen3:4b-base-2507", false},
		// The deny matcher is the allow matcher: structural, the glob in the
		// name only.
		{"deny does not reach another host", nil, []string{"ollama://qwen3*"}, "ollama://qwen3.attacker.example:11434/m", true},
		{"deny host form", nil, []string{"ollama://gpu.internal:11434/*"}, "ollama://gpu.internal:11434/big:70b", false},
		{"deny host form other host", nil, []string{"ollama://gpu.internal:11434/*"}, "ollama://gpu.internal.evil.example:11434/m", true},
		{"deny scheme case", nil, []string{"ollama://qwen3*"}, "OLLAMA://qwen3:4b", true},
		{"allow with a denied base_url", []string{"ollama://m"}, []string{"ollama://m?base_url=http://evil.example"}, "ollama://m?base_url=http://evil.example", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := builtin.ModelPermitted(c.allow, c.deny, c.model); got != c.want {
				t.Errorf("ModelPermitted(%v, %v, %q) = %v", c.allow, c.deny, c.model, got)
			}
		})
	}
}

// Bypass attempts hold for the deny list the same as for the allow list: the
// glob reaches the model name only. A model the glob does not match is not
// denied (it is another endpoint), and not allowed by the same entry.
func TestModelGlobBypassesBothLists(t *testing.T) {
	for _, model := range []string{
		"ollama://qwen3.attacker.example:11434/model",
		"ollama://qwen3:4b@attacker.example:11434/m",
		"ollama://qwen3@attacker.example",
		"ollama://qwen3/x",
		"ollama://qwen3..%2f",
		"ollama://qwen3?base_url=https://evil.example.com",
		"OLLAMA://qwen3:4b",
	} {
		entry := []string{"ollama://qwen3*"}
		if builtin.ModelAllowed(entry, model) {
			t.Errorf("allow entry %v matched %q", entry, model)
		}
		if builtin.ModelDenied(entry, model) {
			t.Errorf("deny entry %v matched %q", entry, model)
		}
	}
}

func TestValidateModelPatterns(t *testing.T) {
	for _, ok := range []string{
		"*", "jev", "jev://typesafe/*", "ollama://qwen3*", "ollama://qwen3:*-instruct-*", "ollama://qwen?:4b",
		"ollama://qwen[23]:4b", "ollama://localhost:11434/*", "ollama://[::1]:11434/*", "ollama://m?base_url=http://gpu.internal:1",
		"ollama://qwen?:4b?base_url=http://gpu.internal:1", " ", "",
	} {
		if err := builtin.ValidateModelPatterns([]string{ok}); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, c := range []struct{ entry, want string }{
		{"oll*", "not a model URI"},
		{"qwen3", "not a model URI"},
		{"*://qwen3", "scheme"},
		{"oll*://qwen3", "scheme"},
		{"ollama://*:11434/m", "host"},
		{"ollama://gpu-?.internal:11434/m", "host"},
		{"ollama://m?base_url=http://gpu.internal*", "params"},
		{"ollama://qwen[3", "bad glob"},
	} {
		err := builtin.ValidateModelPatterns([]string{"ollama://ok", c.entry})
		if !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.entry[:3]) {
			t.Errorf("%q: %v", c.entry, err)
		}
	}
}

func TestModelGlobIPv6HostIsExact(t *testing.T) {
	e := []string{"ollama://[::1]:11434/*"}
	if !builtin.ModelAllowed(e, "ollama://[::1]:11434/m") || builtin.ModelAllowed(e, "ollama://[::2]:11434/m") {
		t.Error("ipv6 host is matched exactly")
	}
}

// A [...] class can match a `/` in path.Match; a glob here may not cross one.
func TestModelGlobClassDoesNotCrossASlash(t *testing.T) {
	for _, c := range []struct {
		entry, model string
		want         bool
	}{
		{"ollama://qwen3[^a]x", "ollama://qwen3/x", false},
		{"ollama://qwen3[^a]x", "ollama://qwen3bx", true},
		{"ollama://hf.co/u[^a]m", "ollama://hf.co/u/m", false},
		{"ollama://hf.co/*/m", "ollama://hf.co/u/m", true},
	} {
		if got := builtin.ModelAllowed([]string{c.entry}, c.model); got != c.want {
			t.Errorf("ModelAllowed(%q, %q) = %v", c.entry, c.model, got)
		}
		if got := builtin.ModelDenied([]string{c.entry}, c.model); got != c.want {
			t.Errorf("ModelDenied(%q, %q) = %v", c.entry, c.model, got)
		}
	}
	if err := builtin.ValidateModelPatterns([]string{"ollama://a[/]b"}); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "class") {
		t.Errorf("slash in a class: %v", err)
	}
}
