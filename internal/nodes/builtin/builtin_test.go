package builtin_test

import (
	"context"
	"reflect"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/schema"
)

func std(t *testing.T) (*builtin.Registry, *coretest.Fixture) {
	t.Helper()
	fx := coretest.New()
	r := builtin.NewRegistry()
	if err := builtin.RegisterStd(r, builtin.Deps{Outputs: fx.DB.Outputs, IDs: fx.IDs, Clock: fx.Clock}); err != nil {
		t.Fatal(err)
	}
	return r, fx
}

// run validates the input as the dispatcher does, then executes.
func run(t *testing.T, r *builtin.Registry, ref string, in map[string]any) core.Result {
	t.Helper()
	n, ok := r.Lookup(ref)
	if !ok {
		t.Fatalf("%s not registered", ref)
	}
	full, err := schema.ApplyInputs(n.Spec().Inputs, in)
	if err != nil {
		t.Fatal(err)
	}
	res, xerr := n.Execute(context.Background(), core.Invocation{Tenant: "default", Run: core.RunContext{ID: "run_1"}, StepID: "s", Node: n.Spec(), In: full})
	if xerr != nil {
		t.Fatal(xerr)
	}
	if res.Outcome == core.OutcomeOK {
		if cerr := schema.CheckOutput(n.Spec(), res.Output); cerr != nil {
			t.Fatalf("output must match the node's own schema: %v", cerr)
		}
	}
	return res
}

func TestRegistryListsSpecs(t *testing.T) {
	r, _ := std(t)
	var refs []string
	for _, n := range r.List() {
		refs = append(refs, n.Spec().Node)
	}
	if want := []string{"flo/dedupe@1", "flo/filter@1", "flo/output@1"}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %v", refs)
	}
	err := builtin.RegisterStd(r, builtin.Deps{Outputs: coretest.NewDB(coretest.NewClock(coretest.Epoch)).Outputs, IDs: &coretest.IDs{}, Clock: coretest.NewClock(coretest.Epoch)})
	if !core.IsCode(err, core.CodeNodeConflict) {
		t.Fatalf("second registration: %v", err)
	}
}

func TestDedupeFirstWins(t *testing.T) {
	r, _ := std(t)
	items := []any{
		map[string]any{"id": "a", "n": 1}, map[string]any{"id": "b", "n": 2},
		map[string]any{"id": "a", "n": 3}, map[string]any{"id": "c", "n": 4},
	}
	res := run(t, r, "flo/dedupe@1", map[string]any{"items": items, "key": "{{ item.id }}", "seen": []any{"b"}})
	out := res.Output.(map[string]any)
	if got := out["keys"]; !reflect.DeepEqual(got, []any{"a", "c"}) {
		t.Fatalf("keys = %v (b was seen; the second a is a duplicate)", got)
	}
	if first := out["items"].([]any)[0].(map[string]any); first["n"] != 1 {
		t.Fatalf("first wins: %v", first)
	}
	if got := out["seen"]; !reflect.DeepEqual(got, []any{"b", "a", "c"}) {
		t.Fatalf("seen = %v (seen then new keys)", got)
	}
}

func TestDedupeMaxDropsOldest(t *testing.T) {
	r, _ := std(t)
	res := run(t, r, "flo/dedupe@1", map[string]any{
		"items": []any{"x", "y"}, "key": "item", "seen": []any{"p", "q", "r"}, "max": 3,
	})
	out := res.Output.(map[string]any)
	if !reflect.DeepEqual(out["seen"], []any{"r", "x", "y"}) {
		t.Fatalf("seen = %v: the oldest keys go", out["seen"])
	}
	if !reflect.DeepEqual(out["keys"], []any{"x", "y"}) {
		t.Fatalf("keys = %v", out["keys"])
	}
}

func TestDedupeIndexAndNonStringKeys(t *testing.T) {
	r, _ := std(t)
	res := run(t, r, "flo/dedupe@1", map[string]any{"items": []any{"a", "a", "b"}, "key": "{{ item + string(index % 2) }}"})
	if keys := res.Output.(map[string]any)["keys"]; !reflect.DeepEqual(keys, []any{"a0", "a1", "b0"}) {
		t.Fatalf("keys = %v", keys)
	}
	res = run(t, r, "flo/dedupe@1", map[string]any{"items": []any{1, 1, 2}, "key": "item"})
	if keys := res.Output.(map[string]any)["keys"]; !reflect.DeepEqual(keys, []any{"1", "2"}) {
		t.Fatalf("a number key is its JSON text: %v", keys)
	}
}

func TestDedupeEmptyAndBadKey(t *testing.T) {
	r, _ := std(t)
	res := run(t, r, "flo/dedupe@1", map[string]any{"items": []any{}, "key": "item"})
	out := res.Output.(map[string]any)
	if len(out["items"].([]any)) != 0 || len(out["seen"].([]any)) != 0 {
		t.Fatalf("out = %v", out)
	}
	res = run(t, r, "flo/dedupe@1", map[string]any{"items": []any{"a"}, "key": "item.nope.deeper"})
	if res.Outcome != core.OutcomeFailed || res.Effect != core.EffectNone {
		t.Fatalf("res = %+v", res)
	}
}

func TestFilter(t *testing.T) {
	r, _ := std(t)
	items := []any{map[string]any{"label": "noise"}, map[string]any{"label": "security"}, map[string]any{"label": "bug"}}
	res := run(t, r, "flo/filter@1", map[string]any{"items": items, "where": "{{ item.label != 'noise' }}"})
	if got := res.Output.(map[string]any)["items"].([]any); len(got) != 2 {
		t.Fatalf("items = %v", got)
	}
	res = run(t, r, "flo/filter@1", map[string]any{"items": items, "where": "index == 0"})
	if got := res.Output.(map[string]any)["items"].([]any); len(got) != 1 {
		t.Fatalf("index is bound: %v", got)
	}
	res = run(t, r, "flo/filter@1", map[string]any{"items": items, "where": "item.label"})
	if res.Outcome != core.OutcomeFailed {
		t.Fatalf("a non-boolean condition fails: %+v", res)
	}
}

func TestOutputWritesToStore(t *testing.T) {
	r, fx := std(t)
	res := run(t, r, "flo/output@1", map[string]any{
		"kind": "findings", "title": "Deps",
		"items": []any{map[string]any{"title": "x 1.2", "label": "security", "refs": []any{"a/b"}}},
	})
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("res = %+v", res.Err)
	}
	got, err := fx.DB.Outputs.ListOutputs(context.Background(), "default", core.OutputFilter{Run: "run_1"})
	if err != nil || len(got) != 1 || got[0].Kind != core.OutputFindings || len(got[0].Items) != 1 || got[0].StepID != "s" {
		t.Fatalf("outputs = %+v err=%v", got, err)
	}
	if string(got[0].ID) != res.Output.(map[string]any)["id"] {
		t.Fatal("output id is returned")
	}
	run(t, r, "flo/output@1", map[string]any{"kind": "brief", "title": "T", "body": "text"})
	if got, _ = fx.DB.Outputs.ListOutputs(context.Background(), "default", core.OutputFilter{Kind: core.OutputBrief}); len(got) != 1 || got[0].Body != "text" {
		t.Fatalf("brief = %+v", got)
	}
}

func TestOutputValidation(t *testing.T) {
	r, _ := std(t)
	n, _ := r.Lookup("flo/output@1")
	for name, in := range map[string]map[string]any{
		"unknown kind":          {"kind": "other"},
		"finding without title": {"kind": "findings", "items": []any{map[string]any{"label": "x"}}},
	} {
		if _, err := schema.ApplyInputs(n.Spec().Inputs, in); err == nil || err.Code != core.CodeInputInvalid {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// what the schema cannot say is the node's check
	for name, in := range map[string]map[string]any{
		"brief without body":    {"kind": "brief", "title": "T"},
		"findings without list": {"kind": "findings"},
	} {
		res, err := n.Execute(context.Background(), core.Invocation{In: in})
		if err != nil || res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
			t.Fatalf("%s: res=%+v err=%v", name, res, err)
		}
	}
}
