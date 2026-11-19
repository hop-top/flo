package builtin

import (
	"context"
	"errors"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/kit/go/ai/judge"
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
	// EngineAuto is the list as written: each model by its own kind, in order.
	// A single local model (or none) is followed by Jev.
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

// classifyJev judges through the Jev judge of LLMDeps.Jev. p is the pick: the
// pick has checked the allow-list and the ceiling and read the OpenRouter key,
// so a refused call read no secret and sent nothing. The judge is hermetic
// (key from the store, never the environment or llm.yaml), runs over the
// invocation's HTTP seam and keeps to Jev's own endpoint, the only place the
// key goes.
func (j *JudgeClassifier) classifyJev(ctx context.Context, req ClassifyRequest, p picked) (Labels, core.Cost, error) {
	if req.Pattern != "" {
		return nil, core.Cost{}, core.New(core.CodeEngineFeature, "engine jev takes no prompt pattern").
			WithSuggestion("drop pattern, or use engine local")
	}
	cases, err := casesOf(req)
	if err != nil {
		return nil, core.Cost{}, err
	}
	jj, err := j.m.d.Jev(p.key, p.ref.jevName(), llm.DoerTransport(j.m.doer(ctx)))
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
		out[i] = Label{Confidence: a.Confidence, Why: a.Why, Engine: EngineJev, Model: p.ref.model}
		if !a.Abstained {
			value := a.Value
			out[i].Label = &value
		}
		cost.USD += v.Cost.USD
		cost.Tokens += v.Cost.Tokens
	}
	return out, cost, nil
}
