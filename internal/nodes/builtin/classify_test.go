package builtin_test

import (
	"context"
	"reflect"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
)

func withClassifier(t *testing.T, c builtin.Classifier) *builtin.Registry {
	t.Helper()
	r, _ := std(t)
	if err := builtin.RegisterClassify(r, c); err != nil {
		t.Fatal(err)
	}
	return r
}

func classifyIn(items ...any) map[string]any {
	return map[string]any{
		"items": append([]any{}, items...), "labels": []any{"security", "breaking", "routine", "noise"},
		"model": "anthropic://claude-haiku-5-5",
	}
}

func labelsOf(res core.Result) []any {
	var out []any
	for _, x := range res.Output.(map[string]any)["items"].([]any) {
		out = append(out, x.(map[string]any)["label"])
	}
	return out
}

func TestClassifyRules(t *testing.T) {
	r := withClassifier(t, builtin.RulesClassifier{})
	res := run(t, r, "flo/ai.classify@1", classifyIn(
		map[string]any{"ghsa_id": "GHSA-x", "package": "a"},
		map[string]any{"severity": "high"},
		map[string]any{"package": "b", "installed": "1.9.0", "version": "2.0.0"},
		map[string]any{"package": "c", "installed": "v2.1.0", "version": "2.2.0"},
		map[string]any{"package": "d", "version": "9.0.0"},
		map[string]any{"package": "kit", "installed": "0.5.0-beta.10", "version": "0.5.0-beta.14"},
	))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("%+v", res.Err)
	}
	want := []any{"security", "security", "breaking", "routine", "routine", "routine"}
	if got := labelsOf(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	first := res.Output.(map[string]any)["items"].([]any)[0].(map[string]any)
	if first["item"].(map[string]any)["package"] != "a" || first["confidence"] != 1.0 || first["why"] == "" {
		t.Fatalf("row = %v", first)
	}
	if !res.Cost.IsZero() {
		t.Errorf("rules cost %+v", res.Cost)
	}
}

func TestClassifyEmptyNeedsNoBackend(t *testing.T) {
	r := withClassifier(t, nil)
	res := run(t, r, "flo/ai.classify@1", classifyIn())
	if res.Outcome != core.OutcomeOK || len(res.Output.(map[string]any)["items"].([]any)) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestClassifyWithoutBackendFails(t *testing.T) {
	r := withClassifier(t, nil)
	res := run(t, r, "flo/ai.classify@1", classifyIn(map[string]any{"version": "1.0.0"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature {
		t.Fatalf("a missing backend is not replaced: %+v", res)
	}
}

func TestClassifyLabelOutsideSet(t *testing.T) {
	r := withClassifier(t, builtin.RulesClassifier{})
	in := classifyIn(map[string]any{"version": "1.0.0"})
	in["labels"] = []any{"security", "breaking"}
	if res := run(t, r, "flo/ai.classify@1", in); res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
		t.Fatalf("routine is not offered: %+v", res)
	}
	in["abstain"] = true
	res := run(t, r, "flo/ai.classify@1", in)
	if res.Outcome != core.OutcomeOK || labelsOf(res)[0] != nil {
		t.Fatalf("abstain: %+v", res)
	}
}

type fixed struct {
	out  builtin.Labels
	cost core.Cost
	err  error
}

func (f fixed) Classify(context.Context, builtin.ClassifyRequest) (builtin.Labels, core.Cost, error) {
	return f.out, f.cost, f.err
}

func TestClassifyChecksBackendAnswers(t *testing.T) {
	bad := "other"
	for name, f := range map[string]fixed{
		"short":      {out: builtin.Labels{}},
		"not in set": {out: builtin.Labels{{Label: &bad}}},
		"no label":   {out: builtin.Labels{{}}},
	} {
		r := withClassifier(t, f)
		res := run(t, r, "flo/ai.classify@1", classifyIn(map[string]any{"version": "1"}))
		if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid {
			t.Errorf("%s: %+v", name, res)
		}
	}
	ok := "noise"
	r := withClassifier(t, fixed{out: builtin.Labels{{Label: &ok, Confidence: .5, Why: "m"}}, cost: core.Cost{USD: .01, Tokens: 5}})
	res := run(t, r, "flo/ai.classify@1", classifyIn(map[string]any{"version": "1"}))
	if res.Outcome != core.OutcomeOK || res.Cost.USD != .01 || res.Cost.Tokens != 5 {
		t.Fatalf("cost is reported: %+v", res)
	}
}

func TestClassifierFor(t *testing.T) {
	if c, err := builtin.ClassifierFor("", builtin.LLMDeps{}); c != nil || err != nil {
		t.Fatalf("unset: %v %v", c, err)
	}
	if c, err := builtin.ClassifierFor("rules", builtin.LLMDeps{}); c == nil || err != nil {
		t.Fatalf("rules: %v %v", c, err)
	}
	if c, err := builtin.ClassifierFor("judge", builtin.LLMDeps{}); c == nil || err != nil {
		t.Fatalf("judge: %v %v", c, err)
	}
	if c, err := builtin.ClassifierFor("judge", builtin.LLMDeps{LocalHosts: []string{"bad host!"}}); c != nil || !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("a bad privacy.local_hosts is a usage error: %v %v", c, err)
	}
	if _, err := builtin.ClassifierFor("nope", builtin.LLMDeps{}); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("unknown: %v", err)
	}
}
