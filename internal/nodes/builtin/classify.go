package builtin

import (
	"context"
	"fmt"
	"slices"

	"hop.top/flo/internal/core"
)

// ClassifyRequest is what flo/ai.classify@1 asks a backend (node.md §6.3).
// Items is never empty: the node answers an empty list itself.
type ClassifyRequest struct {
	Items        []any
	Labels       []string
	Model        string
	Instructions string
	Pattern      string
	Strategy     string
	Schema       map[string]any
	Abstain      bool
}

// Label is one backend answer. Label nil is an abstention.
type Label struct {
	Label      *string
	Confidence float64
	Why        string
	// Extra holds the fields the request's Schema asked for.
	Extra map[string]any
}

// Labels are the answers to a request, one per item, in order.
type Labels []Label

// Classifier is the seam behind flo/ai.classify@1. kit's go/ai/judge
// implements it once it exists; RulesClassifier is the deterministic
// backend for runs without a model.
type Classifier interface {
	Classify(ctx context.Context, req ClassifyRequest) (Labels, core.Cost, error)
}

// RegisterClassify registers flo/ai.classify@1 over c. A nil c registers the
// node with no backend: a step that uses it fails with engine_feature and
// says how to configure one; nothing falls back to another backend.
func RegisterClassify(r core.BuiltinRegistry, c Classifier) error {
	s, err := Spec("flo/ai.classify@1")
	if err != nil {
		return err
	}
	n := fn{s, func(ctx context.Context, inv core.Invocation) (core.Result, error) { return classify(ctx, c, inv) }}
	return r.Register(n) //nolint:wrapcheck // *core.Error
}

func classify(ctx context.Context, c Classifier, inv core.Invocation) (core.Result, error) {
	items, ok := list(inv.In["items"])
	if !ok {
		return pureFail(core.Errorf(core.CodeInputInvalid, "items is not a list")), nil
	}
	labelList, _ := list(inv.In["labels"])
	labels := make([]string, 0, len(labelList))
	for _, l := range labelList {
		s, isStr := l.(string)
		if !isStr || s == "" {
			return pureFail(core.Errorf(core.CodeInputInvalid, "labels must be non-empty strings")), nil
		}
		labels = append(labels, s)
	}
	if len(labels) == 0 {
		return pureFail(core.Errorf(core.CodeInputInvalid, "labels is empty")), nil
	}
	if len(items) == 0 {
		return pureOK(map[string]any{"items": []any{}}), nil
	}
	if c == nil {
		return pureFail(core.New(core.CodeEngineFeature, "flo/ai.classify@1 has no backend configured").
			WithSuggestion("set ai.classifier (env " + EnvClassifier + ") to a backend: rules")), nil
	}
	req := ClassifyRequest{
		Items: items, Labels: labels, Model: asString(inv.In["model"]),
		Instructions: asString(inv.In["instructions"]), Pattern: asString(inv.In["pattern"]),
		Strategy: asString(inv.In["strategy"]),
	}
	req.Schema, _ = inv.In["schema"].(map[string]any)
	req.Abstain, _ = inv.In["abstain"].(bool)

	got, cost, err := c.Classify(ctx, req)
	if err != nil {
		var ce *core.Error
		if asCore(err, &ce) {
			return core.Result{Outcome: core.OutcomeFailed, Err: ce, Effect: core.EffectNone, Cost: cost}, nil
		}
		return core.Result{}, err //nolint:wrapcheck // the backend's own error
	}
	if len(got) != len(items) {
		return pureFail(core.Errorf(core.CodeOutputInvalid, "classifier answered %d items for %d", len(got), len(items))), nil
	}
	out := make([]any, len(items))
	for i, g := range got {
		row := map[string]any{"item": items[i], "confidence": g.Confidence, "why": g.Why}
		switch {
		case g.Label == nil && req.Abstain:
			row["label"] = nil
		case g.Label == nil:
			return pureFail(core.Errorf(core.CodeOutputInvalid, "item %d: no label and abstain is off", i)), nil
		case !slices.Contains(labels, *g.Label):
			return pureFail(core.Errorf(core.CodeOutputInvalid, "item %d: label %q is not in %v", i, *g.Label, labels)), nil
		default:
			row["label"] = *g.Label
		}
		for k, v := range g.Extra {
			if _, taken := row[k]; !taken {
				row[k] = v
			}
		}
		out[i] = row
	}
	res := pureOK(map[string]any{"items": out})
	res.Cost = cost
	return res, nil
}

func asCore(err error, target **core.Error) bool {
	ce, ok := err.(*core.Error) //nolint:errorlint // classified errors are returned unwrapped
	if ok {
		*target = ce
	}
	return ok
}

// EnvClassifier names the setting that picks the classify backend.
const EnvClassifier = "FLO_AI_CLASSIFIER"

// Backend names of `ai.classifier`.
const (
	// BackendRules is the deterministic rules classifier: no model, no
	// network. It is an explicit choice, never a fallback.
	BackendRules = "rules"
	// BackendJudge is kit's go/ai/judge; not available until it exists.
	BackendJudge = "judge"
)

// ClassifierFor resolves the `ai.classifier` setting. Empty is no backend
// (nil, nil): the node fails when used. "judge" is refused until kit's
// go/ai/judge is wired, rather than quietly replaced by rules.
func ClassifierFor(name string) (Classifier, error) {
	switch name {
	case "":
		return nil, nil
	case BackendRules:
		return RulesClassifier{}, nil
	case BackendJudge:
		return nil, core.New(core.CodeEngineFeature, "ai.classifier=judge: the judge backend is not available in this build").
			WithSuggestion("use ai.classifier=rules, or leave it unset")
	}
	return nil, core.Errorf(core.CodeUsage, "ai.classifier=%s: unknown backend (%s)", name, fmt.Sprint([]string{BackendRules, BackendJudge}))
}
