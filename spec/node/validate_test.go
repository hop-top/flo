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
