package run

import (
	"errors"
	"strings"
	"testing"

	"hop.top/flo/spec"
)

func parse(t *testing.T, doc string) *spec.Flow {
	t.Helper()
	f, err := spec.ParseFlow(strings.NewReader(doc), "t.yaml")
	if err != nil {
		t.Fatalf("fixture must parse: %v", err)
	}
	return f
}

// The three worked flows are clean.
func TestCheckExamplesClean(t *testing.T) {
	for _, name := range []string{"dependency-watch", "meeting-brief", "inbox-triage"} {
		if r := Check(exampleFlow(t, name)); len(r) > 0 {
			t.Errorf("%s: %v", name, r)
		}
	}
}

const head = "flow: t\nversion: 1.0.0\n"

// Each fixture breaks one rule and must raise exactly that rule.
func TestCheckRules(t *testing.T) {
	cases := []struct {
		rule, doc string
	}{
		{"V07", head + "state: { seen: {} }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V17", head + "state: { seen: { default: [] } }\nsteps:\n" +
			"  - { id: a, kind: exec, exec: { argv: [x] }, outputs: { state: { seen: '{{ result.x }}' } } }\n" +
			"  - { id: b, kind: exec, exec: { argv: [x] }, outputs: { state: { seen: '{{ result.y }}' } } }\n"},
		{"V17", head + "steps:\n  - { id: a, kind: exec, exec: { argv: [x] }, outputs: { state: { nope: '{{ result.x }}' } } }\n"},
		{"V17", head + "steps:\n  - { id: a, kind: exec, exec: { argv: [x] }, outputs: { extra: 1 } }\n"},
		{"V17", head + "state: { n: { default: 0 } }\nsteps:\n  - id: loop\n    repeat: 2\n    steps:\n      - { id: a, kind: exec, exec: { argv: [x] }, outputs: { state: { n: '{{ result.x }}' } } }\n"},
		{"V25", head + "steps:\n  - { id: a, kind: exec, exec: { argv: [x] }, privacy: cloud-allowed }\n"},
		{"V26", head + "on: [ { schedule: { rrule: 'FREQ=SOMETIMES' } } ]\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V26", head + "on: [ { schedule: { rrule: 'FREQ=DAILY', tz: Mars/Olympus } } ]\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V26", head + "on: [ { schedule: { tz: UTC } } ]\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V16", head + "budget: { duration: soon }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V16", head + "steps: [ { id: a, kind: exec, exec: { argv: [x] }, budget: { usd: -1 } } ]\n"},
		{"V16", head + "retention: { recordings: 12h }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V16", head + "retention: { recordings: 4000d }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V16", head + "retention: { recordings: later }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V16", head + "retention: { logs: 30d }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
		{"V05", head + "vars: { n: { type: integer, default: abc } }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n"},
	}
	for i, c := range cases {
		r := Check(parse(t, c.doc))
		if !r.Has(c.rule) || !r.HasErrors() {
			t.Errorf("case %d: want %s, got %v", i, c.rule, r)
		}
		for _, is := range r {
			if is.Rule != c.rule {
				t.Errorf("case %d: stray %v", i, is)
			}
		}
	}
}

func TestCheckLegalVariants(t *testing.T) {
	ok := []string{
		// a cloud-allowed flow may narrow a step to local-only, and keep cloud.
		head + "privacy: cloud-allowed\nsteps:\n  - { id: a, kind: exec, exec: { argv: [x] }, privacy: local-only }\n  - { id: b, kind: exec, exec: { argv: [x] }, privacy: cloud-allowed }\n",
		// a default of null is a default.
		head + "state: { cursor: { default: null } }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] }, outputs: { state: { cursor: '{{ run.started_at }}' } } } ]\n",
		head + "on: [ { schedule: { rrule: 'FREQ=MINUTELY;INTERVAL=15' } }, { manual: {} } ]\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n",
		head + "retention: { recordings: 30d }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n",
		head + "vars: { n: { type: integer, default: 3 }, d: { type: duration, default: 14d } }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n",
	}
	for i, doc := range ok {
		if r := Check(parse(t, doc)); len(r) > 0 {
			t.Errorf("case %d: %v", i, r)
		}
	}
}

func TestReportAPI(t *testing.T) {
	r := Report{{Rule: "V22", Code: CodeInvalidFlow, Severity: SeverityWarning, Msg: "w"}}
	if r.HasErrors() || r.Err() != nil || len(r.Warnings()) != 1 {
		t.Errorf("a warning alone must not fail: %v", r)
	}
	r = append(r, Issue{Rule: "V07", Code: CodeInvalidFlow, Severity: SeverityError, Step: "s", Field: "f", Msg: "m"})
	err := r.Err()
	var is Issue
	if err == nil || !errors.As(err, &is) || is.Rule != "V07" {
		t.Errorf("Err = %v", err)
	}
}

func TestCoerceVars(t *testing.T) {
	f := parse(t, head+`vars:
  n: { type: integer }
  x: { type: number }
  b: { type: boolean }
  d: { type: duration }
  l: { type: list }
  m: { type: map }
  s: { type: string }
  u: {}
  dflt: { type: integer, default: 7 }
steps: [ { id: a, kind: exec, exec: { argv: [x] } } ]
`)
	got, err := ResolveVars(f, map[string]string{
		"n": "42", "x": "1.5", "b": "true", "d": "14d", "l": `["a","b"]`, "m": `{"k":1}`, "s": "007", "u": "raw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["n"] != int64(42) || got["x"] != 1.5 || got["b"] != true || got["d"] != "14d" {
		t.Errorf("scalars: %#v", got)
	}
	if l, ok := got["l"].([]any); !ok || len(l) != 2 {
		t.Errorf("list: %#v", got["l"])
	}
	if m, ok := got["m"].(map[string]any); !ok || m["k"] != float64(1) {
		t.Errorf("map: %#v", got["m"])
	}
	if got["s"] != "007" || got["u"] != "raw" {
		t.Errorf("a string and an untyped var stay as written: %#v", got)
	}
	if got["dflt"] != 7 {
		t.Errorf("a default keeps its declared type: %#v", got["dflt"])
	}
}

func TestCoerceVarFailures(t *testing.T) {
	f := parse(t, head+"vars: { n: { type: integer }, b: { type: boolean }, l: { type: list }, d: { type: duration }, m: { type: map }, x: { type: number } }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n")
	for k, v := range map[string]string{
		"n": "4.5", "b": "yes please", "l": `{"a":1}`, "d": "soon", "m": `[1]`, "x": "NaN",
	} {
		_, err := ResolveVars(f, map[string]string{k: v})
		var ve *VarError
		if !errors.As(err, &ve) || ve.Var != k || !strings.Contains(err.Error(), CodeInvalidVar) {
			t.Errorf("%s=%q: %v; want a typed invalid_var", k, v, err)
		}
	}
	if _, err := ResolveVars(f, map[string]string{"ghost": "1"}); err == nil {
		t.Error("an unknown var must fail")
	}
}
