package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/schema"
	specrun "hop.top/flo/spec/run"
	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/prompt"
)

const jevURL = "https://openrouter.ai/api/v1/systemone"

var engineLabels = []string{"security", "breaking", "routine", "noise"}

// jevAnswer is a System One reply choosing label for the question "label".
func jevAnswer(label string, conf, usd float64) string {
	probs := map[string]float64{}
	for _, l := range engineLabels {
		probs[l] = (1 - conf) / float64(len(engineLabels)-1)
	}
	probs[label] = conf
	b, _ := json.Marshal(map[string]any{
		"model": "typesafe/jev-1.14",
		"answers": map[string]any{"label": map[string]any{
			"type": "choice", "choice": label, "confidence": conf, "probabilities": probs,
		}},
		"usage": map[string]any{"input_tokens": 50, "output_tokens": 5, "cost": usd},
	})
	return string(b)
}

// tagsOf is the ollama /api/tags reply listing names.
func tagsOf(names ...string) string {
	ms := make([]map[string]any, len(names))
	for i, n := range names {
		ms[i] = map[string]any{"name": n, "model": n}
	}
	b, _ := json.Marshal(map[string]any{"models": ms})
	return string(b)
}

// engineWorld is the fake network of the engine tests: an ollama daemon that
// is up or down with the models it lists, and the Jev route.
type engineWorld struct {
	up     bool
	models []string
	// tagsBody overrides the /api/tags reply when set.
	tagsBody string
	jev      string
}

func (w *engineWorld) reply(r *http.Request) (int, string) {
	switch {
	case r.URL.Path == "/api/tags":
		if !w.up {
			return 503, `{"error":"down"}`
		}
		if w.tagsBody != "" {
			return 200, w.tagsBody
		}
		return 200, tagsOf(w.models...)
	case r.URL.Path == "/api/chat":
		return ollamaReply(verdict("routine", 0.8, "local"), 100, 20)(r)
	case r.URL.String() == jevURL:
		return 200, w.jev
	}
	return 500, `{"error":"unexpected ` + r.URL.String() + `"}`
}

func newWorld(up bool, models ...string) (*engineWorld, *fakeHTTP) {
	w := &engineWorld{up: up, models: models, jev: jevAnswer("security", 0.9, 0.0002)}
	return w, &fakeHTTP{reply: w.reply}
}

func (f *fakeHTTP) urls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reqs))
	for i, r := range f.reqs {
		out[i] = r.Method + " " + r.URL.String()
	}
	return out
}

func (f *fakeHTTP) count(sub string) int {
	n := 0
	for _, u := range f.urls() {
		if strings.Contains(u, sub) {
			n++
		}
	}
	return n
}

func engineCls(t *testing.T, h *fakeHTTP, s core.Secrets, engine string) builtin.Classifier {
	t.Helper()
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: s, LocalHosts: listedHosts, JudgeEngine: engine, Jev: fakeJev, Allow: allowAny,
	})
	if err != nil || c == nil {
		t.Fatalf("judge backend: %v %v", c, err)
	}
	return c
}

func engineIn(model, engine string) map[string]any {
	in := judgeIn(model, map[string]any{"package": "net", "version": "0.9.0"})
	if engine != "" {
		in["engine"] = engine
	}
	return in
}

func jevKeys() *storeSecrets {
	return &storeSecrets{t: nil, vals: map[string]string{"openrouter_api_key": "or-s3cret"}}
}

func withT(t *testing.T, s *storeSecrets) *storeSecrets { s.t = t; return s }

func TestEngineLocalIsTheCurrentBehaviour(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	s := withT(t, jevKeys())
	res := classifyAs(t, engineCls(t, h, s, builtin.EngineLocal), specrun.PrivacyLocalOnly, engineIn("ollama://gemma4:e4b", ""))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	if rowsOf(res)[0]["label"] != "routine" || res.Output.(map[string]any)["engine"] != "local" {
		t.Fatalf("%v", res.Output)
	}
	if h.count("/api/tags") != 1 || h.count("/api/chat") != 1 || h.count("openrouter") != 0 || len(s.got) != 0 {
		t.Errorf("calls %v secrets %v", h.urls(), s.got)
	}
}

func TestEngineJev(t *testing.T) {
	w, h := newWorld(true)
	w.jev = jevAnswer("security", 0.9, 0.0002)
	s := withT(t, jevKeys())
	// The model is the local engine's; Jev ignores it.
	res := classifyAs(t, engineCls(t, h, s, builtin.EngineJev), specrun.PrivacyCloudAllowed, engineIn("ollama://gemma4:e4b", ""))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	row := rowsOf(res)[0]
	if row["label"] != "security" || row["confidence"] != 0.9 {
		t.Fatalf("row %v", row)
	}
	if res.Output.(map[string]any)["engine"] != "jev" {
		t.Errorf("engine %v", res.Output)
	}
	if res.Cost.USD != 0.0002 || res.Cost.Tokens != 55 {
		t.Errorf("cost %+v", res.Cost)
	}
	if got := h.urls(); len(got) != 1 || got[0] != "POST "+jevURL {
		t.Fatalf("calls %v", got)
	}
	if a := h.reqs[0].Header.Get("Authorization"); a != "Bearer or-s3cret" {
		t.Errorf("authorization %q", a)
	}
	if len(s.got) != 1 || s.got[0] != "openrouter_api_key" {
		t.Errorf("secrets read %v", s.got)
	}
	// Labels and instructions map as Jev takes them.
	for _, want := range []string{`"choice"`, "security", "breaking", "routine", "noise", `"net"`} {
		if !strings.Contains(h.bodies[0], want) {
			t.Errorf("request lacks %q: %s", want, h.bodies[0])
		}
	}
}

func TestEngineJevMissingKey(t *testing.T) {
	_, h := newWorld(true)
	for _, c := range []core.Secrets{withT(t, &storeSecrets{}), nil} {
		res := classifyAs(t, engineCls(t, h, c, builtin.EngineJev), specrun.PrivacyCloudAllowed, engineIn("ollama://m", ""))
		if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound {
			t.Fatalf("%+v", res)
		}
	}
	if h.calls() != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

func TestEngineJevIsCloud(t *testing.T) {
	_, h := newWorld(true)
	s := withT(t, jevKeys())
	for _, level := range []specrun.Privacy{specrun.PrivacyLocalOnly, ""} {
		res := classifyAs(t, engineCls(t, h, s, builtin.EngineJev), level, engineIn("ollama://m", ""))
		if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
			t.Fatalf("%q: %+v", level, res)
		}
	}
	if h.calls() != 0 || len(s.got) != 0 {
		t.Errorf("a refused step sent %v, read %v", h.urls(), s.got)
	}
}

func TestEngineAutoPicksLocalWhenTheModelIsListed(t *testing.T) {
	for _, tag := range []string{"gemma4:e4b", "gemma4:latest"} {
		model := "ollama://gemma4:e4b"
		if tag == "gemma4:latest" {
			model = "ollama://gemma4"
		}
		_, h := newWorld(true, "other:1b", tag)
		s := withT(t, jevKeys())
		res := classifyAs(t, engineCls(t, h, s, ""), specrun.PrivacyLocalOnly, engineIn(model, ""))
		if res.Outcome != core.OutcomeOK || res.Output.(map[string]any)["engine"] != "local" {
			t.Fatalf("%s: %+v %v", tag, res.Err, res.Output)
		}
		want := []string{"GET http://localhost:11434/api/tags", "POST http://localhost:11434/api/chat"}
		if got := h.urls(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("%s: calls %v", tag, got)
		}
		if len(s.got) != 0 {
			t.Errorf("a local pick read %v", s.got)
		}
	}
}

func TestEngineAutoFallsBackToJev(t *testing.T) {
	for _, c := range []struct {
		name   string
		up     bool
		models []string
		tags   string
	}{
		{"ollama down", false, nil, ""},
		{"model not pulled", true, []string{"other:1b"}, ""},
		{"no models", true, nil, ""},
		{"garbled tags", true, nil, "not json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, h := newWorld(c.up, c.models...)
			w.tagsBody = c.tags
			s := withT(t, jevKeys())
			res := classifyAs(t, engineCls(t, h, s, builtin.EngineAuto), specrun.PrivacyCloudAllowed, engineIn("ollama://gemma4:e4b", ""))
			if res.Outcome != core.OutcomeOK || res.Output.(map[string]any)["engine"] != "jev" {
				t.Fatalf("%+v %v", res.Err, res.Output)
			}
			if h.count("/api/chat") != 0 || h.count(jevURL) != 1 {
				t.Errorf("calls %v", h.urls())
			}
		})
	}
}

// An unreachable daemon (a transport error, not a status) is "not detected".
func TestEngineAutoUnreachableDaemon(t *testing.T) {
	var calls []string
	d := doerFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.String())
		if r.URL.Host == "localhost:11434" {
			return nil, errors.New("connection refused")
		}
		return &http.Response{
			StatusCode: 200, Header: http.Header{}, Request: r,
			Body: nopBody(jevAnswer("noise", 0.7, 0.0001)),
		}, nil
	})
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{HTTP: d, Secrets: withT(t, jevKeys()), LocalHosts: listedHosts, Jev: fakeJev, Allow: allowAny})
	if err != nil {
		t.Fatal(err)
	}
	res := classifyAs(t, c, specrun.PrivacyCloudAllowed, engineIn("ollama://m", ""))
	if res.Outcome != core.OutcomeOK || res.Output.(map[string]any)["engine"] != "jev" {
		t.Fatalf("%+v calls %v", res.Err, calls)
	}
}

func TestEngineAutoLocalOnlyNeverPicksJev(t *testing.T) {
	for _, c := range []struct {
		name   string
		up     bool
		models []string
		model  string
	}{
		{"ollama down", false, nil, "ollama://gemma4:e4b"},
		{"model not pulled", true, []string{"other:1b"}, "ollama://gemma4:e4b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := newWorld(c.up, c.models...)
			s := withT(t, jevKeys())
			for _, level := range []specrun.Privacy{specrun.PrivacyLocalOnly, ""} {
				res := classifyAs(t, engineCls(t, h, s, ""), level, engineIn(c.model, ""))
				if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
					t.Fatalf("%q: %+v", level, res)
				}
				for _, want := range []string{"local", "jev"} {
					if !strings.Contains(strings.ToLower(res.Err.Message+res.Err.Suggestion), want) {
						t.Errorf("message lacks %q: %q / %q", want, res.Err.Message, res.Err.Suggestion)
					}
				}
			}
			if h.count("openrouter") != 0 || h.count("/api/chat") != 0 || len(s.got) != 0 {
				t.Errorf("calls %v secrets %v", h.urls(), s.got)
			}
		})
	}
}

// An explicit cloud model under a local-only ceiling is refused as named:
// Jev does not stand in for it.
func TestEngineAutoCloudModelUnderLocalOnlyIsRefusedNotSwapped(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	s := withT(t, jevKeys())
	res := classifyAs(t, engineCls(t, h, s, ""), specrun.PrivacyLocalOnly, engineIn("fakecloud://big", ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePrivacyViolation {
		t.Fatalf("%+v", res)
	}
	if h.calls() != 0 || len(s.got) != 0 {
		t.Errorf("calls %v secrets %v", h.urls(), s.got)
	}
}

func TestEngineAutoNoLocalNoKeyNamesBothOptions(t *testing.T) {
	_, h := newWorld(false)
	res := classifyAs(t, engineCls(t, h, withT(t, &storeSecrets{}), ""), specrun.PrivacyCloudAllowed, engineIn("ollama://gemma4:e4b", ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeSecretNotFound {
		t.Fatalf("%+v", res)
	}
	text := res.Err.Message + " " + res.Err.Suggestion
	for _, want := range []string{"openrouter_api_key", "gemma4:e4b", "ollama"} {
		if !strings.Contains(text, want) {
			t.Errorf("error lacks %q: %s", want, text)
		}
	}
	if h.count("openrouter") != 0 || h.count("/api/chat") != 0 {
		t.Errorf("calls %v", h.urls())
	}
}

// A model that is not local is not probed: nothing is asked of a host a flow
// file names beyond the call the flow names, and Jev's key stays with Jev's
// own endpoint. Auto does not swap it for Jev.
func TestEngineAutoNeverProbesANonLocalHost(t *testing.T) {
	_, h := newWorld(true)
	s := withT(t, jevKeys())
	for _, model := range []string{
		"ollama://evil.example.com:11434/m",
		"ollama://localhost:11434/m?base_url=https://evil.example.com",
	} {
		res := classifyAs(t, engineCls(t, h, s, ""), specrun.PrivacyCloudAllowed, engineIn(model, ""))
		if res.Outcome != core.OutcomeOK || res.Output.(map[string]any)["engine"] != "local" {
			t.Fatalf("%s: %+v", model, res)
		}
	}
	for _, r := range h.reqs {
		if r.URL.Path == "/api/tags" || r.URL.String() == jevURL {
			t.Errorf("request to %s", r.URL)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("%s: authorization %q", r.URL, r.Header.Get("Authorization"))
		}
	}
	if len(s.got) != 0 {
		t.Errorf("secrets read: %v", s.got)
	}
}

// The key goes to Jev's endpoint only; the local daemon and its probe never
// see it.
func TestEngineKeyOnlyGoesToJev(t *testing.T) {
	_, h := newWorld(true, "gemma4:e4b")
	s := withT(t, jevKeys())
	classifyAs(t, engineCls(t, h, s, ""), specrun.PrivacyCloudAllowed, engineIn("ollama://gemma4:e4b", ""))
	classifyAs(t, engineCls(t, h, s, builtin.EngineJev), specrun.PrivacyCloudAllowed, engineIn("ollama://gemma4:e4b", ""))
	for _, r := range h.reqs {
		isJev := r.URL.String() == jevURL
		if got := r.Header.Get("Authorization") != ""; got != isJev {
			t.Errorf("%s: authorization present = %v", r.URL, got)
		}
	}
	for _, b := range h.bodies {
		if strings.Contains(b, "or-s3cret") {
			t.Errorf("key in a body: %s", b)
		}
	}
}

func TestEngineAutoProbesAListedLocalHost(t *testing.T) {
	_, h := newWorld(true)
	h.reply = func(r *http.Request) (int, string) {
		if r.URL.Host == "gpu.internal:11434" && r.URL.Path == "/api/tags" {
			return 200, tagsOf("big:70b")
		}
		return ollamaReply(verdict("routine", 0.8, "x"), 1, 1)(r)
	}
	res := classifyAs(t, engineCls(t, h, withT(t, jevKeys()), ""), specrun.PrivacyLocalOnly, engineIn("ollama://gpu.internal:11434/big:70b", ""))
	if res.Outcome != core.OutcomeOK || res.Output.(map[string]any)["engine"] != "local" {
		t.Fatalf("%+v", res)
	}
	if got := h.urls(); got[0] != "GET http://gpu.internal:11434/api/tags" {
		t.Errorf("calls %v", got)
	}
}

func TestEngineConfigAndStepOverride(t *testing.T) {
	for _, c := range []struct {
		name, cfg, step, want string
	}{
		{"config jev", builtin.EngineJev, "", "jev"},
		{"config local", builtin.EngineLocal, "", "local"},
		{"step local beats config jev", builtin.EngineJev, builtin.EngineLocal, "local"},
		{"step jev beats config local", builtin.EngineLocal, builtin.EngineJev, "jev"},
		{"step auto beats config jev", builtin.EngineJev, builtin.EngineAuto, "local"},
		{"default is auto", "", "", "local"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := newWorld(true, "gemma4:e4b")
			res := classifyAs(t, engineCls(t, h, withT(t, jevKeys()), c.cfg), specrun.PrivacyCloudAllowed, engineIn("ollama://gemma4:e4b", c.step))
			if res.Outcome != core.OutcomeOK || res.Output.(map[string]any)["engine"] != c.want {
				t.Fatalf("%+v %v", res.Err, res.Output)
			}
		})
	}
}

func TestEngineUnknownValueIsInputInvalid(t *testing.T) {
	_, h := newWorld(true, "m")
	// In the engine's configuration.
	_, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{HTTP: h, JudgeEngine: "remote", Allow: allowAny})
	if !core.IsCode(err, core.CodeUsage) {
		t.Errorf("config: %v", err)
	}
	// In a step's `with`: the node spec refuses it.
	r, _ := std(t)
	if err := builtin.RegisterClassify(r, engineCls(t, h, nil, "")); err != nil {
		t.Fatal(err)
	}
	n, _ := r.Lookup("flo/ai.classify@1")
	if _, err := schema.ApplyInputs(n.Spec().Inputs, engineIn("ollama://m", "remote")); err == nil || err.Code != core.CodeInputInvalid {
		t.Errorf("spec: %v", err)
	}
	// And the backend holds the line on its own.
	_, _, berr := engineCls(t, h, nil, "").Classify(context.Background(), builtin.ClassifyRequest{
		Items: []any{map[string]any{"a": 1}}, Labels: []string{"x", "y"}, Model: "ollama://m", Engine: "remote",
	})
	if !core.IsCode(berr, core.CodeInputInvalid) || h.calls() != 0 {
		t.Errorf("backend: %v calls %v", berr, h.urls())
	}
}

func TestEngineDetectionIsCachedPerRun(t *testing.T) {
	_, h := newWorld(true, "m")
	c := engineCls(t, h, withT(t, jevKeys()), "")
	req := func(run string) builtin.ClassifyRequest {
		return builtin.ClassifyRequest{
			Items: []any{map[string]any{"package": "a"}}, Labels: engineLabels, Model: "ollama://m",
			LocalOnly: true, Tenant: "default", Profile: "prof", RunID: run,
		}
	}
	for _, run := range []string{"run_1", "run_1", "run_1", "run_2"} {
		if _, _, err := c.Classify(context.Background(), req(run)); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.count("/api/tags"); n != 2 {
		t.Errorf("%d probes for 2 runs: %v", n, h.urls())
	}
}

// The dispatcher's seam on the context carries the probe and the calls of
// both engines, so a recorded run's tape sees them.
func TestEngineCallsGoThroughTheContextSeam(t *testing.T) {
	_, deps := newWorld(true, "m")
	_, seam := newWorld(false)
	c := engineCls(t, deps, withT(t, jevKeys()), "")
	ctx := core.WithHTTP(context.Background(), seam)
	_, _, err := c.Classify(ctx, builtin.ClassifyRequest{
		Items: []any{map[string]any{"package": "a"}}, Labels: engineLabels, Model: "ollama://m",
		Tenant: "default", Profile: "prof", RunID: "run_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if deps.calls() != 0 || seam.count("/api/tags") != 1 || seam.count(jevURL) != 1 {
		t.Errorf("deps %v seam %v", deps.urls(), seam.urls())
	}
}

func TestEngineJevRefusesWhatItCannotDo(t *testing.T) {
	_, h := newWorld(true)
	// A pattern needs a pattern source, as with the local engine.
	in := engineIn("ollama://m", builtin.EngineJev)
	in["pattern"] = "terse"
	res := classifyAs(t, engineCls(t, h, withT(t, jevKeys()), ""), specrun.PrivacyCloudAllowed, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature || h.calls() != 0 {
		t.Fatalf("%+v", res)
	}
	in = engineIn("ollama://m", builtin.EngineJev)
	in["schema"] = map[string]any{"type": "object"}
	res = classifyAs(t, engineCls(t, h, withT(t, jevKeys()), ""), specrun.PrivacyCloudAllowed, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature || h.calls() != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestEngineJevProviderErrors(t *testing.T) {
	w, h := newWorld(true)
	h.reply = func(r *http.Request) (int, string) { return 401, `{"error":"bad key"}` }
	_ = w
	res := classifyAs(t, engineCls(t, h, withT(t, jevKeys()), builtin.EngineJev), specrun.PrivacyCloudAllowed, engineIn("ollama://m", ""))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUpstreamFailed {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(res.Err.Message, "or-s3cret") {
		t.Errorf("key in error: %s", res.Err.Message)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func nopBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

// fakeJev stands in for kit's Jev judge: it posts the case to Jev's endpoint
// with the key and reads the System One reply for the question "label".
func fakeJev(key, _ string, rt http.RoundTripper) (judge.Judge, error) {
	return fakeJevJudge{key, rt}, nil
}

type fakeJevJudge struct {
	key string
	rt  http.RoundTripper
}

func (j fakeJevJudge) Judge(ctx context.Context, c judge.Case) (judge.Verdict, error) {
	body, _ := json.Marshal(map[string]any{"state": c.State, "questions": c.Questions})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/systemone", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+j.key)
	rt := j.rt
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return judge.Verdict{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusOK {
		return judge.Verdict{}, fmt.Errorf("jev: status %d", resp.StatusCode)
	}
	var r struct {
		Answers map[string]struct {
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
		Usage struct {
			In   int     `json:"input_tokens"`
			Out  int     `json:"output_tokens"`
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return judge.Verdict{}, err
	}
	a := r.Answers["label"]
	return judge.Verdict{
		Answers: map[string]judge.Answer{"label": {Value: a.Choice, Confidence: a.Confidence}},
		Cost:    judge.Cost{USD: r.Usage.Cost, Tokens: int64(r.Usage.In + r.Usage.Out)},
	}, nil
}

// The real kit Jev wire through the factory: request mapping (a choice over
// the labels, the item as state), response parsing and usage.cost.
func TestKitJevWire(t *testing.T) {
	_, h := newWorld(false)
	h.reply = func(r *http.Request) (int, string) {
		return 200, `{"model":"typesafe/jev-1.14","answers":{"label":{"type":"choice","choice":"breaking","confidence":0.7,` +
			`"probabilities":{"security":0.1,"breaking":0.7,"routine":0.1,"noise":0.1}}},"usage":{"input_tokens":120,"output_tokens":12,"cost":0.000042}}`
	}
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: withT(t, jevKeys()), LocalHosts: listedHosts, JudgeEngine: builtin.EngineJev, Jev: builtin.KitJev, Allow: allowAny,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := engineIn("ollama://m", "")
	in["instructions"] = "Label by impact."
	res := classifyAs(t, c, specrun.PrivacyCloudAllowed, in)
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	row := rowsOf(res)[0]
	if row["label"] != "breaking" || math.Abs(row["confidence"].(float64)-0.7) > 1e-9 || res.Cost.USD != 0.000042 || res.Cost.Tokens != 132 {
		t.Fatalf("row %v cost %+v", row, res.Cost)
	}
	if got := h.urls(); len(got) != 1 || got[0] != "POST "+jevURL {
		t.Fatalf("calls %v", got)
	}
	if h.reqs[0].Header.Get("Authorization") != "Bearer or-s3cret" {
		t.Errorf("authorization %q", h.reqs[0].Header.Get("Authorization"))
	}
	var req struct {
		Model     string                    `json:"model"`
		State     map[string]any            `json:"state"`
		Questions map[string]map[string]any `json:"questions"`
	}
	if err := json.Unmarshal([]byte(h.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	q := req.Questions["label"]
	crit, _ := q["criteria"].(map[string]any)
	if req.Model != "typesafe/jev-1.14" || req.State["package"] != "net" || q["type"] != "choice" ||
		q["instructions"] != "Label by impact." || len(crit) != 4 {
		t.Errorf("request %s", h.bodies[0])
	}
}

// kitJevCls is the judge backend over the real kit Jev, on a fake wire that
// answers every Jev request with a verdict; the request bodies are kept.
func kitJevCls(t *testing.T, h *fakeHTTP, engine string, pats map[string]*prompt.Pattern) builtin.Classifier {
	t.Helper()
	h.reply = func(r *http.Request) (int, string) {
		if r.URL.Path == "/api/tags" {
			return 200, tagsOf()
		}
		return 200, `{"model":"typesafe/jev-1.14","answers":{"label":{"type":"choice","choice":"breaking","confidence":0.7,` +
			`"probabilities":{"security":0.1,"breaking":0.7,"routine":0.1,"noise":0.1}}},"usage":{"input_tokens":1,"output_tokens":1,"cost":0.00001}}`
	}
	c, err := builtin.ClassifierFor(builtin.BackendJudge, builtin.LLMDeps{
		HTTP: h, Secrets: withT(t, jevKeys()), LocalHosts: listedHosts, JudgeEngine: engine, Jev: builtin.KitJev,
		Allow: allowAny, Patterns: patterns(pats),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// jevInstructions is the instructions field of the first Jev request on the wire.
func jevInstructions(t *testing.T, h *fakeHTTP) string {
	t.Helper()
	for i, r := range h.reqs {
		if r.URL.String() != jevURL {
			continue
		}
		var req struct {
			Questions map[string]map[string]any `json:"questions"`
		}
		if err := json.Unmarshal([]byte(h.bodies[i]), &req); err != nil {
			t.Fatal(err)
		}
		s, _ := req.Questions["label"]["instructions"].(string)
		return s
	}
	t.Fatalf("no Jev request: %v", h.urls())
	return ""
}

var tersePattern = map[string]*prompt.Pattern{"terse": {Name: "terse", System: "ANSWER TERSELY\n"}}

// A pattern is not refused by Jev: its system text goes first in the
// question's instructions, then a blank line, then the step's instructions.
func TestJevPatternTextReachesTheInstructions(t *testing.T) {
	for _, c := range []struct{ name, instr, want string }{
		{"with instructions", "Label by impact.", "ANSWER TERSELY\n\nLabel by impact."},
		{"without instructions", "", "ANSWER TERSELY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := newWorld(false)
			cls := kitJevCls(t, h, builtin.EngineJev, tersePattern)
			in := engineIn("ollama://m", "")
			in["pattern"] = "terse"
			in["instructions"] = c.instr
			res := classifyAs(t, cls, specrun.PrivacyCloudAllowed, in)
			if res.Outcome != core.OutcomeOK || out(res)["engine"] != "jev" {
				t.Fatalf("%+v %v", res.Err, res.Output)
			}
			if got := jevInstructions(t, h); got != c.want {
				t.Errorf("instructions %q, want %q", got, c.want)
			}
		})
	}
}

// The auto fallback to Jev maps the pattern the same way.
func TestJevPatternOnTheAutoFallback(t *testing.T) {
	_, h := newWorld(false)
	cls := kitJevCls(t, h, builtin.EngineAuto, tersePattern)
	in := engineIn("ollama://m", "")
	in["pattern"] = "terse"
	in["instructions"] = "Label by impact."
	res := classifyAs(t, cls, specrun.PrivacyCloudAllowed, in)
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "jev" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	if got := jevInstructions(t, h); got != "ANSWER TERSELY\n\nLabel by impact." {
		t.Errorf("instructions %q", got)
	}
}

// strategy is ignored by Jev, not a failure: the run records a note.
func TestJevStrategyIsIgnoredWithANote(t *testing.T) {
	_, h := newWorld(false)
	cls := kitJevCls(t, h, builtin.EngineJev, nil)
	in := engineIn("ollama://m", "")
	in["strategy"] = "votes:5"
	res := classifyAs(t, cls, specrun.PrivacyCloudAllowed, in)
	if res.Outcome != core.OutcomeOK || out(res)["engine"] != "jev" {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	notes, _ := out(res)["notes"].([]any)
	if len(notes) != 1 || !strings.Contains(notes[0].(string), "strategy") || !strings.Contains(notes[0].(string), "votes:5") ||
		!strings.Contains(notes[0].(string), "jev") {
		t.Errorf("notes %v", out(res)["notes"])
	}
	if h.count(jevURL) != 1 {
		t.Errorf("calls %v", h.urls())
	}
}

// No strategy, no note; and a local judge keeps its strategy, with no note.
func TestJevNoStrategyNoNote(t *testing.T) {
	_, h := newWorld(false)
	res := classifyAs(t, kitJevCls(t, h, builtin.EngineJev, nil), specrun.PrivacyCloudAllowed, engineIn("ollama://m", ""))
	if _, has := out(res)["notes"]; res.Outcome != core.OutcomeOK || has {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
	_, h2 := newWorld(true, "m")
	in := engineIn("ollama://m", builtin.EngineLocal)
	in["strategy"] = "votes:2"
	res = classifyAs(t, engineCls(t, h2, withT(t, jevKeys()), ""), specrun.PrivacyCloudAllowed, in)
	if _, has := out(res)["notes"]; res.Outcome != core.OutcomeOK || has {
		t.Fatalf("%+v %v", res.Err, res.Output)
	}
}

// A pattern that is not found is still input_invalid under Jev, before any request.
func TestJevUnknownPatternFails(t *testing.T) {
	_, h := newWorld(false)
	in := engineIn("ollama://m", "")
	in["pattern"] = "missing"
	res := classifyAs(t, kitJevCls(t, h, builtin.EngineJev, tersePattern), specrun.PrivacyCloudAllowed, in)
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || h.count(jevURL) != 0 {
		t.Fatalf("%+v", res)
	}
}
