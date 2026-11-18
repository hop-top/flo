package node

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExamplesValidate(t *testing.T) {
	files, _ := filepath.Glob("testdata/examples/*.yaml")
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // fixed glob
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Load(bytes.NewReader(data), f); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

// Each fixture in testdata/invalid is named for the rule it breaks and
// must fail that rule, and only decode cleanly (so the rule, not Parse,
// is what refuses it).
func TestInvalidFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/invalid/N*.yaml")
	if len(files) != 13 {
		t.Fatalf("want one fixture per rule N03..N15, got %d", len(files))
	}
	for _, f := range files {
		rule := strings.TrimSuffix(filepath.Base(f), ".yaml")
		t.Run(rule, func(t *testing.T) {
			data, err := os.ReadFile(f) //nolint:gosec // fixed glob
			if err != nil {
				t.Fatal(err)
			}
			s, err := Parse(bytes.NewReader(data), f)
			if err != nil {
				t.Fatalf("fixture must decode: %v", err)
			}
			errs := Check(s)
			var hit bool
			for _, e := range errs {
				if e.Rule == rule {
					hit = true
				}
			}
			if !hit {
				t.Errorf("no %s error; got %v", rule, errs)
			}
			for _, e := range errs {
				if e.Rule != rule {
					t.Errorf("unexpected %s: %v", e.Rule, e)
				}
			}
		})
	}
}

const nodeHead = "node: acme/x@1\nkind: command\nargv: [x]\ninputs: { q: { type: string }, n: { type: integer, minimum: 1, maximum: 5 } }\noutputs: { schema: { type: object, properties: { a: { type: integer } }, required: [a] } }\ncapabilities: []\nside_effect: read\nprivacy: local\n"

func checkDoc(t *testing.T, doc string) Errors {
	t.Helper()
	s, err := Parse(strings.NewReader(doc), "n.yaml")
	if err != nil {
		t.Fatalf("must decode: %v", err)
	}
	return Check(s)
}

func hasRule(errs Errors, rule, code string) bool {
	for _, e := range errs {
		if e.Rule == rule && e.Code == code {
			return true
		}
	}
	return false
}

func TestRuleCodes(t *testing.T) {
	cases := []struct {
		name, doc, rule, code string
	}{
		{"N04 out of scope is typed", nodeHead + "cwd: '{{ results.a }}'\n", RuleN04, CodeOutOfScope},
		{"N14 out of scope is typed", strings.Replace(nodeHead, "outputs: {", "outputs: { map: '{{ vars.x }}',", 1), RuleN14, CodeOutOfScope},
		{"N07 mismatch is typed", strings.Replace(nodeHead, "capabilities: []", "capabilities: [mail.send]", 1), RuleN07, CodeCapabilityMismatch},
		{"N07 delete needs irreversible", strings.Replace(strings.Replace(nodeHead, "capabilities: []", "capabilities: [docs.delete]", 1), "side_effect: read", "side_effect: write-shared", 1), RuleN07, CodeCapabilityMismatch},
		{"N07 malformed capability", strings.Replace(nodeHead, "capabilities: []", "capabilities: [Mail]", 1), RuleN07, CodeInvalidNode},
		{"N03 http keys on a command", nodeHead + "response: { success: [200] }\n", RuleN03, CodeInvalidNode},
		{"N03 argv on a plugin", strings.Replace(nodeHead, "kind: command", "kind: plugin", 1), RuleN03, CodeInvalidNode},
		{"N10 exec with trust", nodeHead + "lowers_to: exec\napproval: trust\ntrust: { class: x }\n", RuleN10, CodeInvalidNode},
		{"N10 exec with custom exit codes", nodeHead + "lowers_to: exec\nexit_codes: { ok: [0], retry: [75] }\n", RuleN10, CodeInvalidNode},
		{"N12 secret in url", "node: acme/x@1\nkind: http\nrequest: { method: GET, url: 'https://h.test/x?api_key=abc' }\ninputs: {}\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: read\nprivacy: cloud\n", RuleN12, CodeInvalidNode},
		{"N12 credentials in url", "node: acme/x@1\nkind: http\nrequest: { method: GET, url: 'https://u:p@h.test/x' }\ninputs: {}\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: read\nprivacy: cloud\n", RuleN12, CodeInvalidNode},
		{"N12 auth secret is a name", "node: acme/x@1\nkind: http\nrequest: { method: GET, url: 'https://h.test/x', auth: { secret: '{{ in.q }}', scheme: bearer } }\ninputs: { q: { type: string } }\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: read\nprivacy: cloud\n", RuleN12, CodeInvalidNode},
		{"N12 secret in argv", strings.Replace(nodeHead, "argv: [x]", "argv: [x, '--token=abc']", 1), RuleN12, CodeInvalidNode},
		{"N13 out violates schema", nodeHead + "examples: [ { in: { q: a }, out: { a: nope } } ]\n", RuleN13, CodeInvalidNode},
		{"N13 out misses required", nodeHead + "examples: [ { in: { q: a }, out: {} } ]\n", RuleN13, CodeInvalidNode},
		{"N13 input out of range", nodeHead + "examples: [ { in: { n: 9 }, out: { a: 1 } } ]\n", RuleN13, CodeInvalidNode},
		{"N13 unknown input", nodeHead + "examples: [ { in: { zz: 1 } } ]\n", RuleN13, CodeInvalidNode},
		{"N15 per not a duration", nodeHead + "rate_limit: { bucket: b, max: 3, per: sometimes }\n", RuleN15, CodeInvalidNode},
		{"N11 record false on write", strings.Replace(nodeHead, "side_effect: read", "side_effect: write-local\nrecord: false", 1), RuleN11, CodeInvalidNode},
		{"general: required with default", strings.Replace(nodeHead, "q: { type: string }", "q: { type: string, required: true, default: a }", 1), "", CodeInvalidNode},
		{"general: builtin outside flo", strings.Replace(nodeHead, "kind: command\nargv: [x]\n", "kind: builtin\n", 1), "", CodeInvalidNode},
		{"general: flo namespace for a command", strings.Replace(nodeHead, "acme/x@1", "flo/x@1", 1), "", CodeInvalidNode},
		{"general: reserved isolation", nodeHead + "placement: { isolation: container }\n", "", CodeReservedKey},
		{"general: local node on a remote runner", nodeHead + "placement: { runner: farm }\n", "", CodePrivacyViolation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errs := checkDoc(t, c.doc); !hasRule(errs, c.rule, c.code) {
				t.Errorf("want rule %q code %s; got %v", c.rule, c.code, errs)
			}
		})
	}
}

func TestLegalSpecs(t *testing.T) {
	cases := map[string]string{
		"valid base":                          nodeHead,
		"example in range with out":           nodeHead + "examples: [ { in: { q: a, n: 3 }, out: { a: 1 } } ]\n",
		"read verbs on a read node":           strings.Replace(nodeHead, "capabilities: []", "capabilities: [mail.read, tasks.list]", 1),
		"irreversible with required approval": strings.Replace(strings.Replace(nodeHead, "capabilities: []", "capabilities: [mail.send]", 1), "side_effect: read", "side_effect: irreversible\napproval: required", 1),
		"trust approval with a class":         nodeHead + "approval: trust\ntrust: { class: \"{{ in.q }}\" }\nlowers_to: engine\n",
		"record false on a pure node":         strings.Replace(nodeHead, "side_effect: read", "side_effect: none\nrecord: false", 1),
		"http auth by reference":              "node: acme/x@1\nkind: http\nrequest: { method: GET, url: 'https://h.test/x', headers: { Accept: application/json }, auth: { secret: acme-token, scheme: 'header:X-Key' } }\ninputs: {}\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: read\nprivacy: cloud\n",
		"x-keys at the root":                  nodeHead + "x-owner: me\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if errs := checkDoc(t, doc); len(errs) > 0 {
				t.Errorf("%v", errs)
			}
		})
	}
}

// x-* is allowed at the document root only; below it, an unknown key is
// still an error (a typo must not hide behind a prefix).
func TestXKeysBelowRootStayErrors(t *testing.T) {
	doc := strings.Replace(nodeHead, "q: { type: string }", "q: { type: string, x-note: hi }", 1)
	if _, err := Parse(strings.NewReader(doc), "n.yaml"); err == nil {
		t.Error("nested x-* accepted")
	}
}
