package node

import (
	"os"
	"strings"
	"testing"

	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
)

// celRender renders against spec/cel. `in` is a reserved word of CEL, so
// the test binds the inputs as `vars` and rewrites the namespace.
func celRender(t *testing.T) RenderFunc {
	t.Helper()
	env, err := cel.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	return func(tmpl string, scope map[string]any) (any, error) {
		tp, err := env.CompileTemplate(strings.ReplaceAll(tmpl, "in.", "vars."))
		if err != nil {
			return nil, err
		}
		act := cel.Activation{"vars": scope["in"], "config": scope["config"], "run": scope["run"]}
		return tp.Render(act)
	}
}

func loadExample(t *testing.T, name string) *Spec {
	t.Helper()
	f, err := os.Open("testdata/examples/" + name) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	s, err := Load(f, name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoweringTargets(t *testing.T) {
	want := map[string]LowersTo{
		"heo_conversation.list_1.yaml": LowersExec, // plain stdout
		"tlc_task.list_1.yaml":         LowersExec,
		"tlc_task.create_1.yaml":       LowersExec,
		"gh_advisories_1.yaml":         LowersEngine, // stdin
		"inbox_apply_1.yaml":           LowersEngine, // irreversible
		"inbox_plan_1.yaml":            LowersEngine, // map
		"ctxt_find_1.yaml":             LowersEngine, // http
		"calendar_events_1.yaml":       LowersEngine, // plugin
	}
	for f, to := range want {
		if got := loadExample(t, f).Lowering(); got != to {
			t.Errorf("%s lowers to %s, want %s", f, got, to)
		}
	}
	// With lowers_to absent the target is derived, not blindly exec.
	s := loadExample(t, "gh_advisories_1.yaml")
	s.LowersTo = ""
	if s.Lowering() != LowersEngine {
		t.Error("a command with stdin must derive to engine")
	}
	s = loadExample(t, "tlc_task.list_1.yaml")
	s.LowersTo = ""
	if s.Lowering() != LowersExec {
		t.Error("a plain-stdout command derives to exec")
	}
}

func TestLowerToExecStep(t *testing.T) {
	s := loadExample(t, "heo_conversation.list_1.yaml")
	st := spec.Step{
		ID: "threads", Uses: "heo/conversation.list@1", DependsOn: []string{"people"}, When: "x == 1",
		With: map[string]any{"participants": []any{"a@x.test", "b@x.test"}, "since": "30d"},
	}
	got, err := Lower(s, st, celRender(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.To != LowersExec || got.Step == nil {
		t.Fatalf("lowered = %+v", got)
	}
	ex := got.Step
	if ex.Kind != spec.KindExec || ex.Uses != "" || ex.With != nil {
		t.Errorf("step shape: %+v", ex)
	}
	if ex.ID != "threads" || ex.DependsOn[0] != "people" || ex.When != "x == 1" {
		t.Errorf("the step keeps its identity: %+v", ex)
	}
	wantArgv := "heo conversation list --participants a@x.test b@x.test --since 30d --format json"
	// A list-valued whole-element placeholder expands into arguments (§4.1).
	if strings.Join(ex.Exec.Argv, " ") != wantArgv || len(ex.Exec.Argv) != 10 {
		t.Errorf("argv = %q, want %q", strings.Join(ex.Exec.Argv, " "), wantArgv)
	}
	if ex.Exec.Timeout != DefaultTimeout && ex.Exec.Timeout != s.Timeout {
		t.Errorf("timeout = %q", ex.Exec.Timeout)
	}
}

func TestLowerKeyAndErrors(t *testing.T) {
	s := loadExample(t, "tlc_task.create_1.yaml")
	st := spec.Step{ID: "up", Uses: "tlc/task.create@1", With: map[string]any{"title": "T", "description": "D"}}
	got, err := Lower(s, st, celRender(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if k := got.Step.Exec.Env["FLO_IDEMPOTENCY_KEY"]; k != "T" {
		t.Errorf("idempotency key env = %q, want the rendered key", k)
	}
	if _, err := Lower(s, spec.Step{ID: "x", With: map[string]any{"nope": 1}}, celRender(t), nil, nil); err == nil {
		t.Error("a with key outside the inputs must fail")
	}
	if _, err := Lower(s, spec.Step{ID: "x", With: map[string]any{"description": "D"}}, celRender(t), nil, nil); err == nil {
		t.Error("a missing required input must fail")
	}
	// An engine node is reported, not expanded.
	e := loadExample(t, "gh_advisories_1.yaml")
	got, err = Lower(e, spec.Step{ID: "adv"}, celRender(t), nil, nil)
	if err != nil || got.To != LowersEngine || got.Step != nil {
		t.Errorf("engine node: %+v %v", got, err)
	}
}

func TestLowerEnvCwdNullAndDefaults(t *testing.T) {
	s := &Spec{
		Node: "acme/x@1", Kind: KindCommand, LowersTo: LowersExec, SideEffect: SideEffectRead,
		Argv:   []string{"x", "{{ in.a }}", "{{ in.n }}", "{{ in.items }}"},
		Env:    map[string]string{"H": "{{ config.acme.home }}"},
		Cwd:    "{{ config.acme.root }}",
		Inputs: map[string]InputDef{"a": {Default: "dflt"}, "n": {}, "items": {}},
		Retry:  &Retry{MaxAttempts: 3, Backoff: "2s"},
	}
	st := spec.Step{ID: "s", With: map[string]any{"n": nil, "items": []any{"p", "q"}}, Timeout: "5s"}
	cfg := map[string]any{"acme": map[string]any{"home": "/h", "root": "/r"}}
	got, err := Lower(s, st, celRender(t), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ex := got.Step.Exec
	if strings.Join(ex.Argv, "|") != "x|dflt||p|q" {
		t.Errorf("argv = %q; default applied, null empty, list expanded", strings.Join(ex.Argv, "|"))
	}
	if ex.Cwd != "/r" || ex.Env["H"] != "/h" || ex.Timeout != "5s" {
		t.Errorf("exec = %+v", ex)
	}
	if got.Step.Retry == nil || got.Step.Retry.MaxAttempts != 3 {
		t.Errorf("the node's retry is the step's default: %+v", got.Step.Retry)
	}
}
