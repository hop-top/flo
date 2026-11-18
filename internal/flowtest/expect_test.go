package flowtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/flowtest"
)

func writeTests(t *testing.T, name, body string) string {
	t.Helper()
	bundle := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundle, "tests"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "tests", name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestLoadExpectNoneWithoutTests(t *testing.T) {
	for _, b := range []string{"", t.TempDir()} {
		if ex, err := flowtest.LoadExpect(b); err != nil || len(ex) != 0 {
			t.Errorf("%q: %v %v", b, ex, err)
		}
	}
}

func TestLoadExpectRejectsWhatItCannotRead(t *testing.T) {
	for name, body := range map[string]string{
		"no step":       "expect:\n  - status: succeeded\n",
		"unknown field": "expect:\n  - step: a\n    nope: 1\n",
		"not yaml":      "expect: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := flowtest.LoadExpect(writeTests(t, "x.yaml", body)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCheck(t *testing.T) {
	steps := []flowtest.Step{
		{ID: "newadv", Status: "succeeded", Output: map[string]any{"items": []any{map[string]any{"ghsa_id": "GHSA-a", "sev": "high"}}, "seen": []any{"GHSA-a"}}},
		{ID: "findings", Status: "skipped"},
		{ID: "newadv", Status: "succeeded", Index: new(int), Output: "an element"},
	}
	bundle := writeTests(t, "t.yaml", `
expect:
  - step: newadv
    output: { items: [{ ghsa_id: GHSA-a }] }
  - step: findings
    status: skipped
  - step: newadv
    output: { seen: [GHSA-a] }
`)
	ex, err := flowtest.LoadExpect(bundle)
	if err != nil || len(ex) != 3 {
		t.Fatalf("expect = %+v %v", ex, err)
	}
	if held, problems := flowtest.Check(steps, ex); held != 3 || len(problems) != 0 {
		t.Fatalf("held %d problems %+v", held, problems)
	}

	bad := writeTests(t, "t.yaml", `
expect:
  - step: newadv
    output: { items: [{ ghsa_id: GHSA-b }] }
  - step: findings
  - step: missing
  - step: newadv
    output: { seen: [GHSA-a, GHSA-b] }
`)
	ex, err = flowtest.LoadExpect(bad)
	if err != nil {
		t.Fatal(err)
	}
	held, problems := flowtest.Check(steps, ex)
	if held != 0 || len(problems) != 4 {
		t.Fatalf("held %d problems %+v", held, problems)
	}
	for i, want := range []string{"items[0].ghsa_id", "ended skipped, expected succeeded", "not in the run", "seen"} {
		if problems[i].Kind != flowtest.ProblemExpect || !strings.Contains(problems[i].Message, want) {
			t.Errorf("problem %d = %+v, want %q", i, problems[i], want)
		}
	}
}
