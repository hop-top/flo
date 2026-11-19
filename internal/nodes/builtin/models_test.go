package builtin_test

import (
	"net/http"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	specrun "hop.top/flo/spec/run"
	"hop.top/kit/go/ai/judge"
)

const jevModel = "jev://typesafe/jev-1.14"

// allowAny is the allow-list of tests that are not about the allow-list.
var allowAny = []string{"*"}

func TestModelAllowed(t *testing.T) {
	for _, c := range []struct {
		name  string
		allow []string
		model string
		want  bool
	}{
		{"exact", []string{"ollama://gemma4:e4b"}, "ollama://gemma4:e4b", true},
		{"exact other", []string{"ollama://gemma4:e4b"}, "ollama://gemma4:e2b", false},
		{"exact is not a prefix", []string{"ollama://gemma4"}, "ollama://gemma4:e4b", false},
		{"glob", []string{"ollama://qwen3*"}, "ollama://qwen3:4b-instruct-2507-q4_K_M", true},
		{"glob matches the bare prefix", []string{"ollama://qwen3*"}, "ollama://qwen3", true},
		{"glob other", []string{"ollama://qwen3*"}, "ollama://gemma4:e4b", false},
		{"glob host form", []string{"ollama://gpu.internal:11434/*"}, "ollama://gpu.internal:11434/big:70b", true},
		{"glob stops at the query", []string{"ollama://qwen3*"}, "ollama://qwen3?base_url=https://evil.example.com", false},
		{"glob in the query is not a glob", []string{"ollama://qwen3?base_url=http://gpu.internal*"}, "ollama://qwen3?base_url=http://gpu.internal:1", false},
		{"star alone", []string{"*"}, "anthropic://claude-haiku-5-5", true},
		{"star in the middle", []string{"ollama://*3:4b"}, "ollama://qwen3:4b", true},
		{"star in the middle other", []string{"ollama://*3:4b"}, "ollama://qwen3:8b", false},
		{"question mark", []string{"ollama://qwen?:4b"}, "ollama://qwen3:4b", true},
		{"question mark is one char", []string{"ollama://qwen?:4b"}, "ollama://qwen33:4b", false},
		{"class", []string{"ollama://qwen[23]:4b"}, "ollama://qwen3:4b", true},
		{"class other", []string{"ollama://qwen[23]:4b"}, "ollama://qwen4:4b", false},
		{"negated class", []string{"ollama://qwen[^2]:4b"}, "ollama://qwen2:4b", false},
		{"two stars", []string{"ollama://qwen3:*-instruct-*"}, "ollama://qwen3:4b-instruct-2507-q4_K_M", true},
		{"two stars other", []string{"ollama://qwen3:*-instruct-*"}, "ollama://qwen3:4b-base-2507", false},
		{"jev glob", []string{"jev://typesafe/*"}, "jev://typesafe/jev-2", true},
		{"jev glob matches the shorthand", []string{"jev://typesafe/*"}, "jev", true},
		{"jev glob other", []string{"jev://other/*"}, "jev", false},
		{"bad glob matches nothing", []string{"ollama://qwen[3"}, "ollama://qwen3", false},
		{"question mark glob keeps the query", []string{"ollama://qwen?:4b?base_url=http://gpu.internal:1"}, "ollama://qwen3:4b?base_url=http://gpu.internal:1", true},
		{"question mark glob keeps the query other", []string{"ollama://qwen?:4b?base_url=http://gpu.internal:1"}, "ollama://qwen3:4b?base_url=http://evil.example", false},
		{"class past a slash", []string{"ollama://a[/]b"}, "ollama://a/b", false},
		{"question mark does not cross a slash", []string{"ollama://qwen3?x"}, "ollama://qwen3/x", false},
		{"star does not cross a slash in the middle", []string{"ollama://qwen*:4b"}, "ollama://qwen/x:4b", false},
		{"host smuggled through a mid glob", []string{"ollama://qwen*:4b"}, "ollama://qwen3.attacker.example:11434/m:4b", false},
		{"userinfo through a mid glob", []string{"ollama://qwen*b"}, "ollama://qwen3:4b@attacker.example:11434/b", false},
		{"dot dot through a mid glob", []string{"ollama://qwen*b"}, "ollama://qwen..b", false},
		{"base_url smuggled past a mid glob", []string{"ollama://qwen*b"}, "ollama://qwen3:4b?base_url=http://evil.example", false},
		{"any entry", []string{"x://a", "ollama://m"}, "ollama://m", true},
		{"jev shorthand entry", []string{"jev"}, jevModel, true},
		{"jev shorthand model", []string{jevModel}, "jev", true},
		{"jev shorthand both", []string{"jev"}, "jev", true},
		{"jev other model", []string{"jev"}, "jev://typesafe/jev-2", false},
		// Bypasses: a glob reaches the model name only, never the endpoint.
		{"host smuggled through the glob", []string{"ollama://qwen3*"}, "ollama://qwen3.attacker.example:11434/model", false},
		{"host smuggled with userinfo", []string{"ollama://qwen3*"}, "ollama://qwen3:4b@attacker.example:11434/m", false},
		{"userinfo in the name", []string{"ollama://qwen3*"}, "ollama://qwen3@attacker.example", false},
		{"path segment past the glob", []string{"ollama://qwen3*"}, "ollama://qwen3/x", false},
		{"dot dot past the glob", []string{"ollama://qwen3*"}, "ollama://qwen3..%2f", false},
		{"base_url smuggled past a host entry", []string{"ollama://gpu.internal:11434/*"}, "ollama://gpu.internal:11434/m?base_url=http://evil.example", false},
		{"other host with the entry as prefix", []string{"ollama://gpu.internal:11434/*"}, "ollama://gpu.internal.evil.example:11434/m", false},
		{"host entry does not match a hostless model", []string{"ollama://gpu.internal:11434/*"}, "ollama://m", false},
		{"hostless entry does not match a host model", []string{"ollama://*"}, "ollama://evil.example:11434/m", false},
		{"scheme case", []string{"ollama://qwen3*"}, "OLLAMA://qwen3:4b", false},
		{"scheme prefix glob", []string{"oll*"}, "ollama://qwen3", false},
		{"exact with query matches itself", []string{"ollama://m?base_url=http://gpu.internal:1"}, "ollama://m?base_url=http://gpu.internal:1", true},
		{"exact without query does not match a query", []string{"ollama://m"}, "ollama://m?base_url=http://evil.example", false},
		{"namespaced model under a glob", []string{"ollama://hf.co/user/*"}, "ollama://hf.co/user/model:q4", true},
		{"empty list", nil, "ollama://m", false},
		{"blank entries are none", []string{"", " "}, "ollama://m", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := builtin.ModelAllowed(c.allow, c.model); got != c.want {
				t.Errorf("ModelAllowed(%v, %q) = %v", c.allow, c.model, got)
			}
		})
	}
}

// listCls is the judge backend of the list tests: the Jev factory records the
// model it is asked for.
func listCls(t *testing.T, h *fakeHTTP, s core.Secrets, allow []string, engine string) builtin.Classifier {
	t.Helper()
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: s, LocalHosts: listedHosts, JudgeEngine: engine, Jev: fakeJev, Allow: allow,
	})
	if err != nil || c == nil {
		t.Fatalf("judge backend: %v %v", c, err)
	}
	return c
}

func listIn(models any, engine string) map[string]any {
	in := engineIn("", engine)
	in["model"] = models
	return in
}

func out(res core.Result) map[string]any { return res.Output.(map[string]any) }

// sees fails when a recorded request or secret read is not expected.
func noTraffic(t *testing.T, h *fakeHTTP, s *storeSecrets) {
	t.Helper()
	if h.calls() != 0 || len(s.got) != 0 {
		t.Errorf("refused call sent %v and read %v", h.urls(), s.got)
	}
}

func TestListFallsBackWhenTheLocalModelIsMissing(t *testing.T) {
	_, h := newWorld(true, "other:1b")
	s := withT(t, jevKeys())
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://gemma4:e4b", "jev"}, ""))
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "jev" || out(res)["model"] != jevModel {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if h.count("/api/chat") != 0 || h.count(jevURL) != 1 || h.count("/api/tags") != 1 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestListFirstDetectedWinsAndRecordsTheModel(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b", "gemma4:e2b")
	s := withT(t, jevKeys())
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://gemma4:e2b", "ollama://gemma4:e4b", "jev"}, ""))
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "local" || out(res)["model"] != "ollama://gemma4:e2b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if len(s.got) != 0 || h.count(jevURL) != 0 {
		t.Errorf("a local pick read %v, calls %v", s.got, h.urls())
	}
	if !strings.Contains(h.bodies[len(h.bodies)-1], "gemma4:e2b") {
		t.Errorf("chat body %s", h.bodies[len(h.bodies)-1])
	}
}

func TestListJevWithoutKeyFallsToTheNext(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	s := withT(t, &storeSecrets{})
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"jev", "ollama://gemma4:e4b"}, ""))
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "local" || out(res)["model"] != "ollama://gemma4:e4b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if h.count(jevURL) != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestListCeilingSkipsCloudEntries(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	s := withT(t, jevKeys())
	for _, level := range []specrun.Privacy{specrun.PrivacyLocalOnly, ""} {
		res := classifyAs(t, listCls(t, h, s, allowAny, ""), level,
			listIn([]any{"jev", "fakecloud://big", "ollama://gemma4:e4b"}, ""))
		if res.Outcome != core.OutcomeOK || out(res)["model"] != "ollama://gemma4:e4b" {
			t.Fatalf("%q: %+v %v", level, res.Err, res.Output)
		}
	}
	if len(s.got) != 0 {
		t.Errorf("a ceiling-refused entry read %v", s.got)
	}
}

func TestListNoneUsableListsEveryReason(t *testing.T) {
	_, h := newWorld(false)
	s := withT(t, &storeSecrets{})
	allow := []string{"ollama://*", "fakecloud://*", "jev"}
	res := classifyAs(t, listCls(t, h, s, allow, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://gemma4:e4b", "fakecloud://big", "jev", "ollama://other:1b?base_url=http://x.example"}, ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{
		"ollama://gemma4:e4b", "not detected", "fakecloud://big", "fakecloud_api_key", jevModel, "openrouter_api_key",
		"ollama://other:1b", "not allowed",
	} {
		if !strings.Contains(res.Err.Message, want) {
			t.Errorf("message lacks %q: %s", want, res.Err.Message)
		}
	}
	if h.count("/api/chat") != 0 || h.count(jevURL) != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestListNoneUsableCodeByTheMostSignificantReason(t *testing.T) {
	for _, c := range []struct {
		name   string
		level  specrun.Privacy
		models []any
		allow  []string
		code   core.Code
		want   string
	}{
		{"ceiling beats the rest", specrun.PrivacyLocalOnly, []any{"ollama://gemma4:e4b", "jev"}, allowAny, core.CodePrivacyViolation, "ceiling"},
		{"no key beats not detected", specrun.PrivacyCloudAllowed, []any{"ollama://gemma4:e4b", "jev"}, allowAny, core.CodeSecretNotFound, "no key"},
		{"not detected alone", specrun.PrivacyCloudAllowed, []any{"ollama://gemma4:e4b", "ollama://gemma4:e2b"}, allowAny, core.CodeUpstreamFailed, "not detected"},
		{"not detected beats not allowed", specrun.PrivacyCloudAllowed, []any{"ollama://gemma4:e4b", "ollama://nope"}, []string{"ollama://gemma4*"}, core.CodeUpstreamFailed, "not allowed"},
		{"all not allowed", specrun.PrivacyCloudAllowed, []any{"ollama://a", "jev"}, []string{"ollama://z"}, core.CodePrivacyViolation, "not allowed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := newWorld(false)
			res := classifyAs(t, listCls(t, h, withT(t, &storeSecrets{}), c.allow, ""), c.level, listIn(c.models, ""))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != c.code {
				t.Fatalf("%+v", res)
			}
			if !strings.Contains(res.Err.Message, c.want) {
				t.Errorf("message lacks %q: %s", c.want, res.Err.Message)
			}
		})
	}
}

// A call that fails on the chosen model is reported, not retried on the next.
func TestListCallErrorIsNotRetried(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b", "gemma4:e2b")
	prev := h.reply
	h.reply = func(r *http.Request) (int, string) {
		if r.URL.Path == "/api/chat" {
			return 500, `{"error":"boom"}`
		}
		return prev(r)
	}
	res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://gemma4:e4b", "ollama://gemma4:e2b", "jev"}, ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUpstreamFailed {
		t.Fatalf("%+v", res)
	}
	if h.count("/api/chat") != 1 || h.count(jevURL) != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

// An explicit cloud model under auto runs as named; Jev never stands in.
func TestAutoHonorsAnExplicitCloudModel(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	prev := h.reply
	h.reply = func(r *http.Request) (int, string) {
		if r.URL.Host == "api.fakecloud.example" {
			return cloudReply(verdict("breaking", 0.9, "named"), 10, 2)(r)
		}
		return prev(r)
	}
	s := withT(t, &storeSecrets{vals: map[string]string{"openrouter_api_key": "or-s3cret", "fakecloud_api_key": "fc-key"}})
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed, listIn("fakecloud://big", ""))
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "local" || out(res)["model"] != "fakecloud://big" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if h.count(jevURL) != 0 || h.count("/api/tags") != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestAutoExplicitCloudModelWithoutItsKeyIsNotSwappedForJev(t *testing.T) {
	_, h := newWorld(true)
	s := withT(t, jevKeys())
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed, listIn("fakecloud://big", ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound || !strings.Contains(res.Err.Message, "fakecloud_api_key") {
		t.Fatalf("%+v", res)
	}
	if h.count(jevURL) != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestAutoExplicitUnlinkedCloudModelRunsAsNamed(t *testing.T) {
	_, h := newWorld(true)
	s := withT(t, &storeSecrets{vals: map[string]string{"openrouter_api_key": "or-s3cret", "anthropic_api_key": "a-key"}})
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed, listIn("anthropic://claude-haiku-5-5", ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || !strings.Contains(res.Err.Suggestion, "ollama") {
		t.Fatalf("%+v", res)
	}
	if h.count(jevURL) != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

// An unset model under auto is Jev.
func TestAutoUnsetModelIsJev(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	blank := listIn("", "")
	absent := listIn("", "")
	delete(absent, "model")
	for name, in := range map[string]map[string]any{"blank": blank, "absent": absent} {
		res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, ""), specrun.PrivacyCloudAllowed, in)
		if res.Outcome != core.OutcomeOK || out(res)["engine"] != "jev" || out(res)["model"] != jevModel {
			t.Fatalf("%s: %+v %v", name, res.Err, res.Output)
		}
	}
}

func TestEngineFiltersTheList(t *testing.T) {
	models := []any{"jev", "ollama://gemma4:e4b"}
	for _, c := range []struct {
		engine, wantEngine, wantModel string
	}{
		{builtin.EngineLocal, "local", "ollama://gemma4:e4b"},
		{builtin.EngineJev, "jev", jevModel},
		{builtin.EngineAuto, "jev", jevModel},
	} {
		t.Run(c.engine, func(t *testing.T) {
			_, h := newWorld(true, "gemma4:e4b")
			res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, ""), specrun.PrivacyCloudAllowed, listIn(models, c.engine))
			if res.Outcome != core.OutcomeOK || out(res)["engine"] != c.wantEngine || out(res)["model"] != c.wantModel {
				t.Fatalf("%+v %v", res.Err, res.Output)
			}
		})
	}
}

func TestEngineLocalWithOnlyJevInTheListIsInvalid(t *testing.T) {
	_, h := newWorld(true)
	s := withT(t, jevKeys())
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed, listIn([]any{"jev"}, builtin.EngineLocal))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
		t.Fatalf("%+v", res)
	}
	noTraffic(t, h, s)
}

func TestEngineLocalWithoutAModelIsInvalid(t *testing.T) {
	_, h := newWorld(true)
	s := withT(t, jevKeys())
	res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed, listIn("", builtin.EngineLocal))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
		t.Fatalf("%+v", res)
	}
	noTraffic(t, h, s)
}

// engine jev keeps ignoring a model that is not a Jev one.
func TestEngineJevIgnoresANonJevModel(t *testing.T) {
	_, h := newWorld(false)
	res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), allowAny, ""), specrun.PrivacyCloudAllowed, listIn("ollama://gemma4:e4b", builtin.EngineJev))
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "jev" || out(res)["model"] != jevModel {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
}

// A Jev entry names the Jev model the factory builds.
func TestJevEntryNamesTheModel(t *testing.T) {
	_, h := newWorld(false)
	var asked []string
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: withT(t, jevKeys()), Allow: allowAny,
		Jev: func(key, model string, rt http.RoundTripper) (judge.Judge, error) {
			asked = append(asked, model)
			return fakeJev(key, model, rt)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"jev", "jev://typesafe/jev-2"} {
		res := classifyAs(t, c, specrun.PrivacyCloudAllowed, listIn([]any{m}, ""))
		if res.Outcome != core.OutcomeOK {
			t.Fatalf("%s: %+v", m, res.Err)
		}
	}
	if len(asked) != 2 || asked[0] != "" || asked[1] != "typesafe/jev-2" {
		t.Errorf("models asked of the factory: %q", asked)
	}
}

// A malformed entry is an error, not an unavailable model.
func TestListMalformedEntryFails(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	s := withT(t, jevKeys())
	for _, c := range []struct {
		models any
		code   core.Code
	}{
		{[]any{"ollama://gemma4:e4b", "not-a-uri"}, core.CodeInputInvalid},
		{[]any{"ollama://gemma4:e4b", "ollama:///x"}, core.CodePrivacyViolation},
		{[]any{}, core.CodeInputInvalid},
		{[]any{"ollama://gemma4:e4b", ""}, core.CodeInputInvalid},
	} {
		res := classifyAs(t, listCls(t, h, s, allowAny, ""), specrun.PrivacyCloudAllowed, listIn(c.models, ""))
		if res.Outcome != core.OutcomeFailed || res.Err.Code != c.code {
			t.Errorf("%v: %+v", c.models, res)
		}
	}
	noTraffic(t, h, s)
}

// The allow-list gate: every model, whatever the engine; a refusal sends
// nothing and reads no secret.
func TestAllowListRefusesBeforeAnyRequestOrSecret(t *testing.T) {
	for _, engine := range []string{"", builtin.EngineLocal, builtin.EngineJev, builtin.EngineAuto} {
		for _, c := range []struct {
			name  string
			allow []string
			model any
		}{
			{"unlisted local", []string{"ollama://other:1b"}, "ollama://gemma4:e4b"},
			{"glob miss", []string{"ollama://qwen3*"}, "ollama://gemma4:e4b"},
			{"query escapes glob", []string{"ollama://gemma4*"}, "ollama://gemma4:e4b?base_url=http://gpu.internal:11434"},
			{"unlisted cloud", []string{"ollama://*"}, "fakecloud://big"},
			{"jev not listed", []string{"ollama://*"}, "jev"},
		} {
			if engine == builtin.EngineLocal && c.model == "jev" {
				continue // the engine leaves nothing to try: input_invalid, not a refusal
			}
			t.Run(engine+"/"+c.name, func(t *testing.T) {
				_, h := newWorld(true, "gemma4:e4b")
				s := withT(t, &storeSecrets{vals: map[string]string{"openrouter_api_key": "k", "fakecloud_api_key": "k"}})
				res := classifyAs(t, listCls(t, h, s, c.allow, engine), specrun.PrivacyCloudAllowed, listIn(c.model, engine))
				if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
					t.Fatalf("%+v", res)
				}
				noTraffic(t, h, s)
			})
		}
	}
}

func TestAllowListPermitsExactAndGlob(t *testing.T) {
	for _, c := range []struct {
		name  string
		allow []string
		model any
		want  string
	}{
		{"exact", []string{"ollama://gemma4:e4b"}, "ollama://gemma4:e4b", "ollama://gemma4:e4b"},
		{"glob", []string{"ollama://gemma4*"}, "ollama://gemma4:e4b", "ollama://gemma4:e4b"},
		{"jev shorthand", []string{"jev"}, []any{"jev"}, jevModel},
		{"jev glob", []string{"jev://typesafe/*"}, []any{"jev"}, jevModel},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := newWorld(true, "gemma4:e4b")
			res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), c.allow, ""), specrun.PrivacyCloudAllowed, listIn(c.model, ""))
			if res.Outcome != core.OutcomeOK || out(res)["model"] != c.want {
				t.Fatalf("%+v %v", res.Err, res.Output)
			}
		})
	}
}

// An unlisted entry of a list is skipped; a listed one after it is used.
func TestAllowListSkipsAnUnlistedEntry(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b", "other:1b")
	res := classifyAs(t, listCls(t, h, withT(t, jevKeys()), []string{"ollama://gemma4*"}, ""), specrun.PrivacyCloudAllowed,
		listIn([]any{"ollama://other:1b", "ollama://gemma4:e4b"}, ""))
	if res.Outcome != core.OutcomeOK || out(res)["model"] != "ollama://gemma4:e4b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if h.count("other:1b") != 0 {
		t.Errorf("the unlisted model was asked about: %v", h.bodies)
	}
}

// The implicit Jev of auto is a model like any other: it needs the list.
func TestAutoImplicitJevNeedsTheAllowList(t *testing.T) {
	_, h := newWorld(false)
	s := withT(t, jevKeys())
	res := classifyAs(t, listCls(t, h, s, []string{"ollama://gemma4*"}, ""), specrun.PrivacyCloudAllowed, listIn("ollama://gemma4:e4b", ""))
	if res.Outcome != core.OutcomeFailed || h.count(jevURL) != 0 || len(s.got) != 0 {
		t.Fatalf("%+v calls %v secrets %v", res, h.urls(), s.got)
	}
	if !strings.Contains(res.Err.Message, "not allowed") || !strings.Contains(res.Err.Message, jevModel) {
		t.Errorf("message %s", res.Err.Message)
	}
}

func TestStepOnlyEngineNamesStillValidated(t *testing.T) {
	if _, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{JudgeEngine: "remote", Allow: allowAny}); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("config: %v", err)
	}
}

// ---- ai.generate ----

func genCls(t *testing.T, h *fakeHTTP, s core.Secrets, allow []string) builtin.Generator {
	t.Helper()
	g, err := builtin.NewLLMGenerator(builtin.LLMDeps{HTTP: h, Secrets: s, LocalHosts: listedHosts, Allow: allow})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func genList(models any) map[string]any {
	in := genIn("", map[string]any{"title": "sync"})
	in["model"] = models
	return in
}

func tagsOrChat(models ...string) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		if r.URL.Path == "/api/tags" {
			return 200, tagsOf(models...)
		}
		return ollamaReply("a brief", 10, 2)(r)
	}
}

func TestGenerateListFallsThroughAndRecordsTheModel(t *testing.T) {
	h := &fakeHTTP{reply: tagsOrChat("gemma4:e4b")}
	s := &storeSecrets{t: t}
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	res := gen(t, genCls(t, h, s, allowAny), b, specrun.PrivacyCloudAllowed,
		genList([]any{"fakecloud://big", "ollama://qwen3:4b", "ollama://gemma4:e4b"}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if out(res)["model"] != "ollama://gemma4:e4b" || out(res)["text"] != "a brief" {
		t.Errorf("%v", res.Output)
	}
	if h.count("api.fakecloud.example") != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestGenerateSingleModelRecordsItAfterTheProbe(t *testing.T) {
	h := &fakeHTTP{reply: tagsOrChat("gemma4:e4b")}
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	res := gen(t, genCls(t, h, &storeSecrets{t: t}, allowAny), b, specrun.PrivacyLocalOnly, genIn("ollama://gemma4:e4b", map[string]any{"title": "sync"}))
	if res.Outcome != core.OutcomeOK || out(res)["model"] != "ollama://gemma4:e4b" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if h.count("/api/tags") != 1 || h.count("/api/chat") != 1 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestGenerateListCeilingAndNoneUsable(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	h := &fakeHTTP{reply: tagsOrChat()}
	s := &storeSecrets{t: t}
	res := gen(t, genCls(t, h, s, allowAny), b, specrun.PrivacyLocalOnly,
		genList([]any{"fakecloud://big", "ollama://gemma4:e4b"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"fakecloud://big", "ceiling", "ollama://gemma4:e4b", "not detected"} {
		if !strings.Contains(res.Err.Message, want) {
			t.Errorf("message lacks %q: %s", want, res.Err.Message)
		}
	}
	if h.count("/api/chat") != 0 || len(s.got) != 0 {
		t.Errorf("calls %v secrets %v", h.urls(), s.got)
	}
}

func TestGenerateAllowListRefusesBeforeAnyRequestOrSecret(t *testing.T) {
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	for _, c := range []struct {
		name  string
		allow []string
		model any
	}{
		{"unlisted", []string{"ollama://other*"}, "ollama://gemma4:e4b"},
		{"list all unlisted", []string{"ollama://other*"}, []any{"ollama://gemma4:e4b", "fakecloud://big"}},
		{"cloud unlisted", []string{"ollama://*"}, "fakecloud://big"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &fakeHTTP{reply: tagsOrChat("gemma4:e4b")}
			s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
			res := gen(t, genCls(t, h, s, c.allow), b, specrun.PrivacyCloudAllowed, genList(c.model))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
				t.Fatalf("%+v", res)
			}
			if h.calls() != 0 || len(s.got) != 0 {
				t.Errorf("calls %v secrets %v", h.urls(), s.got)
			}
		})
	}
}

// The template backend runs no model: the allow-list is not its business.
func TestGenerateTemplateBackendNeedsNoAllowList(t *testing.T) {
	g, err := builtin.GeneratorFor(builtin.BackendTemplate, builtin.LLMDeps{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	res := gen(t, g, b, specrun.PrivacyLocalOnly, genIn("ollama://gemma4:e4b", map[string]any{"title": "sync"}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if _, has := out(res)["model"]; has {
		t.Errorf("the template backend used no model: %v", res.Output)
	}
}

func TestGenerateJevEntryIsRefused(t *testing.T) {
	h := &fakeHTTP{reply: tagsOrChat("gemma4:e4b")}
	b := bundleWith(t, map[string]string{"brief@1.yaml": sysPrompt})
	res := gen(t, genCls(t, h, &storeSecrets{t: t}, allowAny), b, specrun.PrivacyCloudAllowed, genList([]any{"jev", "ollama://gemma4:e4b"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || h.calls() != 0 {
		t.Fatalf("%+v calls %v", res, h.urls())
	}
}
