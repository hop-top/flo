package builtin

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/llmjudge"
	"hop.top/kit/go/ai/llm"
	"hop.top/kit/go/ai/prompt"
)

const (
	// judgeParallel bounds the concurrent model calls of one classify step.
	judgeParallel = 4
	// judgeMaxVotes bounds the samples per item of the votes strategy: each
	// one is a model call per item.
	judgeMaxVotes = 9
	// judgeDefaultVotes is the sample count of a bare "votes" strategy.
	judgeDefaultVotes = 3
	// judgeQuestion names the one question asked per item.
	judgeQuestion = "label"
)

// JudgeClassifier is the judge backend of flo/ai.classify@1: each item is a
// case of kit's go/ai/judge with one choice question over the closed label
// set, answered by the step's model through llmjudge. The model client is
// built per invocation by the same rules as the llm generator: hermetic,
// over the engine's HTTP seam, local-only under a local-only ceiling.
type JudgeClassifier struct {
	m      *modelClients
	engine string
}

// NewJudgeClassifier checks d and returns the backend.
func NewJudgeClassifier(d LLMDeps) (*JudgeClassifier, error) {
	m, err := newModelClients(d)
	if err != nil {
		return nil, err
	}
	engine := d.JudgeEngine
	if engine == "" {
		engine = EngineAuto
	}
	if !validEngine(engine) {
		return nil, core.Errorf(core.CodeUsage, "ai.judge.engine=%s: unknown engine (%s)", engine, strings.Join(engines, ", "))
	}
	return &JudgeClassifier{m: m, engine: engine}, nil
}

// Classify implements Classifier. Every item is judged, as given: the model
// sees the whole item (an advisory's fields included) and the instructions;
// nothing is decided for it by item shape. The engine (local, jev or auto)
// is the step's, else the configured default; see engine.go.
func (j *JudgeClassifier) Classify(ctx context.Context, req ClassifyRequest) (Labels, core.Cost, error) {
	if req.Schema != nil {
		return nil, core.Cost{}, core.New(core.CodeEngineFeature, "the judge backend cannot return schema fields").
			WithSuggestion("drop schema, or use a backend that returns extra fields")
	}
	engine := req.Engine
	if engine == "" {
		engine = j.engine
	}
	if !validEngine(engine) {
		return nil, core.Cost{}, core.Errorf(core.CodeInputInvalid, "engine %q is not known to the judge backend", engine).
			WithSuggestion("use " + strings.Join(engines, ", "))
	}
	models, implicit, err := j.candidates(req, engine)
	if err != nil {
		return nil, core.Cost{}, err
	}
	// The strategy is read only for a local judge: Jev never sees it (the
	// step notes it, see classifyJev). When no candidate can be Jev, a bad
	// strategy fails before any model is probed; otherwise it is read once
	// the pick is local.
	var mode llmjudge.ModeKind
	canJev := implicit
	for _, m := range models {
		canJev = canJev || strings.HasPrefix(canonModel(m), jevPrefix)
	}
	if !canJev {
		if mode, err = modeOf(req.Strategy); err != nil {
			return nil, core.Cost{}, err
		}
	}
	p, err := j.m.pick(ctx, pickIn{
		models: models, implicitJev: implicit, jevOK: true, localOnly: req.LocalOnly,
		run: req.RunID, tenant: req.Tenant, profile: req.Profile,
	})
	if err != nil {
		return nil, core.Cost{}, err
	}
	if p.ref.jev {
		return j.classifyJev(ctx, req, p)
	}
	if canJev {
		if mode, err = modeOf(req.Strategy); err != nil {
			return nil, core.Cost{}, err
		}
	}
	req.Model = p.ref.model
	return j.classifyLocal(ctx, req, mode)
}

// candidates are the models the step tries, in order, and whether Jev follows
// them. The list implies the engine of each entry: a Jev entry is judged by
// Jev, any other by llmjudge. The engine filters it: local keeps the others,
// jev keeps the Jev entries (none: the default Jev model, whatever else the
// step names), auto keeps all. A single model (not a list) under auto that is
// local, or none, is followed by Jev: auto decides between the two only then;
// an explicit cloud model runs as named.
func (j *JudgeClassifier) candidates(req ClassifyRequest, engine string) (models []string, implicitJev bool, err error) {
	listed := req.Models != nil
	given := req.Models
	if !listed && strings.TrimSpace(req.Model) != "" {
		given = []string{req.Model}
	}
	isJev := func(m string) bool { return strings.HasPrefix(canonModel(m), jevPrefix) }
	switch engine {
	case EngineLocal:
		for _, m := range given {
			if !isJev(m) {
				models = append(models, m)
			}
		}
		switch {
		case len(given) == 0:
			return nil, false, core.New(core.CodeInputInvalid, "engine local needs a model").
				WithSuggestion("write model: ollama://<model>, or use engine jev")
		case len(models) == 0:
			return nil, false, core.New(core.CodeInputInvalid, "engine local, but model names only jev").
				WithSuggestion("name a local model, or use engine jev or auto")
		}
		return models, false, nil
	case EngineJev:
		for _, m := range given {
			if isJev(m) {
				models = append(models, m)
			}
		}
		return models, len(models) == 0, nil
	}
	if listed || len(given) > 0 && isJev(given[0]) {
		return given, false, nil
	}
	if len(given) == 0 {
		return nil, true, nil
	}
	level, perr := node.ModelPrivacy(canonModel(given[0]), j.m.local)
	return given, perr == nil && level == node.PrivacyLocal, nil
}

// classifyLocal judges over the step's model through llmjudge.
func (j *JudgeClassifier) classifyLocal(ctx context.Context, req ClassifyRequest, mode llmjudge.ModeKind) (Labels, core.Cost, error) {
	pat, err := j.patternOf(req.Pattern)
	if err != nil {
		return nil, core.Cost{}, err
	}
	uri, err := llm.ParseURI(req.Model)
	if err != nil {
		return nil, core.Cost{}, core.Errorf(core.CodeInputInvalid, "model %q is not a model URI", llm.RedactURI(req.Model)).
			WithSuggestion("write scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	cases, err := casesOf(req)
	if err != nil {
		return nil, core.Cost{}, err
	}
	// The ceiling comes before any client: a refused call reads no secret
	// and sends nothing. The node's ModelPrivacy and kit's local-only
	// resolution decide the same set of models.
	level, perr := node.ModelPrivacy(req.Model, j.m.local)
	if errors.Is(perr, node.ErrNotRunnable) {
		return nil, core.Cost{}, core.Errorf(core.CodePrivacyViolation, "model %q: %v", llm.RedactURI(req.Model), perr).
			WithSuggestion("write ollama://<model> for the local daemon or scheme://host:port/<model>")
	}
	if req.LocalOnly && (perr != nil || level == node.PrivacyCloud) {
		return nil, core.Cost{}, core.Errorf(core.CodePrivacyViolation, "model %q is not local under a local-only ceiling", llm.RedactURI(req.Model)).
			WithSuggestion("name a local model (ollama://<model>, a local scheme://host:port/<model>), list the host in privacy.local_hosts, or set privacy: cloud-allowed")
	}
	client, err := j.m.open(ctx, req.Model, uri, level, req.LocalOnly, req.Tenant, req.Profile)
	if err != nil {
		return nil, core.Cost{}, err
	}
	defer client.Provider().Close() //nolint:errcheck // adapters hold nothing open between calls

	opts := []llmjudge.Option{llmjudge.Model(uri.Model), llmjudge.Mode(mode)}
	if pat != nil {
		opts = append(opts, llmjudge.Pattern(pat))
	}
	if p, ok := j.m.price(ctx, level, uri); ok {
		opts = append(opts, llmjudge.Price(p))
	}
	verdicts, err := judge.Batch(ctx, llmjudge.New(client, opts...), cases, judgeParallel)
	if err != nil {
		if errors.Is(err, judge.ErrInvalid) {
			return nil, core.Cost{}, core.Errorf(core.CodeInputInvalid, "%v", err).
				WithSuggestion("labels needs at least two distinct labels, none of them \"abstain\"")
		}
		return nil, core.Cost{}, upstreamErr(err)
	}
	out := make(Labels, len(verdicts))
	var cost core.Cost
	for i, v := range verdicts {
		a := v.Answers[judgeQuestion]
		out[i] = Label{Confidence: a.Confidence, Why: a.Why, Engine: EngineLocal, Model: llm.RedactURI(req.Model)}
		if !a.Abstained {
			value := a.Value
			out[i].Label = &value
		}
		cost.USD += v.Cost.USD
		cost.Tokens += tokensOf(v.Usage)
	}
	return out, cost, nil
}

// casesOf is one case per item, in order: the item is the case's state and
// the question is the closed label set.
func casesOf(req ClassifyRequest) ([]judge.Case, error) {
	opts := make([]judge.Option, len(req.Labels))
	for i, l := range req.Labels {
		opts[i] = judge.Option{Value: l}
	}
	q := judge.Question{
		Name: judgeQuestion, Kind: judge.KindChoice, Instructions: req.Instructions,
		Options: opts, Abstain: req.Abstain,
	}
	cases := make([]judge.Case, len(req.Items))
	for i, it := range req.Items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, core.Errorf(core.CodeInputInvalid, "item %d is not a map", i)
		}
		cases[i] = judge.Case{State: m, Questions: []judge.Question{q}}
	}
	return cases, nil
}

// modeOf reads the `strategy` input: "" or "verbalized" (one sample, the
// model's stated confidence) or "votes" / "votes:<n>" (n samples, confidence
// the share of samples that agree).
func modeOf(s string) (llmjudge.ModeKind, error) {
	bad := func() error {
		return core.Errorf(core.CodeInputInvalid, "strategy %q is not known to the judge backend", s).
			WithSuggestion("use verbalized, votes or votes:<n> (n from 2 to " + strconv.Itoa(judgeMaxVotes) + ")")
	}
	switch name, arg, hasArg := strings.Cut(strings.TrimSpace(s), ":"); {
	case s == "" || (name == "verbalized" && !hasArg):
		return llmjudge.Verbalized, nil
	case name == "votes" && !hasArg:
		return llmjudge.Votes(judgeDefaultVotes), nil
	case name == "votes":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 2 || n > judgeMaxVotes {
			return llmjudge.ModeKind{}, bad()
		}
		return llmjudge.Votes(n), nil
	}
	return llmjudge.ModeKind{}, bad()
}

// patternOf loads the named pattern; none named is none.
func (j *JudgeClassifier) patternOf(name string) (*prompt.Pattern, error) {
	if name == "" {
		return nil, nil
	}
	if j.m.d.Patterns == nil {
		return nil, core.Errorf(core.CodeEngineFeature, "pattern %q: no pattern source is configured for the judge backend", name).
			WithSuggestion("drop pattern, or configure a pattern source")
	}
	p, err := j.m.d.Patterns.Load(name)
	if err != nil {
		if errors.Is(err, prompt.ErrNotFound) {
			return nil, core.Errorf(core.CodeInputInvalid, "pattern %q: %v", name, err)
		}
		return nil, core.Wrap(core.CodeInternal, err)
	}
	return p, nil
}
