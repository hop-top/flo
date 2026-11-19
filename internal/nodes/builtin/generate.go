package builtin

import (
	"context"
	"errors"
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
	// Model is the step's model when it gave a string, Models its models
	// when it gave a list, in order; at most one is set. The llm backend
	// tries them in order; the template backend ignores both.
	Model  string
	Models []string
	Schema map[string]any
	// LocalOnly is set when the step runs under a local-only ceiling. The
	// node has already refused a cloud model; a backend that picks its own
	// endpoint must stay local too.
	LocalOnly bool
	// Level is the model's privacy level as the node computed it (flo's
	// ModelPrivacy): local or cloud. Empty when the model is not a URI.
	Level node.Privacy
	// Tenant and Profile scope the secrets a backend reads: the run's
	// tenant and the aps profile the step acts as.
	Tenant  core.TenantID
	Profile string
	// RunID scopes what a backend remembers for one run (the llm backend's
	// detection of a local model). Empty remembers nothing.
	RunID string
}

// Generation is a backend answer. Data is set when the request had a Schema.
// Model is the model that answered, as a URI without secrets; empty when the
// backend has none.
type Generation struct {
	Text  string
	Data  map[string]any
	Model string
}

// Generator is the seam behind flo/ai.generate@1. LLMGenerator (kit's model
// client) implements it; TemplateGenerator is the deterministic backend for
// runs without a model.
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
func RegisterGenerate(r core.BuiltinRegistry, g Generator, src PromptSource, local *node.LocalHosts) error {
	s, err := Spec("flo/ai.generate@1")
	if err != nil {
		return err
	}
	n := fn{s, func(ctx context.Context, inv core.Invocation) (core.Result, error) {
		return generate(ctx, g, src, local, inv)
	}}
	return r.Register(n) //nolint:wrapcheck // *core.Error
}

func generate(ctx context.Context, g Generator, src PromptSource, local *node.LocalHosts, inv core.Invocation) (core.Result, error) {
	ref := asString(inv.In["prompt"])
	if ref == "" {
		return pureFail(core.Errorf(core.CodeInputInvalid, "prompt is empty")), nil
	}
	models, listed, merr := modelsOf(inv.In["model"])
	if merr == nil && len(models) == 0 {
		merr = core.New(core.CodeInputInvalid, "model is empty")
	}
	if merr != nil {
		return pureFail(merr), nil
	}
	var ctxIn map[string]any
	if v, present := inv.In["context"]; present && v != nil {
		var ok bool
		if ctxIn, ok = v.(map[string]any); !ok {
			return pureFail(core.Errorf(core.CodeInputInvalid, "context is not an object")), nil
		}
	}
	// The ceiling comes before any backend: a refused call sends nothing.
	// An invocation that carries no ceiling is read as the strictest. A list
	// is refused here only when no model of it can pass; the backend skips
	// the ones that cannot.
	localOnly := inv.Privacy != specrun.PrivacyCloudAllowed
	var lvl node.Privacy
	refused := 0
	for _, model := range models {
		if strings.HasPrefix(canonModel(model), jevPrefix) {
			return pureFail(core.Errorf(core.CodeInputInvalid, "model %q is a judge model: only flo/ai.classify@1 runs it", model).
				WithSuggestion("name a model of the llm backend, e.g. ollama://gemma4:e4b")), nil
		}
		l, perr := node.ModelPrivacy(model, local)
		if errors.Is(perr, node.ErrNotRunnable) {
			return pureFail(core.Errorf(core.CodePrivacyViolation, "model %q: %v", model, perr).
				WithSuggestion("write ollama://<model> for the local daemon or scheme://host:port/<model>")), nil
		}
		if lvl == "" {
			lvl = l
		}
		if localOnly && (perr != nil || l == node.PrivacyCloud) {
			refused++
		}
	}
	if refused == len(models) {
		what := fmt.Sprintf("model %q is not local", models[0])
		if listed {
			what = "no model of the list is local"
		}
		return pureFail(core.Errorf(core.CodePrivacyViolation, "%s under a local-only ceiling", what).
			WithSuggestion("name a local model (ollama://<model>, a local scheme://host:port/<model>), list the host in privacy.local_hosts, or set privacy: cloud-allowed")), nil
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
	req := GenerateRequest{
		Prompt: p, Context: ctxIn, LocalOnly: localOnly,
		Level: lvl, Tenant: inv.Tenant, Profile: inv.RunAs, RunID: string(inv.Run.ID),
	}
	if listed {
		req.Models = models
	} else {
		req.Model = models[0]
	}
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
	if got.Model != "" {
		out["model"] = got.Model
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
	// BackendLLM calls the step's model through kit's model client.
	BackendLLM = "llm"
)

// GeneratorFor resolves the `ai.generator` setting. Empty is no backend
// (nil, nil): the node fails when used. d is what the "llm" backend needs;
// the template backend ignores it.
func GeneratorFor(name string, d LLMDeps) (Generator, error) {
	switch name {
	case "":
		return nil, nil
	case BackendTemplate:
		return TemplateGenerator{}, nil
	case BackendLLM:
		g, err := NewLLMGenerator(d)
		if err != nil {
			return nil, err
		}
		return g, nil
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
