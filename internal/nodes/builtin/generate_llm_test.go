package builtin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/prompts"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
	"hop.top/kit/go/ai/llm"
	_ "hop.top/kit/go/ai/llm/ollama"
)

// fakeHTTP is the engine's HTTP seam: it records every request and answers
// from reply. Nothing reaches a network.
type fakeHTTP struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []string
	reply  func(*http.Request) (int, string)
}

func (f *fakeHTTP) Do(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		// The reply func may want the body too.
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	code, out := f.reply(r)
	return &http.Response{
		StatusCode: code, Status: http.StatusText(code), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(out)), Request: r,
	}, nil
}

func (f *fakeHTTP) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }

// ollamaReply answers an ollama /api/chat call with text and token counts.
func ollamaReply(text string, in, out int) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) {
		b, _ := json.Marshal(map[string]any{
			"message": map[string]any{"role": "assistant", "content": text},
			"done":    true, "prompt_eval_count": in, "eval_count": out,
		})
		return 200, string(b)
	}
}

// The cloud scheme of these tests: an adapter that posts to its default
// endpoint with the key as a bearer token. It stands for a hosted provider
// without a vendor SDK.
const fakeCloud = "fakecloud"

func init() {
	llm.Register(fakeCloud, func(cfg llm.ResolvedConfig) (llm.Provider, error) {
		return &fakeCloudProvider{cfg: cfg}, nil
	}, llm.Declaration{BaseURL: "https://api.fakecloud.example/v1"})
}

type fakeCloudProvider struct{ cfg llm.ResolvedConfig }

func (p *fakeCloudProvider) Close() error { return nil }

func (p *fakeCloudProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Provider.BaseURL+"/complete", bytes.NewReader(nil))
	if err != nil {
		return llm.Response{}, err
	}
	if p.cfg.Provider.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.Provider.APIKey)
	}
	resp, err := p.cfg.HTTPClient().Do(hr)
	if err != nil {
		return llm.Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Text    string `json:"text"`
		In, Out int
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{Content: out.Text, Role: "assistant",
		Usage: llm.Usage{PromptTokens: out.In, CompletionTokens: out.Out, TotalTokens: out.In + out.Out}}, nil
}

func cloudReply(text string, in, out int) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) {
		b, _ := json.Marshal(map[string]any{"text": text, "In": in, "Out": out})
		return 200, string(b)
	}
}

// storeSecrets is core.Secrets over a map of secret name to value; it holds
// the engine's tenant and profile to account.
type storeSecrets struct {
	t    *testing.T
	vals map[string]string
	got  []string
}

func (s *storeSecrets) Get(_ context.Context, tenant core.TenantID, profile, name string) ([]byte, error) {
	s.t.Helper()
	if tenant != "default" || profile != "prof" {
		s.t.Errorf("secret asked for tenant %q profile %q", tenant, profile)
	}
	s.got = append(s.got, name)
	v, ok := s.vals[name]
	if !ok {
		return nil, core.Errorf(core.CodeSecretNotFound, "secret %q not found for profile %q", name, profile)
	}
	return []byte(v), nil
}

func perMillion(in, out float64) builtin.PriceFunc {
	return func(_ context.Context, scheme, model string) (llm.Price, bool) {
		if scheme == fakeCloud && model == "big" {
			return llm.Price{Input: in, Output: out}, true
		}
		return llm.Price{}, false
	}
}

func llmGen(t *testing.T, h *fakeHTTP, s core.Secrets, price builtin.PriceFunc) builtin.Generator {
	t.Helper()
	g, err := builtin.NewLLMGenerator(builtin.LLMDeps{HTTP: h, Secrets: s, LocalHosts: listedHosts, Price: price, Allow: allowAny})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const sysPrompt = "system: be brief\ntemplate: |\n  Meeting {{ .title }}\n"

func TestLLMHappyPathLocal(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("a brief", 120, 30)}
	s := &storeSecrets{t: t}
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	res := gen(t, llmGen(t, h, s, nil), b, specrun.PrivacyLocalOnly,
		genIn("ollama://gemma4:e4b", map[string]any{"title": "sync"}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if out := res.Output.(map[string]any); out["text"] != "a brief" {
		t.Errorf("text %v", out["text"])
	}
	if h.calls() != 1 || h.reqs[0].URL.Path != "/api/chat" || h.reqs[0].URL.Host != "localhost:11434" {
		t.Fatalf("calls %d %v", h.calls(), h.reqs)
	}
	var sent struct {
		Model    string
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(h.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "gemma4:e4b" || len(sent.Messages) != 2 ||
		sent.Messages[0].Role != "system" || sent.Messages[0].Content != "be brief" ||
		sent.Messages[1].Role != "user" || sent.Messages[1].Content != "Meeting sync\n" {
		t.Errorf("sent %+v", sent)
	}
	if h.reqs[0].Header.Get("Authorization") != "" {
		t.Error("a local model got a key")
	}
	if len(s.got) != 0 {
		t.Errorf("secrets read for a local model: %v", s.got)
	}
	if res.Cost.USD != 0 || res.Cost.Tokens != 150 {
		t.Errorf("local cost %+v: want zero USD, 150 tokens", res.Cost)
	}
}

func TestLLMNoSystemTextSendsOnlyTheUserMessage(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, llmGen(t, h, &storeSecrets{t: t}, nil), b, specrun.PrivacyLocalOnly, genIn("ollama://m", nil))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if strings.Contains(h.bodies[0], `"system"`) {
		t.Errorf("empty system text sent: %s", h.bodies[0])
	}
}

func TestLLMCloudCostAndKey(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("hello", 1000, 200)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "s3cret"}}
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	res := gen(t, llmGen(t, h, s, perMillion(3, 15)), b, specrun.PrivacyCloudAllowed,
		genIn("fakecloud://big", map[string]any{"title": "t"}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if h.calls() != 1 || h.reqs[0].URL.String() != "https://api.fakecloud.example/v1/complete" {
		t.Fatalf("calls %d", h.calls())
	}
	if got := h.reqs[0].Header.Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("authorization %q", got)
	}
	// (1000*3 + 200*15) / 1e6
	if res.Cost.Tokens != 1200 || res.Cost.USD < 0.0059999 || res.Cost.USD > 0.0060001 {
		t.Errorf("cost %+v", res.Cost)
	}
}

// A model the price table does not know costs its tokens and no dollars: the
// run's token budget still counts it.
func TestLLMUnpricedCloudModelCountsTokens(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("hello", 10, 5)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, llmGen(t, h, s, perMillion(3, 15)), b, specrun.PrivacyCloudAllowed, genIn("fakecloud://other", nil))
	if res.Outcome != core.OutcomeOK || res.Cost.Tokens != 15 || res.Cost.USD != 0 {
		t.Fatalf("%+v %+v", res.Cost, res.Err)
	}
}

func TestLLMMissingSecretFailsBeforeAnyCall(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	s := &storeSecrets{t: t}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, llmGen(t, h, s, nil), b, specrun.PrivacyCloudAllowed, genIn("fakecloud://big", nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.Err.Message, "fakecloud_api_key") && !strings.Contains(res.Err.Suggestion, "fakecloud_api_key") {
		t.Errorf("error does not name the secret: %+v", res.Err)
	}
	if h.calls() != 0 {
		t.Errorf("%d calls with no key", h.calls())
	}
}

func TestLLMNoSecretStoreIsAMissingSecret(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, llmGen(t, h, nil, nil), b, specrun.PrivacyCloudAllowed, genIn("fakecloud://big", nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound || h.calls() != 0 {
		t.Fatalf("%+v calls=%d", res, h.calls())
	}
}

// A secret never goes to an endpoint the model URI names: the store's key is
// for the scheme's own default endpoint.
func TestLLMKeyStaysOffAnEndpointTheURINames(t *testing.T) {
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "s3cret"}}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	for _, model := range []string{
		"fakecloud://other.example:8443/m",
		"fakecloud://m?base_url=https://other.example/v1",
	} {
		h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
		res := gen(t, llmGen(t, h, s, nil), b, specrun.PrivacyCloudAllowed, genIn(model, nil))
		if res.Outcome != core.OutcomeOK {
			t.Fatalf("%s: %+v", model, res.Err)
		}
		if got := h.reqs[0].Header.Get("Authorization"); got != "" {
			t.Errorf("%s: key sent to a named endpoint: %q", model, got)
		}
	}
	if len(s.got) != 0 {
		t.Errorf("secret read for a named endpoint: %v", s.got)
	}
}

func TestLLMLocalOnlyCeilingRefusesCloud(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	for _, c := range []struct {
		name  string
		model string
		level specrun.Privacy
	}{
		{"cloud scheme", "fakecloud://big", specrun.PrivacyLocalOnly},
		{"no ceiling is the strictest", "fakecloud://big", ""},
		{"public ollama host", "ollama://ollama.example.com:11434/gemma", specrun.PrivacyLocalOnly},
		{"base_url past a local host", "ollama://localhost:11434/m?base_url=https://api.example.com", specrun.PrivacyLocalOnly},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := gen(t, llmGen(t, h, s, nil), b, c.level, genIn(c.model, nil))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
				t.Fatalf("%+v", res)
			}
		})
	}
	if h.calls() != 0 || len(s.got) != 0 {
		t.Errorf("a refused step sent %d requests, read %v", h.calls(), s.got)
	}
}

// The backend holds the line by itself: handed a cloud model under a
// local-only request, kit's local-only resolution refuses it, with no key
// read and nothing sent.
func TestLLMBackendRefusesCloudWhenLocalOnly(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
	g := llmGen(t, h, s, nil)
	p := loadPrompt(t, "template: hi\n")
	for _, model := range []string{"fakecloud://big", "ollama://ollama.example.com:11434/g", "ollama://foo.localhost:11434/g"} {
		_, _, err := g.Generate(context.Background(), builtin.GenerateRequest{
			Prompt: p, Model: model, LocalOnly: true, Level: node.PrivacyLocal, Tenant: "default", Profile: "prof",
		})
		if !core.IsCode(err, core.CodePrivacyViolation) {
			t.Errorf("%s: %v", model, err)
		}
	}
	if h.calls() != 0 {
		t.Errorf("%d calls", h.calls())
	}
}

// The ceiling check of the node (flo's ModelPrivacy) and kit's local-only
// resolution decide the same set of models: kit is the source of truth.
func TestLLMLocalOnlyAgreesWithModelPrivacy(t *testing.T) {
	lh := localHostsOf(t)
	for _, model := range []string{
		"ollama://gemma",
		"ollama://localhost:11434/gemma",
		"ollama://127.0.0.1:11434/gemma",
		"ollama://[::1]:11434/gemma",
		"ollama://10.1.2.3:11434/gemma",
		"ollama://192.168.0.9:11434/gemma",
		"ollama://172.20.0.1:11434/gemma",
		"ollama://[fd00::1]:11434/gemma",
		"ollama://gpu.internal:11434/gemma",
		"ollama://100.64.1.2:11434/gemma",
		"ollama://box.ts.net:11434/gemma",
		"ollama://100.200.1.2:11434/gemma",
		"ollama://printer.local:11434/gemma",
		"ollama://foo.localhost:11434/gemma",
		"ollama://localhost.example.com:11434/gemma",
		"ollama://ollama.example.com:11434/gemma",
		"ollama://8.8.8.8:11434/gemma",
		"ollama://169.254.1.1:11434/gemma",
		"ollama://m?base_url=http://localhost:9999",
		"ollama://m?base_url=http://printer.local:9999",
		"ollama://m?base_url=http://foo.localhost:9999",
		"ollama://m?base_url=https://api.example.com",
		"ollama://localhost:11434/m?base_url=https://api.example.com",
		"ollama://gpu.internal:11434/m?base_url=https://api.example.com",
		"fakecloud://m",
		"fakecloud://localhost:8080/m",
	} {
		lvl, err := node.ModelPrivacy(model, lh)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		_, kerr := llm.Resolve(model, llm.Hermetic(), llm.LocalOnly(llm.AllowHosts(listedHosts...)))
		if (lvl == node.PrivacyLocal) != (kerr == nil) {
			t.Errorf("%s: flo says %s; kit local-only resolve: %v", model, lvl, kerr)
		}
	}
}

func TestLLMStructuredData(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply(`{"label":"pass"}`, 20, 5)}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	in := genIn("ollama://m", nil)
	in["schema"] = map[string]any{
		"type": "object", "required": []any{"label"}, "additionalProperties": false,
		"properties": map[string]any{"label": map[string]any{"type": "string"}},
	}
	res := gen(t, llmGen(t, h, &storeSecrets{t: t}, nil), b, specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	out := res.Output.(map[string]any)
	if d, _ := out["data"].(map[string]any); d["label"] != "pass" {
		t.Errorf("data %v", out["data"])
	}
	if !strings.Contains(h.bodies[0], `"format"`) {
		t.Errorf("schema not sent: %s", h.bodies[0])
	}
	if res.Cost.Tokens != 25 {
		t.Errorf("cost %+v", res.Cost)
	}
}

// Output that never matches the schema fails output_invalid, and the spend of
// every attempt is on the result.
func TestLLMStructuredInvalidKeepsTheCost(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply(`{"nope":1}`, 10, 10)}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	in := genIn("ollama://m", nil)
	in["schema"] = map[string]any{
		"type": "object", "required": []any{"label"},
		"properties": map[string]any{"label": map[string]any{"type": "string"}},
	}
	res := gen(t, llmGen(t, h, &storeSecrets{t: t}, nil), b, specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid {
		t.Fatalf("%+v", res)
	}
	if h.calls() < 2 || res.Cost.Tokens != int64(20*h.calls()) {
		t.Errorf("calls %d cost %+v", h.calls(), res.Cost)
	}
}

func TestLLMProviderErrorIsUpstreamFailed(t *testing.T) {
	h := &fakeHTTP{reply: func(*http.Request) (int, string) { return 500, `{"error":"boom"}` }}
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, llmGen(t, h, &storeSecrets{t: t}, nil), b, specrun.PrivacyLocalOnly, genIn("ollama://m", nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUpstreamFailed {
		t.Fatalf("%+v", res)
	}
}

func TestLLMCancelledContextIsNotClassified(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
	g := llmGen(t, h, &storeSecrets{t: t}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := g.Generate(ctx, builtin.GenerateRequest{
		Prompt: loadPrompt(t, "template: hi\n"), Model: "ollama://m", Level: node.PrivacyLocal,
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestLLMBadModelURI(t *testing.T) {
	// The key is there: the scheme is what is wrong.
	keys := &storeSecrets{t: t, vals: map[string]string{"nosuchscheme_api_key": "k"}}
	g := llmGen(t, &fakeHTTP{reply: ollamaReply("x", 1, 1)}, keys, nil)
	_, _, err := g.Generate(context.Background(), builtin.GenerateRequest{Prompt: loadPrompt(t, "template: hi\n"), Model: "gemma"})
	if !core.IsCode(err, core.CodeInputInvalid) {
		t.Fatalf("%v", err)
	}
	_, _, err = g.Generate(context.Background(), builtin.GenerateRequest{Prompt: loadPrompt(t, "template: hi\n"), Model: "nosuchscheme://m", Tenant: "default", Profile: "prof"})
	if !core.IsCode(err, core.CodeInputInvalid) {
		t.Fatalf("%v", err)
	}
}

func TestGeneratorForLLM(t *testing.T) {
	g, err := builtin.GeneratorFor(builtin.BackendLLM, builtin.LLMDeps{})
	if err != nil || g == nil {
		t.Fatalf("%v %v", g, err)
	}
	if _, err := builtin.GeneratorFor(builtin.BackendLLM, builtin.LLMDeps{LocalHosts: []string{"bad host"}}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("a bad privacy.local_hosts entry must fail at startup: %v", err)
	}
}

func loadPrompt(t *testing.T, body string) *prompts.Prompt {
	t.Helper()
	ref, err := prompts.ParseRef("brief@1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := prompts.Parse(ref, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
