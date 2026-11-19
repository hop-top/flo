//go:build !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/flowtest"
)

// recordedLabels are the judge's labels on the committed tape, in item order:
// what the model answered when the recording was made.
var recordedLabels = []struct{ pkg, version, label string }{
	{"github.com/cli/cli/v2", "2.105.0", "routine"},
	{"github.com/cli/cli/v2", "2.104.0", "routine"},
	{"github.com/example-org/oldlib", "3.0.0", "breaking"},
	{"github.com/example-org/oldlib", "2.9.0", "routine"},
	{"github.com/hop-top/poly-kit/kit", "0.5.0-beta.14", "routine"},
	{"github.com/hop-top/poly-kit/kit", "0.5.0-beta.13", "routine"},
	{"golang.org/x/net", "0.63.0", "routine"},
	{"golang.org/x/net", "0.62.0", "routine"},
	{"golang.org/x/net", "0.61.0", "routine"},
}

// replayDepWatch replays the recording of a copy of the dependency-watch
// bundle; mutate may edit the copy first.
func replayDepWatch(t *testing.T, mutate func(bundle string)) (flowtest.Result, result) {
	t.Helper()
	e := newEnv(t, "FLO_AI_CLASSIFIER=judge", "FLO_AI_MODELS_ALLOW=ollama://qwen3*")
	e.installRepoPack("github")
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	bundle := filepath.Join(t.TempDir(), depWatch)
	if out, err := cpR(filepath.Join(repoRoot, "flows", depWatch), bundle); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if mutate != nil {
		mutate(bundle)
	}
	e.ok("flow", "apply", "-f", bundle, "--yes")
	return e.flowTest(depWatch)
}

// TestDependencyWatchRecordingLabels: the replay of the committed recording
// ends with the labels the model gave on the tape, engine and model included.
func TestDependencyWatchRecordingLabels(t *testing.T) {
	res, r := replayDepWatch(t, nil)
	if r.Exit != 0 || !res.Passed {
		t.Fatalf("replay: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	var out map[string]any
	for _, s := range res.Steps {
		if s.ID == "judge" && s.Index == nil {
			out, _ = s.Output.(map[string]any)
		}
	}
	if out == nil {
		t.Fatal("no judge step in the replay")
	}
	if out["engine"] != "local" || out["model"] != "ollama://qwen3:4b-instruct-2507-q4_K_M" {
		t.Errorf("judge engine/model = %v/%v, want local/ollama://qwen3:4b-instruct-2507-q4_K_M", out["engine"], out["model"])
	}
	items, _ := out["items"].([]any)
	if len(items) != len(recordedLabels) {
		t.Fatalf("judge labelled %d releases, the tape has %d", len(items), len(recordedLabels))
	}
	for i, want := range recordedLabels {
		row, _ := items[i].(map[string]any)
		item, _ := row["item"].(map[string]any)
		if item["package"] != want.pkg || item["version"] != want.version || row["label"] != want.label {
			t.Errorf("item %d: got %v@%v %v, want %s@%s %s", i, item["package"], item["version"], row["label"], want.pkg, want.version, want.label)
		}
	}
}

// TestDependencyWatchRecordingLabelDriftFails: a chat answer on the tape
// changed after the fact (a copy; the committed tape is untouched) no longer
// matches the labels the flow's tests pin, and the test fails naming the item.
func TestDependencyWatchRecordingLabelDriftFails(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"breaking becomes routine", `value\": \"breaking`, `value\": \"routine`},
		{"routine becomes security", `value\": \"routine`, `value\": \"security`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed string
			res, r := replayDepWatch(t, func(bundle string) {
				files, _ := filepath.Glob(filepath.Join(bundle, "record", "cassettes", "http-*.resp.yaml"))
				for _, f := range files {
					b, err := os.ReadFile(f) //nolint:gosec // a copy of this repository's recording, in a temp directory
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(b), tc.from) {
						continue
					}
					if err := os.WriteFile(f, []byte(strings.Replace(string(b), tc.from, tc.to, 1)), 0o600); err != nil { //nolint:gosec // a copy of this repository's recording, in a temp directory
						t.Fatal(err)
					}
					changed = f
					return
				}
			})
			if changed == "" {
				t.Fatalf("no recorded answer holds %q", tc.from)
			}
			if r.Exit == 0 || res.Passed || !hasProblem(res, flowtest.ProblemExpect) || res.Held >= res.Expected {
				t.Fatalf("a drifted label passed: exit %d, %+v", r.Exit, res)
			}
			var msg string
			for _, p := range res.Problems {
				if p.Kind == flowtest.ProblemExpect {
					msg += p.Message + "\n"
				}
			}
			if !strings.Contains(msg, "tests/watch.yaml") || !strings.Contains(msg, "step judge output differs at items[") || !strings.Contains(msg, "].label") {
				t.Errorf("the failure does not say which label drifted:\n%s", msg)
			}
			t.Logf("drift message: %s", strings.TrimSpace(msg))
		})
	}
}
