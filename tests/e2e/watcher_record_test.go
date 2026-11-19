//go:build !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/flowtest"
)

const depWatch = "dependency-watch"

// TestDependencyWatchCommittedRecording: the recording in
// flows/dependency-watch/record replays, offline and with no gh, aps, jq or
// model on the machine, through the judge backend: the model's answers are
// in the cassettes. With FLO_E2E_RECORD set the recording is rewritten first:
// the reads run against the fake gh and the judge asks the local ollama
// (OLLAMA_HOST, default 127.0.0.1:11434) for real.
func TestDependencyWatchCommittedRecording(t *testing.T) {
	src := filepath.Join(repoRoot, "flows", depWatch, "record")
	if os.Getenv("FLO_E2E_RECORD") != "" {
		e := watcherEnvWith(t, "judge")
		bundle := filepath.Join(t.TempDir(), depWatch)
		if out, err := cpR(filepath.Join(repoRoot, "flows", depWatch), bundle); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if err := os.RemoveAll(filepath.Join(bundle, "record")); err != nil {
			t.Fatal(err)
		}
		e.ok("grant", "set", "automation", "vcs.read", "--yes")
		e.ok("flow", "apply", "-f", bundle, "--yes")
		e.recordTest(depWatch)
		if err := os.RemoveAll(src); err != nil {
			t.Fatal(err)
		}
		if out, err := cpR(filepath.Join(bundle, "record"), src); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}

	e := newEnv(t, "FLO_AI_CLASSIFIER=judge", "FLO_AI_MODELS_ALLOW=ollama://qwen3*") // no shims, no model: a call that is not answered from the recording fails
	e.installRepoPack("github")
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	bundle := filepath.Join(t.TempDir(), depWatch)
	if out, err := cpR(filepath.Join(repoRoot, "flows", depWatch), bundle); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.ok("flow", "apply", "-f", bundle, "--yes")
	res, r := e.flowTest(depWatch)
	if r.Exit != 0 || !res.Passed || res.Mode != flowtest.ModeReplay || res.Expected == 0 || res.Held != res.Expected || res.Replayed == 0 || res.Replayed != res.Calls {
		t.Fatalf("replay of the committed recording: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	// The model calls on the tape: the daemon's model list (the judge asks
	// for it even for one model), then the chat.
	files, _ := filepath.Glob(filepath.Join(src, "cassettes", "http-*.req.yaml"))
	var tags, chats int
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // a file of this repository
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(string(b), "/api/tags"):
			tags++
		case strings.Contains(string(b), "/api/chat"):
			chats++
		default:
			t.Errorf("%s is not a model call", f)
		}
	}
	if tags == 0 || chats == 0 {
		t.Errorf("the recording holds %d model-list and %d chat calls, want both", tags, chats)
	}
}
