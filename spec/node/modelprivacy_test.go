package node

import (
	"errors"
	"strings"
	"testing"
)

func mustLocal(t *testing.T, entries ...string) *LocalHosts {
	t.Helper()
	l, err := NewLocalHosts(entries)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestModelPrivacy(t *testing.T) {
	listed := mustLocal(t, "gpu.internal", "100.64.0.0/10", "box.ts.net")
	cases := []struct {
		name  string
		model string
		local *LocalHosts
		want  Privacy
	}{
		// scheme defaults
		{"ollama bare model", "ollama://gemma4:e4b", nil, PrivacyLocal},
		{"ollama dotted model is a model name", "ollama://llama3.1", nil, PrivacyLocal},
		{"ollama dotted authority is a model name", "ollama://example.com", nil, PrivacyLocal},
		{"ollama namespaced model", "ollama://hf.co/user/model:q4", nil, PrivacyLocal},
		{"lmstudio default is localhost", "lmstudio://qwen", nil, PrivacyLocal},
		{"openai default is hosted", "openai://gpt-5", nil, PrivacyCloud},
		{"anthropic", "anthropic://claude-sonnet-5-5", nil, PrivacyCloud},
		{"openrouter", "openrouter://openai/gpt-5", nil, PrivacyCloud},
		{"unknown scheme", "acme://m", nil, PrivacyCloud},
		// host:port
		{"ollama localhost", "ollama://localhost:11434/gemma", nil, PrivacyLocal},
		{"ollama loopback", "ollama://127.5.5.5:11434/gemma", nil, PrivacyLocal},
		{"ollama ipv6 loopback", "ollama://[::1]:11434/gemma", nil, PrivacyLocal},
		{"ollama rfc1918", "ollama://192.168.1.5:11434/gemma", nil, PrivacyLocal},
		{"openai local server", "openai://localhost:8080/m", nil, PrivacyLocal},
		{"openai private", "openai://192.168.1.5:8080/m", nil, PrivacyLocal},
		{"openai named host", "openai://api.openai.com:443/gpt", nil, PrivacyCloud},
		{"ollama public ip", "ollama://8.8.8.8:11434/gemma", nil, PrivacyCloud},
		{"ollama 172.32 is public", "ollama://172.32.0.1:11434/gemma", nil, PrivacyCloud},
		{"mdns name is a named host", "ollama://box.local:11434/gemma", nil, PrivacyCloud},
		{"a *.localhost name is a named host, as kit reads it", "ollama://foo.localhost:11434/gemma", nil, PrivacyCloud},
		{"lookalike of localhost", "ollama://localhost.example.com:11434/gemma", nil, PrivacyCloud},
		{"lmstudio remote host", "lmstudio://example.com:1234/m", nil, PrivacyCloud},
		// base_url wins over host and default
		{"base_url leak past local host", "openai://localhost:8000/m?base_url=https://api.openai.com/v1", nil, PrivacyCloud},
		{"base_url leak past local default", "ollama://m?base_url=https://api.example.com", nil, PrivacyCloud},
		{"base_url leak past lmstudio default", "lmstudio://m?base_url=https://api.example.com/v1", nil, PrivacyCloud},
		{"base_url local over cloud default", "openai://m?base_url=http://localhost:8000/v1", nil, PrivacyLocal},
		{"base_url local over cloud host", "openai://api.example.com:443/m?base_url=http://127.0.0.1:8000", nil, PrivacyLocal},
		{"base_url with other params", "openai://m?api_key=x&base_url=http://localhost:8000/v1", nil, PrivacyLocal},
		{"empty base_url drops host, takes default", "openai://localhost:8000/m?base_url=", nil, PrivacyCloud},
		// tailnet and named hosts need listing
		{"tailnet ip", "ollama://100.64.1.2:11434/gemma", nil, PrivacyCloud},
		{"tailnet name", "ollama://box.ts.net:11434/gemma", nil, PrivacyCloud},
		{"tailnet ip listed by cidr", "ollama://100.64.1.2:11434/gemma", listed, PrivacyLocal},
		{"tailnet name listed", "ollama://box.ts.net:11434/gemma", listed, PrivacyLocal},
		{"named host listed", "openai://gpu.internal:8000/m", listed, PrivacyLocal},
		{"named host listed, case", "openai://GPU.Internal:8000/m", listed, PrivacyLocal},
		{"unlisted named host with list", "openai://other.internal:8000/m", listed, PrivacyCloud},
		{"listed host via base_url", "openai://m?base_url=http://gpu.internal:8000/v1", listed, PrivacyLocal},
		{"listed host does not rescue a public base_url", "openai://gpu.internal:8000/m?base_url=https://api.openai.com/v1", listed, PrivacyCloud},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ModelPrivacy(c.model, c.local)
			if err != nil || got != c.want {
				t.Errorf("%s: got %q,%v want %q", c.model, got, err, c.want)
			}
		})
	}
}

func TestModelPrivacyRefused(t *testing.T) {
	for _, m := range []string{
		"ollama:///gemma",
		"ollama:///var/run/ollama.sock",
		"ollama://unix:/var/run/ollama.sock",
		"ollama://%2Fvar%2Frun%2Follama.sock/gemma",
		"ollama://localhost/gemma",
		"ollama://127.0.0.1/gemma",
		"ollama://[::1]/gemma",
		"openai://localhost/m",
		"ollama://localhost:abc/gemma",
		"openai://m?base_url=localhost:8000",
		"openai://m?base_url=ftp://example.com",
	} {
		_, err := ModelPrivacy(m, nil)
		if !errors.Is(err, ErrNotRunnable) {
			t.Errorf("%s: err = %v, want ErrNotRunnable", m, err)
			continue
		}
		if !strings.Contains(err.Error(), "ollama://<model>") {
			t.Errorf("%s: no guidance in %q", m, err)
		}
	}
}

func TestModelPrivacyNotURI(t *testing.T) {
	for _, m := range []string{"claude", "://x", ""} {
		if _, err := ModelPrivacy(m, nil); !errors.Is(err, ErrNotModelURI) {
			t.Errorf("%q: err = %v", m, err)
		}
	}
}

func TestNewLocalHostsRejectsBadCIDR(t *testing.T) {
	if _, err := NewLocalHosts([]string{"10.0.0.0/99"}); err == nil {
		t.Error("bad CIDR accepted")
	}
}
