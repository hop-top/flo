package node

import "testing"

func TestModelPrivacy(t *testing.T) {
	cases := []struct {
		model string
		want  Privacy
		ok    bool
	}{
		{"ollama://gemma4:e4b", PrivacyLocal, true},
		{"ollama://gemma", PrivacyLocal, true},
		{"ollama://localhost:11434/gemma", PrivacyLocal, true},
		{"ollama://127.0.0.1/gemma", PrivacyLocal, true},
		{"ollama://127.5.5.5:11434/gemma", PrivacyLocal, true},
		{"ollama://[::1]:11434/gemma", PrivacyLocal, true},
		{"ollama://10.1.2.3/gemma", PrivacyLocal, true},
		{"ollama://172.16.0.9:11434/gemma", PrivacyLocal, true},
		{"ollama://192.168.1.5/gemma", PrivacyLocal, true},
		{"ollama://box.local:11434/gemma", PrivacyLocal, true},
		{"ollama:///var/run/ollama.sock", PrivacyLocal, true},
		{"ollama://unix:/var/run/ollama.sock", PrivacyLocal, true},
		{"ollama://%2Fvar%2Frun%2Follama.sock/gemma", PrivacyLocal, true},
		{"ollama://ollama.example.com/gemma", PrivacyCloud, true},
		{"ollama://8.8.8.8:11434/gemma", PrivacyCloud, true},
		{"ollama://172.32.0.1/gemma", PrivacyCloud, true},
		{"ollama://localhost.example.com/gemma", PrivacyCloud, true},
		{"ollama://[2001:db8::1]:11434/gemma", PrivacyCloud, true},
		{"openai://localhost:8080/m", PrivacyLocal, true},
		{"openai://192.168.1.5:8080/m", PrivacyLocal, true},
		{"openai://api.openai.com/gpt", PrivacyCloud, true},
		{"openai://gpt", PrivacyCloud, true},
		{"anthropic://claude-sonnet-5-5", PrivacyCloud, true},
		{"claude", "", false},
		{"://x", "", false},
	}
	for _, c := range cases {
		got, ok := ModelPrivacy(c.model)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q,%v want %q,%v", c.model, got, ok, c.want, c.ok)
		}
	}
}
