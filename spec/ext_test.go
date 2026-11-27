package spec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const extDocYAML = `
flow: ext
version: 1.0.0
vars:
  who: {default: ada}
x-owner: platform
x-meta:
  labels: [a, "{{who}}"]
  n: 3
steps:
  - id: one
    title: One
    x-acme:
      tags: [t1, "for {{who}}"]
      priority: 2
  - id: loop
    repeat: 2
    steps:
      - id: inner
        x-acme: {note: "inner {{who}}"}
`

const extDocJSON = `{
  "flow": "ext", "version": "1.0.0",
  "vars": {"who": {"default": "ada"}},
  "x-owner": "platform",
  "x-meta": {"labels": ["a", "{{who}}"], "n": 3},
  "steps": [
    {"id": "one", "title": "One", "x-acme": {"tags": ["t1", "for {{who}}"], "priority": 2}},
    {"id": "loop", "repeat": 2, "steps": [{"id": "inner", "x-acme": {"note": "inner {{who}}"}}]}
  ]
}`

func parseExt(t *testing.T, doc, name string) *Flow {
	t.Helper()
	r, err := ParseFlow(strings.NewReader(doc), name)
	if err != nil {
		t.Fatalf("ParseFlow %s: %v", name, err)
	}
	return r
}

func assertExtDoc(t *testing.T, r *Flow) {
	t.Helper()
	if r.Ext["x-owner"] != "platform" {
		t.Errorf("flow Ext x-owner = %v", r.Ext["x-owner"])
	}
	meta, _ := r.Ext["x-meta"].(map[string]any)
	if meta["n"] != 3 || !reflect.DeepEqual(meta["labels"], []any{"a", "{{who}}"}) {
		t.Errorf("flow Ext x-meta = %#v", r.Ext["x-meta"])
	}
	acme, _ := r.Steps[0].Ext["x-acme"].(map[string]any)
	if acme["priority"] != 2 || !reflect.DeepEqual(acme["tags"], []any{"t1", "for {{who}}"}) {
		t.Errorf("step Ext x-acme = %#v", r.Steps[0].Ext["x-acme"])
	}
	inner, _ := r.Steps[1].Steps[0].Ext["x-acme"].(map[string]any)
	if inner["note"] != "inner {{who}}" {
		t.Errorf("nested step Ext = %#v", r.Steps[1].Steps[0].Ext)
	}
	if r.Steps[1].Ext != nil {
		t.Errorf("block step Ext = %#v, want none", r.Steps[1].Ext)
	}
}

func TestParseFlow_ExtKeysAcceptedYAMLAndJSON(t *testing.T) {
	assertExtDoc(t, parseExt(t, extDocYAML, "ext.yaml"))
	assertExtDoc(t, parseExt(t, extDocJSON, "ext.json"))
}

// Only the x- prefix opens the door: any other unknown key stays an
// error, at the flow root and on a step, in both encodings, and also when
// an x- key sits next to it.
func TestParseFlow_NonExtUnknownKeysStillRejected(t *testing.T) {
	cases := map[string]string{
		"yaml root":           "flow: a\nversion: 1\nbogus: 1\nsteps:\n  - id: a\n",
		"yaml root with x":    "flow: a\nversion: 1\nx-ok: 1\nbogus: 1\nsteps:\n  - id: a\n",
		"yaml step":           "flow: a\nversion: 1\nsteps:\n  - id: a\n    bogus: 1\n",
		"yaml step with x":    "flow: a\nversion: 1\nsteps:\n  - id: a\n    x-ok: 1\n    bogus: 1\n",
		"yaml nested step":    "flow: a\nversion: 1\nsteps:\n  - id: a\n    repeat: 1\n    steps:\n      - id: b\n        x-ok: 1\n        bogus: 1\n",
		"yaml x in exec":      "flow: a\nversion: 1\nsteps:\n  - id: a\n    kind: exec\n    exec: {argv: [a], x-in: 1}\n",
		"yaml x in vars":      "flow: a\nversion: 1\nvars: {v: {x-in: 1}}\nsteps:\n  - id: a\n",
		"yaml track":          "flow: a\nversion: 1\ntrack: {title: t}\nsteps:\n  - id: a\n",
		"yaml track with x":   "flow: a\nversion: 1\nx-ok: 1\ntrack: {title: t}\nsteps:\n  - id: a\n",
		"json root":           `{"flow":"a","version":"1","bogus":1,"steps":[{"id":"a"}]}`,
		"json root with x":    `{"flow":"a","version":"1","x-ok":1,"bogus":1,"steps":[{"id":"a"}]}`,
		"json step":           `{"flow":"a","version":"1","steps":[{"id":"a","bogus":1}]}`,
		"json step with x":    `{"flow":"a","version":"1","steps":[{"id":"a","x-ok":1,"bogus":1}]}`,
		"json nested step":    `{"flow":"a","version":"1","steps":[{"id":"a","repeat":1,"steps":[{"id":"b","x-ok":1,"bogus":1}]}]}`,
		"json x in exec":      `{"flow":"a","version":"1","steps":[{"id":"a","kind":"exec","exec":{"argv":["a"],"x-in":1}}]}`,
		"json track":          `{"flow":"a","version":"1","track":{"title":"t"},"steps":[{"id":"a"}]}`,
		"json track with x":   `{"flow":"a","version":"1","x-ok":1,"track":{"title":"t"},"steps":[{"id":"a"}]}`,
		"yaml X uppercase":    "flow: a\nversion: 1\nX-owner: 1\nsteps:\n  - id: a\n",
		"yaml x without dash": "flow: a\nversion: 1\nxowner: 1\nsteps:\n  - id: a\n",
	}
	for name, doc := range cases {
		file := "d.yaml"
		if strings.HasPrefix(name, "json") {
			file = "d.json"
		}
		if _, err := ParseFlow(strings.NewReader(doc), file); err == nil {
			t.Errorf("%s: accepted, want unknown key error", name)
		}
	}
}

func TestParseFlow_TrackKeyIsUnknown(t *testing.T) {
	_, err := parseFlowString(t, "flow: a\nversion: 1\ntrack: {title: t}\nsteps:\n  - id: a\n")
	if err == nil || !strings.Contains(err.Error(), "track") {
		t.Fatalf("track: accepted or wrong error: %v", err)
	}
}

func TestParseFlow_ExtOnlyWhenPresent(t *testing.T) {
	r := parseExt(t, "flow: a\nversion: 1\nsteps:\n  - id: a\n", "a.yaml")
	if r.Ext != nil || r.Steps[0].Ext != nil {
		t.Errorf("Ext = %#v / %#v, want nil", r.Ext, r.Steps[0].Ext)
	}
}

func TestParseFlow_ExtDoesNotHideOtherErrors(t *testing.T) {
	_, err := parseFlowString(t, "flow: a\nversion: 1\nx-ok: 1\nsteps:\n  - id: a\n    depends_on: [nope]\n")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("validation skipped with x- keys present: %v", err)
	}
}

func TestParseFlow_HashCoversExtKeys(t *testing.T) {
	a := parseExt(t, "flow: a\nversion: 1\nsteps:\n  - id: a\n", "a.yaml")
	b := parseExt(t, "flow: a\nversion: 1\nx-k: 1\nsteps:\n  - id: a\n", "a.yaml")
	if a.Hash == b.Hash {
		t.Error("hash ignores x- keys; it is the hash of the raw bytes")
	}
}

func TestValidateFlow_ExtKeyPrefix(t *testing.T) {
	r := parseExt(t, "flow: a\nversion: 1\nsteps:\n  - id: a\n", "a.yaml")
	r.Ext = map[string]any{"owner": 1}
	if err := ValidateFlow(r); err == nil || !strings.Contains(err.Error(), "x-") {
		t.Errorf("flow Ext key without prefix: %v", err)
	}
	r.Ext = nil
	r.Steps[0].Ext = map[string]any{"owner": 1}
	if err := ValidateFlow(r); err == nil || !strings.Contains(err.Error(), "x-") {
		t.Errorf("step Ext key without prefix: %v", err)
	}
}

// parse -> serialize -> parse keeps every extension, in both encodings
// and across them.
func TestFlow_ExtRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, doc, file string }{
		{"yaml", extDocYAML, "ext.yaml"},
		{"json", extDocJSON, "ext.json"},
	} {
		first := parseExt(t, tc.doc, tc.file)

		y, err := yaml.Marshal(first)
		if err != nil {
			t.Fatalf("%s: yaml.Marshal: %v", tc.name, err)
		}
		second := parseExt(t, string(y), "again.yaml")
		assertExtDoc(t, second)
		assertSameExt(t, tc.name+" via yaml", first, second)

		j, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("%s: json.Marshal: %v", tc.name, err)
		}
		third := parseExt(t, string(j), "again.json")
		assertExtDoc(t, third)
		assertSameExt(t, tc.name+" via json", first, third)
	}
}

func assertSameExt(t *testing.T, label string, a, b *Flow) {
	t.Helper()
	if !reflect.DeepEqual(a.Ext, b.Ext) {
		t.Errorf("%s: flow Ext %#v != %#v", label, a.Ext, b.Ext)
	}
	var walk func(x, y []Step)
	walk = func(x, y []Step) {
		if len(x) != len(y) {
			t.Fatalf("%s: step count %d != %d", label, len(x), len(y))
		}
		for i := range x {
			if !reflect.DeepEqual(x[i].Ext, y[i].Ext) {
				t.Errorf("%s: step %s Ext %#v != %#v", label, x[i].ID, x[i].Ext, y[i].Ext)
			}
			walk(x[i].Steps, y[i].Steps)
		}
	}
	walk(a.Steps, b.Steps)
}

func TestFlow_MarshalWithoutExtIsUnchanged(t *testing.T) {
	r := parseExt(t, "flow: a\nversion: 1\nsteps:\n  - id: a\n    title: A\n", "a.yaml")
	y, err := yaml.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(y), "x-") {
		t.Errorf("yaml has ext keys: %s", y)
	}
	j, err := json.Marshal(r)
	if err != nil || bytes.Contains(j, []byte("x-")) {
		t.Errorf("json has ext keys: %s (%v)", j, err)
	}
}

func extLocator(t *testing.T) (*Flow, memLocator) {
	t.Helper()
	child := parseExt(t, `
flow: child
version: 1.0.0
vars: {target: {required: true}}
x-child: kept
steps:
  - id: scan
    kind: exec
    exec: {argv: [scan]}
    x-acme: {target: "{{target}}", n: 1}
`, "child.yaml")
	parent := parseExt(t, `
flow: parent
version: 1.0.0
vars: {pr: {required: true}}
steps:
  - id: before
    x-acme: {keep: true}
  - id: sec
    include: child@1.0.0
    with: {target: "{{pr}}"}
  - id: loop
    repeat: 2
    steps:
      - id: fix
        x-acme: {iter: "{{run.iteration}}"}
`, "parent.yaml")
	return parent, memLocator{"child@1.0.0": child}
}

// Extensions survive include prefixing, include var binding, repeat
// unrolling, selection and rendering; each step keeps its own.
func TestExpand_StepExtSurvivesIncludeRepeatSelectRender(t *testing.T) {
	parent, loc := extLocator(t)
	steps, err := Expand(parent, loc)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	byID := map[string]Step{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	if got := byID["before"].Ext["x-acme"].(map[string]any)["keep"]; got != true {
		t.Errorf("plain step Ext lost: %#v", byID["before"].Ext)
	}
	sec := byID["sec/scan"].Ext["x-acme"].(map[string]any)
	if sec["n"] != 1 || sec["target"] != "{{pr}}" {
		t.Errorf("included step Ext = %#v (var binding must splice the parent expression)", sec)
	}
	for i, id := range []string{"loop/1/fix", "loop/2/fix"} {
		got := byID[id].Ext["x-acme"].(map[string]any)["iter"]
		if want := string(rune('1' + i)); got != want {
			t.Errorf("%s Ext iter = %v, want %s", id, got, want)
		}
	}

	sel, _, err := Select(steps, Selector{IDs: map[string]bool{"sec/scan": true}}, false)
	if err != nil || len(sel) != 1 || sel[0].Ext["x-acme"] == nil {
		t.Fatalf("selection dropped Ext: %v %#v", err, sel)
	}

	rendered, err := RenderSteps(steps, Scope{Vars: map[string]any{"pr": "42"}})
	if err != nil {
		t.Fatalf("RenderSteps: %v", err)
	}
	for _, s := range rendered {
		if s.ID == "sec/scan" {
			if got := s.Ext["x-acme"].(map[string]any)["target"]; got != "42" {
				t.Errorf("rendered Ext target = %v, want 42", got)
			}
		}
	}
	if got := byID["sec/scan"].Ext["x-acme"].(map[string]any)["target"]; got != "{{pr}}" {
		t.Errorf("rendering mutated the expanded input: %v", got)
	}
}

func TestRenderStep_ExtStringLeaves(t *testing.T) {
	st := Step{ID: "a", Ext: map[string]any{"x-t": map[string]any{
		"tags": []any{"{{who}}", 7, "plain"}, "s": "hi {{who}}",
	}}}
	out, err := RenderStep(st, Scope{Vars: map[string]any{"who": "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	m := out.Ext["x-t"].(map[string]any)
	if m["s"] != "hi ada" || !reflect.DeepEqual(m["tags"], []any{"ada", 7, "plain"}) {
		t.Errorf("rendered = %#v", m)
	}
	if st.Ext["x-t"].(map[string]any)["s"] != "hi {{who}}" {
		t.Error("input mutated")
	}
	if _, err := RenderStep(st, Scope{}); err == nil || !strings.Contains(err.Error(), "who") {
		t.Errorf("unknown var in Ext leaf: %v, want the error other step fields give", err)
	}
}

func TestValidateFlow_ExtTemplateRefsChecked(t *testing.T) {
	_, err := parseFlowString(t, "flow: a\nversion: 1\nsteps:\n  - id: a\n    x-t: {k: \"{{missing}}\"}\n")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("unknown var in step Ext accepted: %v", err)
	}
	if _, err := parseFlowString(t, "flow: a\nversion: 1\nx-t: {k: \"{{missing}}\"}\nsteps:\n  - id: a\n"); err != nil {
		t.Errorf("flow Ext is not templated or validated: %v", err)
	}
}

func TestStepFields_ListsExtLeaves(t *testing.T) {
	s := Step{ID: "a", Ext: map[string]any{"x-t": map[string]any{"l": []any{"p", 1}}, "x-u": "q"}}
	paths := map[string]string{}
	for _, f := range StepFields(&s) {
		paths[f.Path] = f.Value
	}
	if paths["x-t.l[0]"] != "p" || paths["x-u"] != "q" {
		t.Errorf("paths = %v", paths)
	}
}

func TestRenderMap(t *testing.T) {
	in := map[string]any{"x-a": map[string]any{"k": "{{who}}!", "l": []any{"{{who}}", 2}}, "x-b": "z"}
	out, err := RenderMap(in, Scope{Vars: map[string]any{"who": "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"x-a": map[string]any{"k": "ada!", "l": []any{"ada", 2}}, "x-b": "z"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("RenderMap = %#v, want %#v", out, want)
	}
	if in["x-a"].(map[string]any)["k"] != "{{who}}!" {
		t.Error("input mutated")
	}
	if _, err := RenderMap(in, Scope{}); err == nil || !strings.Contains(err.Error(), "who") {
		t.Errorf("unknown var: %v", err)
	}
	if got, err := RenderMap(nil, Scope{}); err != nil || got != nil {
		t.Errorf("nil map = %v, %v", got, err)
	}
}

// Flow-level extensions are exposed unrendered.
func TestParseFlow_FlowExtLeftUnrendered(t *testing.T) {
	r := parseExt(t, extDocYAML, "ext.yaml")
	if !reflect.DeepEqual(r.Ext["x-meta"].(map[string]any)["labels"], []any{"a", "{{who}}"}) {
		t.Errorf("flow Ext rendered: %#v", r.Ext)
	}
	out, err := RenderMap(r.Ext, Scope{Vars: map[string]any{"who": "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["x-meta"].(map[string]any)["labels"], []any{"a", "ada"}) {
		t.Errorf("rendered = %#v", out)
	}
}
