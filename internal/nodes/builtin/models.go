package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
	"hop.top/kit/go/ai/llm"
)

// EnvModelsAllow names the setting of the model allow-list (`ai.models.allow`,
// comma-separated in the environment).
const EnvModelsAllow = "FLO_AI_MODELS_ALLOW"

// Model names of Jev: `jev` is the kit default model, written out as
// JevModel; jev://<name> is any other.
const (
	jevShorthand = "jev"
	jevPrefix    = "jev://"
)

// canonModel is the model as the allow-list and the run read it: the `jev`
// shorthand is the kit default Jev model.
func canonModel(s string) string {
	s = strings.TrimSpace(s)
	if s == jevShorthand {
		return JevModel
	}
	return s
}

// ModelAllowed reports whether model is on the allow-list. An entry is an
// exact model URI, or a prefix with a trailing `*`; a `*` alone allows every
// model. A prefix does not reach past the `?` of a model URI (its query holds
// the endpoint, `base_url`), unless the prefix itself does. `jev` stands for
// the default Jev model on both sides.
func ModelAllowed(allow []string, model string) bool {
	m := canonModel(model)
	for _, e := range allow {
		e = canonModel(e)
		if e == "" {
			continue
		}
		p, glob := strings.CutSuffix(e, "*")
		if !glob {
			if e == m {
				return true
			}
			continue
		}
		rest, ok := strings.CutPrefix(m, p)
		if ok && (p == "" || strings.Contains(p, "?") || !strings.Contains(rest, "?")) {
			return true
		}
	}
	return false
}

// modelRef is one model of a step, read.
type modelRef struct {
	// model is the URI as the flow wrote it (the Jev shorthand spelled out).
	model string
	jev   bool
	uri   llm.URI
	level node.Privacy
}

// jevName is the Jev model name the factory is asked for: empty for the kit
// default.
func (r modelRef) jevName() string {
	name := strings.TrimPrefix(r.model, jevPrefix)
	if jevPrefix+name == JevModel {
		return ""
	}
	return name
}

func (r modelRef) shown() string { return llm.RedactURI(r.model) }

// refOf reads one model of a step. A model the run cannot read at all is an
// error, whatever else is in the list: it is not an unavailable model.
func (m *modelClients) refOf(raw string, jevOK bool) (modelRef, error) {
	model := canonModel(raw)
	if model == "" {
		return modelRef{}, core.New(core.CodeInputInvalid, "a model is empty").
			WithSuggestion("write scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	if name, ok := strings.CutPrefix(model, jevPrefix); ok {
		if name == "" || strings.ContainsAny(name, "?#") {
			return modelRef{}, core.Errorf(core.CodeInputInvalid, "model %q is not a Jev model", model).
				WithSuggestion("write jev, or jev://<model>")
		}
		if !jevOK {
			return modelRef{}, core.Errorf(core.CodeInputInvalid, "model %q is a judge model: only flo/ai.classify@1 runs it", model).
				WithSuggestion("name a model of the llm backend, e.g. ollama://gemma4:e4b")
		}
		return modelRef{model: model, jev: true, level: node.PrivacyCloud}, nil
	}
	uri, err := llm.ParseURI(model)
	if err != nil {
		return modelRef{}, core.Errorf(core.CodeInputInvalid, "model %q is not a model URI", llm.RedactURI(model)).
			WithSuggestion("write scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	level, perr := node.ModelPrivacy(model, m.local)
	switch {
	case errors.Is(perr, node.ErrNotRunnable):
		return modelRef{}, core.Errorf(core.CodePrivacyViolation, "model %q: %v", llm.RedactURI(model), perr).
			WithSuggestion("write ollama://<model> for the local daemon or scheme://host:port/<model>")
	case perr != nil:
		return modelRef{}, core.Errorf(core.CodeInputInvalid, "model %q is not a model URI", llm.RedactURI(model)).
			WithSuggestion("write scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	return modelRef{model: model, uri: uri, level: level}, nil
}

// pickIn is what a step asks of the model list.
type pickIn struct {
	// models are the step's models, in order.
	models []string
	// implicitJev puts the default Jev model after them.
	implicitJev bool
	// jevOK is whether the backend runs Jev models.
	jevOK     bool
	localOnly bool
	// run scopes the daemon's model list, remembered per run.
	run     string
	tenant  core.TenantID
	profile string
}

// picked is the model a step runs: key is the OpenRouter key of a Jev model,
// already read.
type picked struct {
	ref modelRef
	key string
}

// why a model of the list is not usable, in the order of significance of the
// error the list fails with when none is.
type skipKind int

const (
	skipNotAllowed skipKind = iota
	skipNotDetected
	skipNoKey
	skipNoJev
	skipCeiling
)

type skip struct {
	ref  modelRef
	kind skipKind
	why  string
}

// pick is the first usable model, in order. Usable is: on the allow-list,
// allowed by the ceiling, and available: a local model the daemon lists
// (asked only when there is a choice to make), a keyed model with its key in
// the store. Only unavailability falls through to the next model; a model
// that cannot be read at all fails the step before any is tried. Nothing is
// sent and no secret read for a model the allow-list or the ceiling refuses.
func (m *modelClients) pick(ctx context.Context, in pickIn) (picked, error) {
	refs := make([]modelRef, 0, len(in.models)+1)
	for _, raw := range in.models {
		r, err := m.refOf(raw, in.jevOK)
		if err != nil {
			return picked{}, err
		}
		refs = append(refs, r)
	}
	if in.implicitJev {
		refs = append(refs, modelRef{model: JevModel, jev: true, level: node.PrivacyCloud})
	}
	if len(refs) == 0 {
		return picked{}, core.New(core.CodeInputInvalid, "no model is given").
			WithSuggestion("write model: scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	if len(m.d.Allow) == 0 {
		return picked{}, core.New(core.CodePrivacyViolation, "no model is allowed: ai.models.allow is not set").
			WithSuggestion("list the model URIs flows may use under ai.models.allow (env " + EnvModelsAllow +
				", comma-separated; a trailing * matches a prefix), e.g. ollama://qwen3*")
	}
	probe := len(refs) > 1
	var skips []skip
	for _, r := range refs {
		switch {
		case !ModelAllowed(m.d.Allow, r.model):
			skips = append(skips, skip{r, skipNotAllowed, "not allowed: not on ai.models.allow"})
			continue
		case in.localOnly && r.level == node.PrivacyCloud:
			skips = append(skips, skip{r, skipCeiling, "refused by the local-only ceiling: it is cloud"})
			continue
		}
		key, sk, err := m.available(ctx, r, in, probe)
		if err != nil {
			return picked{}, err
		}
		if sk != nil {
			skips = append(skips, *sk)
			continue
		}
		return picked{r, key}, nil
	}
	return picked{}, noneUsable(skips)
}

// available says whether the model can be used now: nil skip when it can.
// The error is the caller's to return: a store or a context failure.
func (m *modelClients) available(ctx context.Context, r modelRef, in pickIn, probe bool) (key string, _ *skip, _ error) {
	missing := func(scheme string) (string, *skip, error) {
		return "", &skip{r, skipNoKey, fmt.Sprintf("no key: secret %q not found for profile %q", SecretNameOf(scheme), in.profile)}, nil
	}
	switch {
	case r.jev:
		if m.d.Jev == nil {
			return "", &skip{r, skipNoJev, "jev is not available in this build"}, nil
		}
		key, err := m.secret(ctx, in.tenant, in.profile, jevScheme)
		if core.IsCode(err, core.CodeSecretNotFound) {
			return missing(jevScheme)
		}
		return key, nil, err
	case r.level == node.PrivacyCloud:
		if _, named := r.uri.Params["api_key"]; named || !keyed(r.level, r.uri) {
			return "", nil, nil
		}
		_, err := m.secret(ctx, in.tenant, in.profile, r.uri.Scheme)
		if core.IsCode(err, core.CodeSecretNotFound) {
			return missing(r.uri.Scheme)
		}
		return "", nil, err
	case !probe:
		return "", nil, nil
	case r.uri.Scheme != "ollama":
		return "", &skip{r, skipNotDetected, "not detected: only ollama models can be detected"}, nil
	}
	found, why, err := m.detectOllama(ctx, in.run, r.uri)
	if err != nil {
		return "", nil, err
	}
	if !found {
		return "", &skip{r, skipNotDetected, "not detected: " + why}, nil
	}
	return "", nil, nil
}

// noneUsable is the failure when no model is usable: every model with its
// reason. The code is the most significant reason: the ceiling, a build
// without Jev, a missing key, a model that is not detected, the allow-list.
func noneUsable(skips []skip) error {
	code := core.CodePrivacyViolation
	kinds := map[skipKind]bool{}
	parts := make([]string, len(skips))
	for i, s := range skips {
		parts[i] = s.ref.shown() + ": " + s.why
		kinds[s.kind] = true
	}
	order := []struct {
		kind skipKind
		code core.Code
	}{
		{skipCeiling, core.CodePrivacyViolation},
		{skipNoJev, core.CodeEngineFeature},
		{skipNoKey, core.CodeSecretNotFound},
		{skipNotDetected, core.CodeUpstreamFailed},
		{skipNotAllowed, core.CodePrivacyViolation},
	}
	for _, o := range order {
		if kinds[o.kind] {
			code = o.code
			break
		}
	}
	var hints []string
	if kinds[skipCeiling] {
		hints = append(hints, "name a local model (ollama://<model>, a local scheme://host:port/<model>), list the host in privacy.local_hosts, or set privacy: cloud-allowed")
	}
	if kinds[skipNoKey] {
		hints = append(hints, "store the API key of the model's scheme as the secret <scheme>_api_key of the flow's profile (openrouter_api_key for jev)")
	}
	if kinds[skipNotDetected] {
		hints = append(hints, "run ollama with the model pulled")
	}
	if kinds[skipNotAllowed] {
		hints = append(hints, "add the model to ai.models.allow (env "+EnvModelsAllow+")")
	}
	if kinds[skipNoJev] {
		hints = append(hints, "use a build with jev, or another model")
	}
	e := core.Errorf(code, "no usable model: %s", strings.Join(parts, "; "))
	return e.WithSuggestion(strings.Join(hints, "; "))
}

// modelsOf reads the `model` input: a string, or a list of strings. list is
// whether it was a list. No model (absent, null or blank) is none.
func modelsOf(v any) (models []string, list bool, err *core.Error) {
	switch t := v.(type) {
	case nil:
		return nil, false, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, false, nil
		}
		return []string{t}, false, nil
	case []string:
		v = anys(t)
	}
	l, ok := v.([]any)
	if !ok {
		return nil, false, core.New(core.CodeInputInvalid, "model is not a string or a list of strings")
	}
	if len(l) == 0 {
		return nil, true, core.New(core.CodeInputInvalid, "model is an empty list").
			WithSuggestion("list at least one model, e.g. [ollama://gemma4:e4b]")
	}
	for _, e := range l {
		s, isStr := e.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return nil, true, core.New(core.CodeInputInvalid, "model: every entry of the list is a non-empty string")
		}
		models = append(models, s)
	}
	return models, true, nil
}

func anys(s []string) []any {
	out := make([]any, len(s))
	for i, e := range s {
		out[i] = e
	}
	return out
}
