package flowcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/spec"
)

type grants map[string][]string

func (g grants) Capabilities(p string) ([]string, bool) { c, ok := g[p]; return c, ok }

// flowsDir writes the named documents and returns a locator over them.
func flowsDir(t *testing.T, docs map[string]string) *spec.DirLocator {
	t.Helper()
	dir := t.TempDir()
	for name, doc := range docs {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &spec.DirLocator{Dirs: []string{dir}}
}

func flowDoc(name, extra string) string {
	return "flow: " + name + "\nversion: 1.0.0\n" + extra
}

func subOpts(t *testing.T, docs map[string]string) Options {
	o := opts(t)
	o.Flows = flowsDir(t, docs)
	return o
}

func codes(r Report) string {
	var parts []string
	for _, i := range r {
		parts = append(parts, i.Rule+"/"+i.Code)
	}
	return strings.Join(parts, " ")
}

func TestSubFlowResolutionAndVars(t *testing.T) {
	child := flowDoc("child", "vars: { who: { required: true }, n: { type: integer, default: 1 } }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n")
	o := subOpts(t, map[string]string{"child": child})
	cases := []struct{ name, uses, with, rule, code string }{
		{"missing var", "child@1.0.0", "{}", "V11", CodeInvalidFlow},
		{"unknown var", "child@1.0.0", "{ who: a, bogus: 1 }", "V11", CodeInvalidFlow},
		{"wrong literal type", "child@1.0.0", "{ who: a, n: abc }", "V11", CodeInvalidFlow},
		{"not found", "ghost@1.0.0", "{}", "V12", CodeFlowNotFound},
		{"version not pinned", "child", "{ who: a }", "V12", CodeInvalidFlow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Check(parse(t, head+"steps:\n  - { id: s, uses: "+c.uses+", with: "+c.with+" }\n"), o)
			if !issueWith(r, c.rule, c.code) {
				t.Errorf("want %s/%s, got %s", c.rule, c.code, codes(r))
			}
		})
	}
	if r := Check(parse(t, head+"steps:\n  - { id: s, uses: child@1.0.0, with: { who: a } }\n"), o); len(r) > 0 {
		t.Errorf("a valid sub-flow step: %v", r)
	}
}

func TestSubFlowLoopAndDepth(t *testing.T) {
	loop := map[string]string{
		"a": flowDoc("a", "steps: [ { id: s, uses: 'b@1.0.0' } ]\n"),
		"b": flowDoc("b", "steps: [ { id: s, uses: 'a@1.0.0' } ]\n"),
	}
	r := Check(parse(t, flowDoc("a", "steps: [ { id: s, uses: 'b@1.0.0' } ]\n")), subOpts(t, loop))
	if !issueWith(r, "V12", CodeCycle) {
		t.Errorf("a -> b -> a: %s", codes(r))
	}
	deep := map[string]string{}
	for i := 1; i <= 10; i++ {
		name := "f" + string(rune('a'+i-1))
		next := "f" + string(rune('a'+i))
		body := "steps: [ { id: s, kind: exec, exec: { argv: [x] } } ]\n"
		if i < 10 {
			body = "steps: [ { id: s, uses: '" + next + "@1.0.0' } ]\n"
		}
		deep[name] = flowDoc(name, body)
	}
	r = Check(parse(t, head+"steps: [ { id: s, uses: 'fa@1.0.0' } ]\n"), subOpts(t, deep))
	if !issueWith(r, "V12", CodeInvalidFlow) {
		t.Errorf("nesting past %d: %s", MaxSubFlowDepth, codes(r))
	}
	short := map[string]string{"fa": deep["fa"], "fb": flowDoc("fb", "steps: [ { id: s, kind: exec, exec: { argv: [x] } } ]\n")}
	if r = Check(parse(t, head+"steps: [ { id: s, uses: 'fa@1.0.0' } ]\n"), subOpts(t, short)); len(r) > 0 {
		t.Errorf("a shallow chain: %v", r)
	}
}

func TestSubFlowWithinParent(t *testing.T) {
	cloud := flowDoc("cloudy", "privacy: cloud-allowed\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n")
	reads := flowDoc("reader", "privacy: local-only\nsteps: [ { id: a, uses: tlc/task.list@1, with: { search: x } } ]\n")
	docs := map[string]string{"cloudy": cloud, "reader": reads}

	r := Check(parse(t, head+"steps: [ { id: s, uses: 'cloudy@1.0.0' } ]\n"), subOpts(t, docs))
	if !issueWith(r, "V24", CodePrivacyViolation) {
		t.Errorf("a cloud child under a local-only parent: %s", codes(r))
	}
	if r = Check(parse(t, head+"privacy: cloud-allowed\nsteps: [ { id: s, uses: 'cloudy@1.0.0' } ]\n"), subOpts(t, docs)); len(r) > 0 {
		t.Errorf("cloud under cloud is within: %v", r)
	}

	o := subOpts(t, docs)
	o.Grants = grants{"ops": {"mail.read"}, "full": {"tasks.read", "mail.read"}}
	r = Check(parse(t, head+"run_as: ops\nsteps: [ { id: s, uses: 'reader@1.0.0' } ]\n"), o)
	if !issueWith(r, "V24", CodeGrantDenied) {
		t.Errorf("child needs tasks.read, parent's profile lacks it: %s", codes(r))
	}
	if r = Check(parse(t, head+"run_as: full\nsteps: [ { id: s, uses: 'reader@1.0.0' } ]\n"), o); len(r) > 0 {
		t.Errorf("held capability: %v", r)
	}
}

func TestStepRunAsOnlyNarrows(t *testing.T) {
	o := opts(t)
	o.Grants = grants{"flow": {"tasks.read"}, "wide": {"tasks.read", "tasks.write", "mail.read"}, "narrow": {"mail.read"}}
	doc := func(profile string) string {
		return head + "run_as: flow\nsteps:\n  - { id: s, uses: tlc/task.list@1, with: { search: x }, run_as: " + profile + " }\n"
	}
	// the step needs tasks.read; "wide" holds it, and so does the flow.
	if r := Check(parse(t, doc("wide")), o); len(r) > 0 {
		t.Errorf("wide: %v", r)
	}
	o.Grants = grants{"flow": {"mail.read"}, "wide": {"tasks.read", "mail.read"}}
	r := Check(parse(t, doc("wide")), o)
	if !issueWith(r, "V25", CodeGrantDenied) {
		t.Errorf("a step profile holding a needed capability the flow's lacks: %s", codes(r))
	}
}

func TestIncludeExpandedForChecks(t *testing.T) {
	inc := flowDoc("inc", "steps: [ { id: a, uses: inbox/apply@1, with: { run: x } } ]\n")
	o := subOpts(t, map[string]string{"inc": inc})
	r := Check(parse(t, head+"steps: [ { id: i, include: inc@1.0.0 } ]\n"), o)
	if !issueWith(r, "V20", CodeUnguardedIrreversible) {
		t.Errorf("a rule reaches steps spliced in by include: %s", codes(r))
	}
}
