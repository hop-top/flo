package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/schema"
	specrun "hop.top/flo/spec/run"
	"hop.top/kit/go/ai/prompt"
)

// verdict is one model reply: the judgment of the question "label".
func verdict(value string, conf float64, why string) string {
	b, _ := json.Marshal(map[string]any{"label": map[string]any{"value": value, "confidence": conf, "why": why}})
	return string(b)
}

// byItem answers each request with the verdict keyed by a package name found
// in the request body; tokens are reported per call.
func byItem(answers map[string]string, in, out int, wrap func(string, int, int) func(*http.Request) (int, string)) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		body := bodyOf(r)
		for pkg, ans := range answers {
			if strings.Contains(body, `"`+pkg+`"`) || strings.Contains(body, `\"`+pkg+`\"`) {
				return wrap(ans, in, out)(r)
			}
		}
		return 500, `{"error":"no answer for ` + body + `"}`
	}
}

// bodyOf is the request body the fake recorded; the fake reads it once, so
// the reply func gets it through the context-free GetBody.
func bodyOf(r *http.Request) string {
	if r.GetBody == nil {
		return ""
	}
	rc, err := r.GetBody()
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func judgeCls(t *testing.T, h *fakeHTTP, s core.Secrets, price builtin.PriceFunc, pats prompt.PatternSource) builtin.Classifier {
	t.Helper()
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: s, LocalHosts: listedHosts, Price: price, Patterns: pats, JudgeEngine: builtin.EngineLocal, Allow: allowAny,
	})
	if err != nil || c == nil {
		t.Fatalf("judge backend: %v %v", c, err)
	}
	return c
}

// classifyAs runs flo/ai.classify@1 over c as the executor would: with the
// run's ceiling, tenant and profile.
func classifyAs(t *testing.T, c builtin.Classifier, ceiling specrun.Privacy, in map[string]any) core.Result {
	t.Helper()
	r, _ := std(t)
	if err := builtin.RegisterClassify(r, c); err != nil {
		t.Fatal(err)
	}
	n, _ := r.Lookup("flo/ai.classify@1")
	full, err := schema.ApplyInputs(n.Spec().Inputs, in)
	if err != nil {
		t.Fatal(err)
	}
	res, xerr := n.Execute(context.Background(), core.Invocation{
		Tenant: "default", Run: core.RunContext{ID: "run_1"}, StepID: "s", Node: n.Spec(), In: full,
		Privacy: ceiling, RunAs: "prof",
	})
	if xerr != nil {
		t.Fatal(xerr)
	}
	return res
}

func judgeIn(model string, items ...any) map[string]any {
	in := classifyIn(items...)
	in["model"] = model
	return in
}

func rowsOf(res core.Result) []map[string]any {
	var out []map[string]any
	for _, x := range res.Output.(map[string]any)["items"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestJudgeHappyPath(t *testing.T) {
	h := &fakeHTTP{reply: byItem(map[string]string{
		"left-pad": verdict("routine", 0.8, "patch bump"),
		"net":      verdict("security", 0.95, "fixes a CVE"),
	}, 100, 20, ollamaWrap)}
	s := &storeSecrets{t: t}
	in := judgeIn("ollama://gemma4:e4b",
		map[string]any{"package": "left-pad", "version": "1.0.1"},
		map[string]any{"package": "net", "version": "0.9.0"})
	in["instructions"] = "Label each release by what it means for a dependent."
	res := classifyAs(t, judgeCls(t, h, s, nil, nil), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	rows := rowsOf(res)
	if len(rows) != 2 || rows[0]["label"] != "routine" || rows[0]["confidence"] != 0.8 || rows[0]["why"] != "patch bump" ||
		rows[1]["label"] != "security" || rows[1]["confidence"] != 0.95 || rows[1]["why"] != "fixes a CVE" {
		t.Fatalf("rows %v", rows)
	}
	if rows[1]["item"].(map[string]any)["package"] != "net" {
		t.Errorf("item not echoed: %v", rows[1])
	}
	if h.calls() != 2 || h.reqs[0].URL.Host != "localhost:11434" {
		t.Fatalf("calls %d", h.calls())
	}
	body := h.bodies[0]
	for _, want := range []string{"security", "breaking", "routine", "noise", "what it means for a dependent", "gemma4:e4b"} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %q: %s", want, body)
		}
	}
	if len(s.got) != 0 || h.reqs[0].Header.Get("Authorization") != "" {
		t.Errorf("a local model got a key: %v", s.got)
	}
	if res.Cost.USD != 0 || res.Cost.Tokens != 240 {
		t.Errorf("local cost %+v", res.Cost)
	}
}

func ollamaWrap(text string, in, out int) func(*http.Request) (int, string) {
	return ollamaReply(text, in, out)
}

func TestJudgeAbstain(t *testing.T) {
	h := &fakeHTTP{reply: byItem(map[string]string{"x": verdict("abstain", 0.4, "cannot tell")}, 10, 5, ollamaWrap)}
	in := judgeIn("ollama://m", map[string]any{"package": "x"})
	in["abstain"] = true
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	row := rowsOf(res)[0]
	if l, present := row["label"]; !present || l != nil || row["confidence"] != 0.4 || row["why"] != "cannot tell" {
		t.Fatalf("row %v", row)
	}
	if !strings.Contains(h.bodies[0], "abstain") {
		t.Errorf("abstain not offered: %s", h.bodies[0])
	}
}

// With abstain off the model is never offered "abstain"; answering it anyway
// is output that fails the reply schema, not a label.
func TestJudgeAbstainOffIsRefused(t *testing.T) {
	h := &fakeHTTP{reply: byItem(map[string]string{"x": verdict("abstain", 0.4, "cannot tell")}, 10, 5, ollamaWrap)}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly,
		judgeIn("ollama://m", map[string]any{"package": "x"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(h.bodies[0], `"abstain"`) {
		t.Errorf("abstain offered while off: %s", h.bodies[0])
	}
}

func TestJudgeLabelOutsideTheSetIsRejected(t *testing.T) {
	h := &fakeHTTP{reply: byItem(map[string]string{"x": verdict("urgent", 0.9, "made up")}, 10, 5, ollamaWrap)}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly,
		judgeIn("ollama://m", map[string]any{"package": "x"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid {
		t.Fatalf("%+v", res)
	}
}

func TestJudgeSchemaIsAnEngineFeature(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
	in := judgeIn("ollama://m", map[string]any{"package": "x"})
	in["schema"] = map[string]any{"type": "object"}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature {
		t.Fatalf("%+v", res)
	}
	if h.calls() != 0 {
		t.Errorf("%d calls", h.calls())
	}
}

func TestJudgeLocalOnlyCeilingRefusesCloud(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
	for _, c := range []struct {
		name, model string
		level       specrun.Privacy
	}{
		{"cloud scheme", "fakecloud://big", specrun.PrivacyLocalOnly},
		{"no ceiling is the strictest", "fakecloud://big", ""},
		{"public ollama host", "ollama://ollama.example.com:11434/gemma", specrun.PrivacyLocalOnly},
		{"base_url past a local host", "ollama://localhost:11434/m?base_url=https://api.example.com", specrun.PrivacyLocalOnly},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := classifyAs(t, judgeCls(t, h, s, nil, nil), c.level, judgeIn(c.model, map[string]any{"package": "x"}))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
				t.Fatalf("%+v", res)
			}
		})
	}
	if h.calls() != 0 || len(s.got) != 0 {
		t.Errorf("a refused step sent %d requests, read %v", h.calls(), s.got)
	}
}

// The backend holds the line by itself, whatever the node checked.
func TestJudgeBackendRefusesCloudWhenLocalOnly(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
	c := judgeCls(t, h, s, nil, nil)
	for _, model := range []string{"fakecloud://big", "ollama://ollama.example.com:11434/g"} {
		_, _, err := c.Classify(context.Background(), builtin.ClassifyRequest{
			Items: []any{map[string]any{"package": "x"}}, Labels: []string{"a", "b"}, Model: model,
			LocalOnly: true, Tenant: "default", Profile: "prof",
		})
		if !core.IsCode(err, core.CodePrivacyViolation) {
			t.Errorf("%s: %v", model, err)
		}
	}
	if h.calls() != 0 || len(s.got) != 0 {
		t.Errorf("calls %d secrets %v", h.calls(), s.got)
	}
}

func TestJudgeCloudCostSummedAndKeyed(t *testing.T) {
	// The fake cloud adapter posts no body, so every call gets one answer.
	h := &fakeHTTP{reply: cloudReply(verdict("routine", 0.5, "r"), 1000, 200)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "s3cret"}}
	res := classifyAs(t, judgeCls(t, h, s, perMillion(3, 15), nil), specrun.PrivacyCloudAllowed,
		judgeIn("fakecloud://big", map[string]any{"package": "a"}, map[string]any{"package": "b"}, map[string]any{"package": "c"}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	// 3 calls x (1000*3 + 200*15) / 1e6
	if res.Cost.Tokens != 3600 || res.Cost.USD < 0.017999 || res.Cost.USD > 0.018001 {
		t.Errorf("cost %+v", res.Cost)
	}
	for i, r := range h.reqs {
		if got := r.Header.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("call %d authorization %q", i, got)
		}
	}
}

func TestJudgeUnpricedCloudModelCountsTokens(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply(verdict("routine", 0.5, "r"), 10, 5)}
	s := &storeSecrets{t: t, vals: map[string]string{"fakecloud_api_key": "k"}}
	res := classifyAs(t, judgeCls(t, h, s, perMillion(3, 15), nil), specrun.PrivacyCloudAllowed,
		judgeIn("fakecloud://other", map[string]any{"package": "a"}))
	if res.Outcome != core.OutcomeOK || res.Cost.Tokens != 15 || res.Cost.USD != 0 {
		t.Fatalf("%+v %+v", res.Cost, res.Err)
	}
}

func TestJudgeMissingSecretFailsBeforeAnyCall(t *testing.T) {
	h := &fakeHTTP{reply: cloudReply("x", 1, 1)}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyCloudAllowed,
		judgeIn("fakecloud://big", map[string]any{"package": "a"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound || h.calls() != 0 {
		t.Fatalf("%+v calls %d", res, h.calls())
	}
}

// Rows come back in item order however the calls finish.
func TestJudgeBatchOrderKept(t *testing.T) {
	const n = 9
	answers := map[string]string{}
	items := make([]any, n)
	want := make([]any, n)
	labels := []string{"security", "breaking", "routine", "noise"}
	for i := range n {
		pkg := fmt.Sprintf("pkg%d", i)
		items[i] = map[string]any{"package": pkg}
		want[i] = labels[i%len(labels)]
		answers[pkg] = verdict(labels[i%len(labels)], 0.5, pkg)
	}
	inner := byItem(answers, 1, 1, ollamaWrap)
	h := &fakeHTTP{reply: func(r *http.Request) (int, string) {
		// Early items finish last.
		for i := range n {
			if strings.Contains(bodyOf(r), fmt.Sprintf("pkg%d\\\"", i)) || strings.Contains(bodyOf(r), fmt.Sprintf("pkg%d\"", i)) {
				time.Sleep(time.Duration(n-i) * 5 * time.Millisecond)
				break
			}
		}
		return inner(r)
	}}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, judgeIn("ollama://m", items...))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	for i, r := range rowsOf(res) {
		if r["label"] != want[i] || r["why"] != fmt.Sprintf("pkg%d", i) || r["item"].(map[string]any)["package"] != fmt.Sprintf("pkg%d", i) {
			t.Errorf("row %d = %v", i, r)
		}
	}
}

func TestJudgeVotesStrategy(t *testing.T) {
	h := &fakeHTTP{reply: byItem(map[string]string{"a": verdict("routine", 0.9, "r")}, 10, 5, ollamaWrap)}
	in := judgeIn("ollama://m", map[string]any{"package": "a"})
	in["strategy"] = "votes:3"
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if h.calls() != 3 || rowsOf(res)[0]["confidence"] != 1.0 || res.Cost.Tokens != 45 {
		t.Fatalf("calls %d row %v cost %+v", h.calls(), rowsOf(res)[0], res.Cost)
	}
}

func TestJudgeUnknownStrategyIsInputInvalid(t *testing.T) {
	for _, s := range []string{"bogus", "votes:x", "votes:0", "votes:99"} {
		h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
		in := judgeIn("ollama://m", map[string]any{"package": "a"})
		in["strategy"] = s
		res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, in)
		if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || h.calls() != 0 {
			t.Errorf("%s: %+v calls %d", s, res, h.calls())
		}
	}
}

type patterns map[string]*prompt.Pattern

func (p patterns) Load(name string) (*prompt.Pattern, error) {
	if v, ok := p[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("pattern %q: %w", name, prompt.ErrNotFound)
}
func (p patterns) List() ([]string, error) { return nil, nil }

func TestJudgePattern(t *testing.T) {
	src := patterns{"terse": {Name: "terse", System: "ANSWER TERSELY"}}
	h := &fakeHTTP{reply: byItem(map[string]string{"a": verdict("routine", 0.9, "r")}, 1, 1, ollamaWrap)}
	in := judgeIn("ollama://m", map[string]any{"package": "a"})
	in["pattern"] = "terse"
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, src), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeOK || !strings.Contains(h.bodies[0], "ANSWER TERSELY") {
		t.Fatalf("%+v %s", res.Err, h.bodies)
	}
	in["pattern"] = "missing"
	res = classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, src), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
		t.Fatalf("unknown pattern: %+v", res)
	}
	// No source configured: the pattern is not silently dropped.
	res = classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature {
		t.Fatalf("no pattern source: %+v", res)
	}
}

func TestJudgeItemMustBeAMap(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, judgeIn("ollama://m", "just a string"))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || h.calls() != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestJudgeProviderErrorIsUpstreamFailed(t *testing.T) {
	h := &fakeHTTP{reply: func(*http.Request) (int, string) { return 500, `{"error":"boom"}` }}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly,
		judgeIn("ollama://m", map[string]any{"package": "a"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUpstreamFailed {
		t.Fatalf("%+v", res)
	}
}

func TestJudgeBadModelURI(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyCloudAllowed,
		judgeIn("not a uri", map[string]any{"package": "a"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || h.calls() != 0 {
		t.Fatalf("%+v", res)
	}
}

// A single label is not a question: kit refuses it, flo says input_invalid.
func TestJudgeNeedsTwoLabels(t *testing.T) {
	h := &fakeHTTP{reply: ollamaReply("x", 1, 1)}
	in := judgeIn("ollama://m", map[string]any{"package": "a"})
	in["labels"] = []any{"only"}
	res := classifyAs(t, judgeCls(t, h, &storeSecrets{t: t}, nil, nil), specrun.PrivacyLocalOnly, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || h.calls() != 0 {
		t.Fatalf("%+v", res)
	}
}
