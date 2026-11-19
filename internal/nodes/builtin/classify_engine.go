package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/llmjudge"
	"hop.top/kit/go/ai/llm"
)

// Engines of the judge classifier: what answers the choice question.
const (
	// EngineLocal is llmjudge over the step's model, which must be local
	// under a local-only ceiling.
	EngineLocal = "local"
	// EngineJev is kit's go/ai/judge/jev: TypeSafe's Jev model through
	// OpenRouter, built by LLMDeps.Jev. Always cloud.
	EngineJev = "jev"
	// EngineAuto is local when the step's model is a local ollama model
	// the daemon lists, else Jev (never under a local-only ceiling).
	EngineAuto = "auto"

	// EnvJudgeEngine names the setting of the engine default.
	EnvJudgeEngine = "FLO_AI_JUDGE_ENGINE"

	// jevScheme is the llm scheme whose key Jev takes: Jev is served through
	// OpenRouter, so its key is the secret openrouter_api_key.
	jevScheme = "openrouter"
	// detectTimeout bounds the probe of the local daemon.
	detectTimeout = 2 * time.Second
	// detectMaxBody bounds the daemon's model list.
	detectMaxBody = 4 << 20
	// ollamaBase is the daemon's default endpoint, as in kit's adapter.
	ollamaBase = "http://localhost:11434"
	// probeCacheMax bounds the remembered probes; one run asks a few.
	probeCacheMax = 256
)

var engines = []string{EngineLocal, EngineJev, EngineAuto}

func validEngine(s string) bool {
	for _, e := range engines {
		if s == e {
			return true
		}
	}
	return false
}

// classifyAuto is local when the step's model is detected, else Jev. Under a
// local-only ceiling it never reaches Jev: no local model is a privacy
// violation, whatever the daemon or the key say.
func (j *JudgeClassifier) classifyAuto(ctx context.Context, req ClassifyRequest, mode llmjudge.ModeKind) (Labels, core.Cost, error) {
	found, why, err := j.detectLocal(ctx, req)
	if err != nil {
		return nil, core.Cost{}, err
	}
	if found {
		return j.classifyLocal(ctx, req, mode)
	}
	if req.LocalOnly {
		return nil, core.Cost{}, core.Errorf(core.CodePrivacyViolation, "no local judge model: %s; jev is cloud, so engine auto does not use it under a local-only ceiling", why).
			WithSuggestion("run ollama with the model pulled, name a local model, or set privacy: cloud-allowed")
	}
	if j.m.d.Jev == nil {
		return nil, core.Cost{}, core.Errorf(core.CodeEngineFeature, "no local judge model (%s) and engine jev is not available in this build", why).
			WithSuggestion("run ollama with the model pulled")
	}
	key, err := j.m.secret(ctx, req.Tenant, req.Profile, jevScheme)
	if err != nil {
		if core.IsCode(err, core.CodeSecretNotFound) {
			name := SecretNameOf(jevScheme)
			return nil, core.Cost{}, core.Errorf(core.CodeSecretNotFound, "no local judge model (%s) and secret %q not found for profile %q", why, name, req.Profile).
				WithSuggestion("run ollama with the model pulled, or store the OpenRouter API key as the secret " + name + " of the flow's profile for engine jev")
		}
		return nil, core.Cost{}, err
	}
	return j.classifyJev(ctx, req, key)
}

// classifyJev judges through the Jev judge of LLMDeps.Jev. key is the OpenRouter key when the caller
// has read it, else it is read here. The ceiling comes before the key: a
// refused call reads no secret and sends nothing. The judge is hermetic (key
// from the store, never the environment or llm.yaml), runs over the
// invocation's HTTP seam and keeps to Jev's own endpoint, the only place the
// key goes.
func (j *JudgeClassifier) classifyJev(ctx context.Context, req ClassifyRequest, key string) (Labels, core.Cost, error) {
	if req.LocalOnly {
		return nil, core.Cost{}, core.New(core.CodePrivacyViolation, "engine jev is cloud: it sends the items to a hosted service, which a local-only ceiling forbids").
			WithSuggestion("use engine local with a local model, or set privacy: cloud-allowed")
	}
	if j.m.d.Jev == nil {
		return nil, core.Cost{}, core.New(core.CodeEngineFeature, "engine jev is not available in this build").
			WithSuggestion("use engine local")
	}
	if req.Pattern != "" {
		return nil, core.Cost{}, core.New(core.CodeEngineFeature, "engine jev takes no prompt pattern").
			WithSuggestion("drop pattern, or use engine local")
	}
	cases, err := casesOf(req)
	if err != nil {
		return nil, core.Cost{}, err
	}
	if key == "" {
		if key, err = j.m.secret(ctx, req.Tenant, req.Profile, jevScheme); err != nil {
			return nil, core.Cost{}, err
		}
	}
	jj, err := j.m.d.Jev(key, llm.DoerTransport(j.m.doer(ctx)))
	if err != nil {
		return nil, core.Cost{}, core.Errorf(core.CodeInputInvalid, "engine jev: %v", err)
	}
	verdicts, err := judge.Batch(ctx, jj, cases, judgeParallel)
	if err != nil {
		switch {
		case errors.Is(err, judge.ErrInvalid):
			return nil, core.Cost{}, core.Errorf(core.CodeInputInvalid, "%v", err).
				WithSuggestion("labels needs at least two distinct labels, none of them \"abstain\"")
		}
		return nil, core.Cost{}, upstreamErr(err)
	}
	out := make(Labels, len(verdicts))
	var cost core.Cost
	for i, v := range verdicts {
		a := v.Answers[judgeQuestion]
		out[i] = Label{Confidence: a.Confidence, Why: a.Why, Engine: EngineJev}
		if !a.Abstained {
			value := a.Value
			out[i].Label = &value
		}
		cost.USD += v.Cost.USD
		cost.Tokens += v.Cost.Tokens
	}
	return out, cost, nil
}

// detectLocal reports whether the step's model is a local ollama model the
// daemon lists. why says what is missing when it is not. The error is the
// caller's to return: a bad model URI, or a cancelled context.
func (j *JudgeClassifier) detectLocal(ctx context.Context, req ClassifyRequest) (found bool, why string, err error) {
	uri, perr := llm.ParseURI(req.Model)
	if perr != nil {
		return false, "", core.Errorf(core.CodeInputInvalid, "model %q is not a model URI", llm.RedactURI(req.Model)).
			WithSuggestion("write scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	shown := llm.RedactURI(req.Model)
	level, lerr := node.ModelPrivacy(req.Model, j.local)
	if lerr != nil || level != node.PrivacyLocal {
		return false, "model " + shown + " is not on a local endpoint", nil
	}
	if uri.Scheme != "ollama" {
		return false, "model " + shown + " is not an ollama model, the only kind detected", nil
	}
	base := ollamaBase
	if v, ok := uri.Params["base_url"]; ok {
		if v != "" {
			base = strings.TrimRight(v, "/")
		}
	} else if uri.Host != "" {
		base = "http://" + uri.Host
	}
	ok, err := j.listed(ctx, req.RunID, base, uri.Model)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "ollama at " + base + " is not answering or does not list " + uri.Model, nil
	}
	return true, "", nil
}

// listed asks the daemon at base (GET /api/tags, over the invocation's HTTP
// seam, so a recording keeps the call) whether it lists model. The answer is
// kept for the run: later steps of it do not ask again. A daemon that does
// not answer in time, or answers with anything but a model list, lists
// nothing.
func (j *JudgeClassifier) listed(ctx context.Context, run, base, model string) (bool, error) {
	key := run + "|" + base + "|" + model
	if run != "" {
		if v, ok := j.probes.get(key); ok {
			return v, nil
		}
	}
	v, err := j.probe(ctx, base, model)
	if err != nil {
		return false, err
	}
	if run != "" {
		j.probes.put(key, v)
	}
	return v, nil
}

func (j *JudgeClassifier) probe(ctx context.Context, base, model string) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return false, nil //nolint:nilerr // a base URL that cannot be asked lists nothing
	}
	doer := j.m.doer(ctx)
	var resp *http.Response
	if doer == nil {
		resp, err = http.DefaultClient.Do(hreq)
	} else {
		resp, err = doer.Do(hreq)
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err() //nolint:wrapcheck // the engine's to classify
		}
		return false, nil
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, detectMaxBody))
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err() //nolint:wrapcheck // the engine's to classify
		}
		return false, nil
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &tags) != nil {
		return false, nil
	}
	// A name without a tag is the daemon's :latest.
	want := []string{model}
	if !strings.Contains(model, ":") {
		want = append(want, model+":latest")
	}
	for _, m := range tags.Models {
		for _, w := range want {
			if m.Name == w || m.Model == w {
				return true, nil
			}
		}
	}
	return false, nil
}

// probeCache remembers what the daemon listed, per run.
type probeCache struct {
	mu sync.Mutex
	m  map[string]bool
}

func (c *probeCache) get(k string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *probeCache) put(k string, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= probeCacheMax {
		c.m = map[string]bool{}
	}
	c.m[k] = v
}
