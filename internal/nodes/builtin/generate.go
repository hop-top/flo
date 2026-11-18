package builtin

import (
	"context"
	"fmt"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/prompts"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

// GenerateRequest is what flo/ai.generate@1 asks a backend (node.md §6.5):
// the prompt, loaded, and its context.
type GenerateRequest struct {
	Prompt  *prompts.Prompt
	Context map[string]any
	Model   string
	Schema  map[string]any
	// LocalOnly is set when the step runs under a local-only ceiling. The
	// node has already refused a cloud model; a backend that picks its own
	// endpoint must stay local too.
	LocalOnly bool
}

// Generation is a backend answer. Data is set when the request had a Schema.
type Generation struct {
	Text string
	Data map[string]any
}

// Generator is the seam behind flo/ai.generate@1. kit's model client
// implements it once it exists; TemplateGenerator is the deterministic
// backend for runs without a model.
type Generator interface {
	Generate(ctx context.Context, req GenerateRequest) (Generation, core.Cost, error)
}

// PromptSource resolves a prompt reference for a bundle. prompts.Loader is
// the standard one.
type PromptSource interface {
	Load(bundle, ref string) (*prompts.Prompt, error)
}

// RegisterGenerate registers flo/ai.generate@1 over g, loading prompts
// through src. A nil g registers the node with no backend: a step that
// uses it fails with engine_feature and says how to configure one; nothing
// falls back to another backend.
func RegisterGenerate(r core.BuiltinRegistry, g Generator, src PromptSource) error {
	s, err := Spec("flo/ai.generate@1")
	if err != nil {
		return err
	}
	n := fn{s, func(ctx context.Context, inv core.Invocation) (core.Result, error) { return generate(ctx, g, src, inv) }}
	return r.Register(n) //nolint:wrapcheck // *core.Error
}

func generate(ctx context.Context, g Generator, src PromptSource, inv core.Invocation) (core.Result, error) {
	ref := asString(inv.In["prompt"])
	if ref == "" {
		return pureFail(core.Errorf(core.CodeInputInvalid, "prompt is empty")), nil
	}
	model := asString(inv.In["model"])
	var ctxIn map[string]any
	if v, present := inv.In["context"]; present && v != nil {
		var ok bool
		if ctxIn, ok = v.(map[string]any); !ok {
			return pureFail(core.Errorf(core.CodeInputInvalid, "context is not an object")), nil
		}
	}
	// The ceiling comes before any backend: a refused call sends nothing.
	// An invocation that carries no ceiling is read as the strictest.
	localOnly := inv.Privacy != specrun.PrivacyCloudAllowed
	if lvl, ok := node.ModelPrivacy(model); localOnly && (!ok || lvl == node.PrivacyCloud) {
		return pureFail(core.Errorf(core.CodePrivacyViolation, "model %q is not local under a local-only ceiling", model).
			WithSuggestion("name a local model (ollama://, a local openai://host:port) or set privacy: cloud-allowed")), nil
	}
	if g == nil {
		return pureFail(core.New(core.CodeEngineFeature, "flo/ai.generate@1 has no backend configured").
			WithSuggestion("set ai.generator (env " + EnvGenerator + ") to a backend: " + BackendTemplate)), nil
	}
	if src == nil {
		return pureFail(core.New(core.CodeInternal, "flo/ai.generate@1 has no prompt source")), nil
	}
	p, err := src.Load(inv.Bundle, ref)
	if err != nil {
		return failOf(err, core.Cost{})
	}
	req := GenerateRequest{Prompt: p, Context: ctxIn, Model: model, LocalOnly: localOnly}
	req.Schema, _ = inv.In["schema"].(map[string]any)

	got, cost, err := g.Generate(ctx, req)
	if err != nil {
		return failOf(err, cost)
	}
	if strings.TrimSpace(got.Text) == "" {
		return core.Result{Outcome: core.OutcomeFailed, Cost: cost, Effect: core.EffectNone,
			Err: core.Errorf(core.CodeNodeReportedFailure, "prompt %s produced no text", p.Ref)}, nil
	}
	out := map[string]any{
		"text":   got.Text,
		"prompt": map[string]any{"name": p.Name, "version": p.Version, "hash": p.Hash},
	}
	if req.Schema != nil {
		if got.Data == nil {
			return core.Result{Outcome: core.OutcomeFailed, Cost: cost, Effect: core.EffectNone,
				Err: core.Errorf(core.CodeOutputInvalid, "a schema was given and the generator returned no data")}, nil
		}
		out["data"] = got.Data
	}
	res := pureOK(out)
	res.Cost = cost
	return res, nil
}

// failOf turns a classified backend error into a failed result; anything
// else is the engine's to classify.
func failOf(err error, cost core.Cost) (core.Result, error) {
	var ce *core.Error
	if asCore(err, &ce) {
		return core.Result{Outcome: core.OutcomeFailed, Err: ce, Effect: core.EffectNone, Cost: cost}, nil
	}
	return core.Result{}, err //nolint:wrapcheck // the backend's own error
}

// EnvGenerator names the setting that picks the generate backend.
const EnvGenerator = "FLO_AI_GENERATOR"

// Backend names of `ai.generator`.
const (
	// BackendTemplate renders the prompt's template over the context: no
	// model, no network, no cost. It is an explicit choice, never a fallback.
	BackendTemplate = "template"
	// BackendLLM is a model client; not available until kit's lands.
	BackendLLM = "llm"
)

// GeneratorFor resolves the `ai.generator` setting. Empty is no backend
// (nil, nil): the node fails when used. "llm" is refused until kit's model
// client is wired, rather than quietly replaced by the template backend.
func GeneratorFor(name string) (Generator, error) {
	switch name {
	case "":
		return nil, nil
	case BackendTemplate:
		return TemplateGenerator{}, nil
	case BackendLLM:
		return nil, core.New(core.CodeEngineFeature, "ai.generator=llm: the llm backend is not available in this build").
			WithSuggestion("use ai.generator=template, or leave it unset")
	}
	return nil, core.Errorf(core.CodeUsage, "ai.generator=%s: unknown backend (%s)", name, fmt.Sprint([]string{BackendTemplate, BackendLLM}))
}

// TemplateGenerator is the deterministic backend: it renders the prompt's
// template over the context and returns the text. It has no model, makes no
// call and costs nothing; Model is ignored. With a Schema it cannot produce
// data, and says so.
type TemplateGenerator struct{}

// Generate implements Generator.
func (TemplateGenerator) Generate(_ context.Context, req GenerateRequest) (Generation, core.Cost, error) {
	if req.Schema != nil {
		return Generation{}, core.Cost{}, core.New(core.CodeEngineFeature, "the template backend cannot produce structured data (schema)").
			WithSuggestion("drop schema, or use a model backend")
	}
	text, err := req.Prompt.Render(req.Context)
	if err != nil {
		return Generation{}, core.Cost{}, err
	}
	return Generation{Text: text}, core.Cost{}, nil
}
