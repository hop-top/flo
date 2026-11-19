package builtin_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/schema"
	"hop.top/flo/internal/prompts"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

func bundleWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, "prompts", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// listedHosts is what the tests list as privacy.local_hosts.
var listedHosts = []string{"gpu.internal", "100.64.0.0/10"}

func localHostsOf(t *testing.T) *node.LocalHosts {
	t.Helper()
	lh, err := node.NewLocalHosts(listedHosts)
	if err != nil {
		t.Fatal(err)
	}
	return lh
}

// gen runs flo/ai.generate@1 with a bundle and a ceiling, as the executor
// fills them.
func gen(t *testing.T, g builtin.Generator, bundle string, ceiling specrun.Privacy, in map[string]any) core.Result {
	t.Helper()
	r, _ := std(t)
	if err := builtin.RegisterGenerate(r, g, prompts.Loader{}, localHostsOf(t)); err != nil {
		t.Fatal(err)
	}
	n, _ := r.Lookup("flo/ai.generate@1")
	full, err := schema.ApplyInputs(n.Spec().Inputs, in)
	if err != nil {
		t.Fatal(err)
	}
	res, xerr := n.Execute(context.Background(), core.Invocation{
		Tenant: "default", Run: core.RunContext{ID: "run_1"}, StepID: "s", Node: n.Spec(), In: full,
		Bundle: bundle, Privacy: ceiling, RunAs: "prof",
	})
	if xerr != nil {
		t.Fatal(xerr)
	}
	if res.Outcome == core.OutcomeOK {
		if cerr := schema.CheckOutput(n.Spec(), res.Output); cerr != nil {
			t.Fatalf("output must match the node's own schema: %v", cerr)
		}
	}
	return res
}

func genIn(model string, ctx map[string]any) map[string]any {
	in := map[string]any{"prompt": "brief@1", "model": model}
	if ctx != nil {
		in["context"] = ctx
	}
	return in
}

const briefPrompt = "system: s\ntemplate: |\n  Meeting {{ .title }}: {{ join \", \" .people }}\n"

func TestGenerateTemplateRenders(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": briefPrompt})
	g, err := builtin.GeneratorFor(builtin.BackendTemplate, builtin.LLMDeps{})
	if err != nil {
		t.Fatal(err)
	}
	res := gen(t, g, b, specrun.PrivacyLocalOnly, genIn("ollama://gemma4:e4b",
		map[string]any{"title": "sync", "people": []any{"ann", "bo"}}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	out := res.Output.(map[string]any)
	if out["text"] != "Meeting sync: ann, bo\n" {
		t.Errorf("text %q", out["text"])
	}
	pm := out["prompt"].(map[string]any)
	if pm["name"] != "brief" || pm["version"] != 1 || len(pm["hash"].(string)) != 64 {
		t.Errorf("prompt metadata %v", pm)
	}
	if _, has := out["data"]; has {
		t.Error("data without schema")
	}
	if !res.Cost.IsZero() {
		t.Errorf("template cost %+v", res.Cost)
	}
}

func TestGenerateHashRecordedFollowsTheFile(t *testing.T) {
	a := bundleWith(t, map[string]string{"brief@1.yaml": briefPrompt})
	b := bundleWith(t, map[string]string{"brief@1.yaml": briefPrompt + "  more\n"})
	in := genIn("ollama://m", map[string]any{"title": "t", "people": []any{}})
	ha := gen(t, builtin.TemplateGenerator{}, a, specrun.PrivacyLocalOnly, in).Output.(map[string]any)["prompt"].(map[string]any)["hash"]
	hb := gen(t, builtin.TemplateGenerator{}, b, specrun.PrivacyLocalOnly, in).Output.(map[string]any)["prompt"].(map[string]any)["hash"]
	if ha == hb {
		t.Error("two prompt texts, one hash")
	}
}

func TestGenerateMissingKeyFails(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": briefPrompt})
	res := gen(t, builtin.TemplateGenerator{}, b, specrun.PrivacyLocalOnly, genIn("ollama://m", map[string]any{"title": "t"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
		t.Fatalf("%+v", res)
	}
}

func TestGenerateMissingPrompt(t *testing.T) {
	res := gen(t, builtin.TemplateGenerator{}, t.TempDir(), specrun.PrivacyLocalOnly, genIn("ollama://m", nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeNotFound {
		t.Fatalf("%+v", res)
	}
}

func TestGenerateEmptyTextIsAFailure(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: \"{{ .x }}\"\n"})
	for _, x := range []string{"", "  \n\t"} {
		res := gen(t, builtin.TemplateGenerator{}, b, specrun.PrivacyLocalOnly, genIn("ollama://m", map[string]any{"x": x}))
		if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeNodeReportedFailure {
			t.Fatalf("x=%q: %+v", x, res)
		}
	}
}

func TestGenerateSchemaNeedsAModel(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	in := genIn("ollama://m", nil)
	in["schema"] = map[string]any{"type": "object"}
	res := gen(t, builtin.TemplateGenerator{}, b, specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature {
		t.Fatalf("%+v", res)
	}
}

func TestGenerateRuntimePrivacyCeiling(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	for _, c := range []struct {
		name    string
		model   string
		ceiling specrun.Privacy
		refused bool
	}{
		{"cloud under local-only", "anthropic://claude-sonnet-5-5", specrun.PrivacyLocalOnly, true},
		{"cloud, no ceiling set", "anthropic://claude-sonnet-5-5", "", true},
		{"public ollama host under local-only", "ollama://ollama.example.com:11434/gemma", specrun.PrivacyLocalOnly, true},
		{"public openai host under local-only", "openai://api.example.com:8080/m", specrun.PrivacyLocalOnly, true},
		{"base_url past a local host under local-only", "openai://localhost:8000/m?base_url=https://api.openai.com/v1", specrun.PrivacyLocalOnly, true},
		{"unlisted tailnet name under local-only", "ollama://box.ts.net:11434/gemma", specrun.PrivacyLocalOnly, true},
		{"not a URI under local-only", "gemma", specrun.PrivacyLocalOnly, true},
		{"dotted ollama name is the local daemon", "ollama://llama3.1", specrun.PrivacyLocalOnly, false},
		{"lmstudio default under local-only", "lmstudio://qwen", specrun.PrivacyLocalOnly, false},
		{"base_url to localhost under local-only", "openai://m?base_url=http://localhost:8000/v1", specrun.PrivacyLocalOnly, false},
		{"empty authority is not runnable", "ollama:///llama3.1", specrun.PrivacyLocalOnly, true},
		{"portless localhost is not runnable", "ollama://localhost/llama3.1", specrun.PrivacyLocalOnly, true},
		{"not runnable, even under cloud-allowed", "ollama:///llama3.1", specrun.PrivacyCloudAllowed, true},
		{"local ollama under local-only", "ollama://gemma4:e4b", specrun.PrivacyLocalOnly, false},
		{"private openai under local-only", "openai://10.0.0.7:8080/m", specrun.PrivacyLocalOnly, false},
		{"listed named host under local-only", "openai://gpu.internal:8000/m", specrun.PrivacyLocalOnly, false},
		{"listed tailnet CIDR under local-only", "ollama://100.64.9.9:11434/gemma", specrun.PrivacyLocalOnly, false},
		{"listed host does not rescue a public base_url", "openai://gpu.internal:8000/m?base_url=https://api.openai.com/v1", specrun.PrivacyLocalOnly, true},
		{"cloud under cloud-allowed", "anthropic://claude-sonnet-5-5", specrun.PrivacyCloudAllowed, false},
		{"public host under cloud-allowed", "ollama://ollama.example.com:11434/gemma", specrun.PrivacyCloudAllowed, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := gen(t, builtin.TemplateGenerator{}, b, c.ceiling, genIn(c.model, nil))
			if c.refused {
				if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
					t.Fatalf("not refused: %+v", res)
				}
				if !strings.Contains(res.Err.Suggestion, "ollama://<model>") {
					t.Errorf("suggestion lacks the local spelling: %q", res.Err.Suggestion)
				}
				return
			}
			if res.Outcome != core.OutcomeOK {
				t.Fatalf("refused: %+v", res.Err)
			}
		})
	}
}

// The ceiling is checked before the backend is called: a refused step sends
// nothing.
func TestGenerateRefusalCallsNoBackend(t *testing.T) {
	called := false
	g := genFunc(func(context.Context, builtin.GenerateRequest) (builtin.Generation, core.Cost, error) {
		called = true
		return builtin.Generation{Text: "x"}, core.Cost{}, nil
	})
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, g, b, specrun.PrivacyLocalOnly, genIn("anthropic://c", nil))
	if called || res.Err == nil || res.Err.Code != core.CodePrivacyViolation {
		t.Fatalf("called=%v %+v", called, res)
	}
}

type genFunc func(context.Context, builtin.GenerateRequest) (builtin.Generation, core.Cost, error)

func (f genFunc) Generate(ctx context.Context, r builtin.GenerateRequest) (builtin.Generation, core.Cost, error) {
	return f(ctx, r)
}

func TestGenerateWithoutBackendFails(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	g, err := builtin.GeneratorFor("", builtin.LLMDeps{})
	if err != nil || g != nil {
		t.Fatalf("%v %v", g, err)
	}
	res := gen(t, g, b, specrun.PrivacyLocalOnly, genIn("ollama://m", nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature {
		t.Fatalf("%+v", res)
	}
}

func TestGeneratorFor(t *testing.T) {
	if g, err := builtin.GeneratorFor("template", builtin.LLMDeps{}); err != nil || g == nil {
		t.Errorf("template: %v %v", g, err)
	}
	if _, err := builtin.GeneratorFor("nope", builtin.LLMDeps{}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("unknown: %v", err)
	}
}

func TestGenerateBackendTextBlankFails(t *testing.T) {
	g := genFunc(func(context.Context, builtin.GenerateRequest) (builtin.Generation, core.Cost, error) {
		return builtin.Generation{Text: " "}, core.Cost{}, nil
	})
	b := bundleWith(t, map[string]string{"brief@1.yaml": "template: hi\n"})
	res := gen(t, g, b, specrun.PrivacyCloudAllowed, genIn("anthropic://c", nil))
	if res.Err == nil || res.Err.Code != core.CodeNodeReportedFailure {
		t.Fatalf("%+v", res)
	}
}
