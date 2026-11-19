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
		// Deny reads the entry's name only: its host narrows nothing.
		{"deny host form other host", nil, []string{"ollama://gpu.internal:11434/*"}, "ollama://gpu.internal.evil.example:11434/m", false},
		{"deny scheme case", nil, []string{"ollama://qwen3*"}, "OLLAMA://qwen3:4b", false},
		{"allow with a denied base_url", []string{"ollama://m"}, []string{"ollama://m?base_url=http://evil.example"}, "ollama://m?base_url=http://evil.example", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := builtin.ModelPermitted(c.allow, c.deny, c.model); got != c.want {
				t.Errorf("ModelPermitted(%v, %v, %q) = %v", c.allow, c.deny, c.model, got)
			}
		})
	}
}

// Bypass attempts on the allow list: the glob reaches the model name only,
// the endpoint is matched structurally. Deny is name-only (see
// TestModelDenyMatchesNameAtAnyEndpoint).
func TestModelGlobBypassesAllowList(t *testing.T) {
	for _, model := range []string{
		"ollama://qwen3.attacker.example:11434/model",
		"ollama://qwen3:4b@attacker.example:11434/m",
		"ollama://qwen3@attacker.example",
		"ollama://qwen3/x",
		"ollama://qwen3..%2f",
		"ollama://qwen3?base_url=https://evil.example.com",
		"OLLAMA://qwen3:4b",
		"ollama://otherhost:11434/qwen3:4b",
		"openai://qwen3:4b",
	} {
		entry := []string{"ollama://qwen3*"}
		if builtin.ModelAllowed(entry, model) {
			t.Errorf("allow entry %v matched %q", entry, model)
		}
	}
}

// Deny matches the model name at any endpoint and any scheme: pointing a
// model elsewhere does not escape it.
func TestModelDenyMatchesNameAtAnyEndpoint(t *testing.T) {
	denied := []string{
		"ollama://qwen3:4b",
		"ollama://qwen3:4b?base_url=http://other",
		"ollama://otherhost:11434/qwen3:4b",
		"openai://localhost:11434/qwen3:4b",
		"openai://qwen3:4b",
		"ollama://[::1]:11434/qwen3:4b",
		"ollama://[2001:db8::1]:11434/qwen3:4b?base_url=http://x",
		"OLLAMA://qwen3:4b",
	}
	kept := []string{
		"ollama://gemma4:e4b",
		"ollama://otherhost:11434/gemma4:e4b",
		"ollama://qwen3.attacker.example:11434/gemma4",
		"ollama://x/qwen3:4b/extra",
	}
	for _, entry := range []string{
		"ollama://qwen3*", "qwen3*", "openai://qwen3*", "ollama://gpu.internal:11434/qwen3*",
		"ollama://[::1]:11434/qwen3*", "ollama://qwen3*?base_url=http://z", "qwen3:4b*", "qwen?:4b",
	} {
		for _, m := range denied {
			if !builtin.ModelDenied([]string{entry}, m) {
				t.Errorf("deny %q did not match %q", entry, m)
			}
			if builtin.ModelPermitted(nil, []string{entry}, m) {
				t.Errorf("deny %q permitted %q", entry, m)
			}
		}
		for _, m := range kept {
			if builtin.ModelDenied([]string{entry}, m) {
				t.Errorf("deny %q matched %q", entry, m)
			}
		}
	}
	// Allow stays structural.
	for _, m := range denied[1:] {
		if m == "OLLAMA://qwen3:4b" || m == "openai://qwen3:4b" {
			continue
		}
		if builtin.ModelAllowed([]string{"ollama://qwen3*"}, m) {
			t.Errorf("allow matched %q", m)
		}
	}
}

func TestModelDenyBareNamesAndStar(t *testing.T) {
	for _, c := range []struct {
		deny  []string
		model string
		want  bool
	}{
		{[]string{"gemma4:e4b"}, "ollama://gemma4:e4b", true},
		{[]string{"gemma4:e4b"}, "ollama://h:1/gemma4:e4b", true},
		{[]string{"gemma4:e4b"}, "ollama://gemma4:e2b", false},
		{[]string{"hf.co/u/*"}, "ollama://hf.co/u/m", true},
		{[]string{"hf.co/u/*"}, "ollama://hf.co/v/m", false},
		{[]string{"*"}, "openai://h:1/anything", true},
		{[]string{"jev"}, "jev", true},
		{[]string{"jev"}, "jev://typesafe/jev-1.14", true},
		{[]string{"jev"}, "openai://typesafe/jev-1.14", true},
		{[]string{"jev"}, "ollama://m", false},
		{[]string{"jev://typesafe/*"}, "jev", true},
		{[]string{"jev://typesafe/*"}, "jev://typesafe/other", true},
		{[]string{"jev://typesafe/*"}, "ollama://typesafe/x", true},
		{[]string{"typesafe/*"}, "jev", true},
		{[]string{"typesafe/*"}, "ollama://m", false},
	} {
		if got := builtin.ModelDenied(c.deny, c.model); got != c.want {
			t.Errorf("ModelDenied(%v, %q) = %v", c.deny, c.model, got)
		}
	}
}

// Deny fails safe: odd names over-deny rather than slip through. `*` crosses a
// `/`, no `@ ? # ..` guard, and an unreadable model is denied when any entry is
// set.
func TestModelDenyFailsSafe(t *testing.T) {
	for _, m := range []string{
		"ollama://qwen3:4b@host/m",
		"ollama://qwen3:4b@attacker.example:11434/m",
		"ollama://qwen3@attacker.example",
		"ollama://qwen3..%2f",
		"ollama://qwen3/x",
		"ollama://qwen3/x/y",
		"ollama://qwen3#frag",
		"ollama://h:1/qwen3/a/b",
		"ollama://h:1/qwen3:4b..x",
	} {
		for _, e := range []string{"qwen3*", "ollama://qwen3*"} {
			if !builtin.ModelDenied([]string{e}, m) {
				t.Errorf("deny %q let %q through", e, m)
			}
		}
	}
	if !builtin.ModelDenied([]string{"ollama://m"}, "nope") || !builtin.ModelDenied([]string{"x"}, "::not a uri") {
		t.Error("unreadable model not denied")
	}
	if builtin.ModelDenied([]string{" ", ""}, "::not a uri") || builtin.ModelDenied(nil, "::not a uri") {
		t.Error("no entry, no denial")
	}
	if builtin.ModelDenied([]string{"qwen3*"}, "ollama://gemma4:e4b") {
		t.Error("over-deny of an unrelated name")
	}
	// Allow keeps its guards.
	if builtin.ModelAllowed([]string{"ollama://qwen3*"}, "ollama://qwen3:4b@host/m") || builtin.ModelAllowed([]string{"ollama://qwen3*"}, "ollama://qwen3/x") {
		t.Error("allow lost a guard")
	}
}

func TestValidateDenyPatterns(t *testing.T) {
	for _, ok := range []string{"*", "jev", "qwen3*", "qwen3:4b", "hf.co/u/*", "ollama://qwen3*", "jev://typesafe/*", " ", ""} {
		if err := builtin.ValidateDenyPatterns([]string{ok}); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"qwen[3", "a[/]b", "*://qwen3", "ollama://qwen[3", "ollama://a[/]b", "ollama://*:11434/m"} {
		if err := builtin.ValidateDenyPatterns([]string{"ok*", bad}); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), bad[:3]) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Allow still refuses a bare name.
	if err := builtin.ValidateModelPatterns([]string{"qwen3*"}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("allow bare name: %v", err)
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
		// Deny fails safe: a class or `*` crosses a `/` too.
		if got := builtin.ModelDenied([]string{c.entry}, c.model); !got {
			t.Errorf("ModelDenied(%q, %q) = %v", c.entry, c.model, got)
		}
	}
	if err := builtin.ValidateModelPatterns([]string{"ollama://a[/]b"}); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "class") {
		t.Errorf("slash in a class: %v", err)
	}
}
