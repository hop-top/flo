package builtin_test

import (
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	specrun "hop.top/flo/spec/run"
)

func policyCls(t *testing.T, h *fakeHTTP, s core.Secrets, allow, deny []string, engine string) builtin.Classifier {
	t.Helper()
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: s, LocalHosts: listedHosts, JudgeEngine: engine, Jev: fakeJev, Allow: allow, Deny: deny,
	})
	if err != nil || c == nil {
		t.Fatalf("judge backend: %v %v", c, err)
	}
	return c
}

// Neither list set allows every model.
func TestPolicyNeitherListAllowsAll(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	res := classifyAs(t, policyCls(t, h, withT(t, jevKeys()), nil, nil, ""), specrun.PrivacyCloudAllowed, listIn("ollama://gemma4:e4b", ""))
	if res.Outcome != core.OutcomeOK || out(res)["model"] != "ollama://gemma4:e4b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
}

// Deny wins, for every engine; a refusal sends nothing and reads no secret.
func TestPolicyDenyRefusesBeforeAnyRequestOrSecret(t *testing.T) {
	for _, engine := range []string{"", builtin.EngineLocal, builtin.EngineJev, builtin.EngineAuto} {
		for _, c := range []struct {
			name        string
			allow, deny []string
			model       any
		}{
			{"deny only", nil, []string{"ollama://gemma4*"}, "ollama://gemma4:e4b"},
			{"deny wins", []string{"*"}, []string{"ollama://gemma4:e4b"}, "ollama://gemma4:e4b"},
			{"deny glob wins over allow glob", []string{"ollama://*"}, []string{"ollama://*:e4b"}, "ollama://gemma4:e4b"},
			{"deny star", nil, []string{"*"}, "ollama://gemma4:e4b"},
			{"deny cloud", nil, []string{"fakecloud://*"}, []any{"fakecloud://big"}},
			{"deny jev", nil, []string{"jev"}, []any{"jev"}},
			{"allowed but not listed", []string{"ollama://other*"}, []string{"ollama://gemma4*"}, "ollama://gemma4:e4b"},
		} {
			t.Run(engine+"/"+c.name, func(t *testing.T) {
				if _, isList := c.model.([]any); isList && engine == builtin.EngineLocal && c.name == "deny jev" {
					t.Skip("engine local with only jev is input_invalid")
				}
				_, h := newWorld(true, "gemma4:e4b")
				s := withT(t, &storeSecrets{vals: map[string]string{"openrouter_api_key": "k", "fakecloud_api_key": "k"}})
				// Jev is denied as well: auto and engine jev would fall to it.
				deny := append([]string{"jev"}, c.deny...)
				res := classifyAs(t, policyCls(t, h, s, c.allow, deny, engine), specrun.PrivacyCloudAllowed, listIn(c.model, engine))
				if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
					t.Fatalf("%+v", res)
				}
				noTraffic(t, h, s)
			})
		}
	}
}

// Deny cannot be dodged by pointing the model at another endpoint: the step is
// refused before any request or secret read.
func TestPolicyDenyHoldsAtAnyEndpoint(t *testing.T) {
	for _, model := range []string{
		"ollama://gemma4:e4b?base_url=http://other.example",
		"ollama://gpu.internal:11434/gemma4:e4b",
		"ollama://[::1]:11434/gemma4:e4b",
		"openai://gpu.internal:11434/gemma4:e4b",
	} {
		for _, deny := range [][]string{{"ollama://gemma4*", "jev"}, {"gemma4*", "jev"}} {
			_, h := newWorld(true, "gemma4:e4b")
			s := withT(t, jevKeys())
			res := classifyAs(t, policyCls(t, h, s, nil, deny, ""), specrun.PrivacyCloudAllowed, listIn(model, ""))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation || !strings.Contains(res.Err.Message, "denied") {
				t.Fatalf("%s %v: %+v", model, deny, res)
			}
			noTraffic(t, h, s)
		}
	}
}

func TestPolicyDenyMessageSaysDenied(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	res := classifyAs(t, policyCls(t, h, withT(t, jevKeys()), nil, []string{"ollama://gemma4*", "jev"}, ""), specrun.PrivacyCloudAllowed, listIn("ollama://gemma4:e4b", ""))
	text := res.Err.Message + " " + res.Err.Suggestion
	for _, want := range []string{"denied", "ai.models.deny", "FLO_AI_MODELS_DENY"} {
		if !strings.Contains(text, want) {
			t.Errorf("error lacks %q: %s", want, text)
		}
	}
}

// Only deny set: everything else runs; in a list a denied entry is skipped.
func TestPolicyDenyOnlySkipsDeniedEntry(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b", "gemma4:e2b")
	res := classifyAs(t, policyCls(t, h, withT(t, jevKeys()), nil, []string{"ollama://*:e4b"}, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://gemma4:e4b", "ollama://gemma4:e2b"}, ""))
	if res.Outcome != core.OutcomeOK || out(res)["model"] != "ollama://gemma4:e2b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
}

// Both set: allowed and not denied.
func TestPolicyBoth(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b", "gemma4:e2b")
	res := classifyAs(t, policyCls(t, h, withT(t, jevKeys()), []string{"ollama://gemma4*"}, []string{"ollama://gemma4:e4b"}, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://gemma4:e4b", "ollama://gemma4:e2b"}, ""))
	if res.Outcome != core.OutcomeOK || out(res)["model"] != "ollama://gemma4:e2b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
}

// The implicit Jev of auto is a model like any other: deny reaches it.
func TestPolicyDenyJevStopsTheAutoFallback(t *testing.T) {
	_, h := newWorld(false)
	s := withT(t, jevKeys())
	res := classifyAs(t, policyCls(t, h, s, nil, []string{"jev"}, ""), specrun.PrivacyCloudAllowed, listIn("ollama://gemma4:e4b", ""))
	if res.Outcome != core.OutcomeFailed || h.count(jevURL) != 0 || len(s.got) != 0 {
		t.Fatalf("%+v calls %v secrets %v", res, h.urls(), s.got)
	}
}

func TestPolicyGenerateDeny(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	h := &fakeHTTP{reply: tagsOrChat("gemma4:e4b")}
	s := &storeSecrets{t: t}
	g, err := builtin.NewLLMGenerator(builtin.LLMDeps{HTTP: h, Secrets: s, LocalHosts: listedHosts, Deny: []string{"ollama://gemma4*"}})
	if err != nil {
		t.Fatal(err)
	}
	res := gen(t, g, b, specrun.PrivacyCloudAllowed, genList("ollama://gemma4:e4b"))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation || h.calls() != 0 || len(s.got) != 0 {
		t.Fatalf("%+v calls %v", res, h.urls())
	}
}

// A malformed list is a usage error when the backend is built.
func TestPolicyMalformedListsFailAtBuild(t *testing.T) {
	for _, d := range []builtin.LLMDeps{
		{Allow: []string{"ollama://*:11434/m"}}, {Deny: []string{"ollama://*:11434/m"}}, {Allow: []string{"nope"}},
	} {
		if _, err := builtin.NewLLMGenerator(d); !core.IsCode(err, core.CodeUsage) {
			t.Errorf("%v: %v", d, err)
		}
		if _, err := builtin.ClassifierFor(builtin.BackendJudge, d); !core.IsCode(err, core.CodeUsage) {
			t.Errorf("%v: %v", d, err)
		}
	}
}

// A lone local model is probed too: a missing one fails with the pull hint.
func TestLoneOllamaModelIsProbed(t *testing.T) {
	for _, c := range []struct {
		name  string
		model any
	}{
		{"string", "ollama://gemma4:e4b"},
		{"one-entry list", []any{"ollama://gemma4:e4b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := newWorld(true, "other:1b")
			s := withT(t, jevKeys())
			res := classifyAs(t, listCls(t, h, s, allowAny, builtin.EngineLocal), specrun.PrivacyLocalOnly, listIn(c.model, builtin.EngineLocal))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUpstreamFailed {
				t.Fatalf("%+v", res)
			}
			text := res.Err.Message + " " + res.Err.Suggestion
			for _, want := range []string{"not detected", "ollama pull gemma4:e4b"} {
				if !strings.Contains(text, want) {
					t.Errorf("error lacks %q: %s", want, text)
				}
			}
			if h.count("/api/tags") != 1 || h.count("/api/chat") != 0 || len(s.got) != 0 {
				t.Errorf("calls %v secrets %v", h.urls(), s.got)
			}
		})
	}
}

func TestLoneOllamaModelDaemonDown(t *testing.T) {
	_, h := newWorld(false)
	res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, builtin.EngineLocal), specrun.PrivacyLocalOnly, listIn("ollama://gemma4:e4b", builtin.EngineLocal))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUpstreamFailed || h.count("/api/chat") != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestLoneOllamaModelDetectedRuns(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, builtin.EngineLocal), specrun.PrivacyLocalOnly, listIn([]any{"ollama://gemma4:e4b"}, builtin.EngineLocal))
	if res.Outcome != core.OutcomeOK || h.count("/api/tags") != 1 || h.count("/api/chat") != 1 {
		t.Fatalf("%+v calls %v", res.Err, h.urls())
	}
}

// The probe is remembered per run: a second step does not ask again.
func TestLoneOllamaProbeIsCachedPerRun(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	c := listCls(t, h, withT(t, jevKeys()), allowAny, builtin.EngineLocal)
	for i := 0; i < 2; i++ {
		if res := classifyAs(t, c, specrun.PrivacyLocalOnly, listIn("ollama://gemma4:e4b", builtin.EngineLocal)); res.Outcome != core.OutcomeOK {
			t.Fatalf("%+v", res.Err)
		}
	}
	if h.count("/api/tags") != 1 {
		t.Errorf("calls %v", h.urls())
	}
}

// A lone local model of a scheme that cannot be detected is not probed: it
// runs as written (the call's own failure, if any).
func TestLoneNonOllamaLocalModelIsNotProbed(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, builtin.EngineLocal), specrun.PrivacyLocalOnly, listIn("lmstudio://m", builtin.EngineLocal))
	if h.count("/api/tags") != 0 {
		t.Errorf("calls %v", h.urls())
	}
}
