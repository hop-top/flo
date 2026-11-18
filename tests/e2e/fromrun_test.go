//go:build !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

// TestFlowTestFromRun is the story on the binary: a flow is activated, a real
// run is recorded, and the recording becomes the bundle's test. The bundle's
// record/ is gone to begin with, so the version is untested again until
// --from-run restores a passing one.
func TestFlowTestFromRun(t *testing.T) {
	e := newEnv(t)
	bundle := copyBundle(t, "dependency-watch")
	gh := filepath.Join(e.scratch, "gh.log")
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	e.ok("flow", "apply", "-f", bundle, "--yes")
	e.recordTest("dependency-watch")
	e.ok("flow", "set", "dependency-watch", "--state", "active", "--yes")
	eng := e.startEngine()
	defer eng.stop()

	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "dependency-watch", "--record", "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("recorded run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	live := len(readLines(t, gh))
	if live != 2 {
		t.Fatalf("the real run read through gh once per node: %v", readLines(t, gh))
	}
	// A run that was not recorded is no source.
	var plain created
	e.jsonOf(&plain, "run", "create", "--flow", "dependency-watch", "--wait")
	if r := e.run("flow", "test", "dependency-watch", "--from-run", plain.Run, "--yes"); r.Exit != core.ExitNotFound || !strings.Contains(r.Stderr, "no recording") {
		t.Fatalf("an unrecorded run: exit %d\n%s", r.Exit, r.Stderr)
	}
	live = len(readLines(t, gh))

	// The bundle loses its recordings: nothing tests it any more.
	if err := os.RemoveAll(filepath.Join(bundle, "record")); err != nil {
		t.Fatal(err)
	}
	if r := e.run("flow", "set", "dependency-watch", "--state", "paused", "--yes"); r.Exit != 0 {
		t.Fatalf("pause: exit %d\n%s", r.Exit, r.Stderr)
	}
	if r := e.run("flow", "set", "dependency-watch", "--state", "active", "--yes"); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "changed since") {
		t.Fatalf("activation of a bundle with no recordings: exit %d\n%s", r.Exit, r.Stderr)
	}

	// It writes the bundle, so it asks; --dry-run says what it would do.
	if r := e.run("flow", "test", "dependency-watch", "--from-run", c.Run); r.Exit != core.ExitUsage || !strings.Contains(r.Stderr, "confirmation_required") {
		t.Fatalf("from-run without --yes: exit %d\n%s", r.Exit, r.Stderr)
	}
	r := e.run("flow", "test", "dependency-watch", "--from-run", c.Run, "--write-expect", "--dry-run", "--format", "json")
	if r.Exit != 0 || !strings.Contains(r.Stdout, "record/cassettes/") || !strings.Contains(r.Stdout, "tests/from-"+c.Run+".yaml") {
		t.Fatalf("from-run --dry-run: exit %d\n%s\n%s", r.Exit, r.Stdout, r.Stderr)
	}
	if exists(filepath.Join(bundle, "record")) || exists(filepath.Join(bundle, "tests")) {
		t.Fatal("--dry-run wrote to the bundle")
	}
	if r := e.run("flow", "test", "dependency-watch", "--from-run", c.Run, "--record", "--yes"); r.Exit == 0 {
		t.Error("--from-run with --record")
	}
	if r := e.run("flow", "test", "dependency-watch", "--write-expect", "--yes"); r.Exit != core.ExitUsage {
		t.Errorf("--write-expect without --from-run: exit %d", r.Exit)
	}

	var res serve.FromRunResult
	if r := e.jsonOf(&res, "flow", "test", "dependency-watch", "--from-run", c.Run, "--write-expect", "--yes"); r.Exit != 0 {
		t.Fatalf("from-run: exit %d\n%s\n%s", r.Exit, r.Stdout, r.Stderr)
	}
	if !res.Test.Passed || res.Test.Mode != flowtest.ModeReplay || res.Test.Calls != 2 || res.Test.Replayed != 2 || res.Test.Expected != 5 || res.Test.Held != 5 {
		t.Fatalf("test = %+v", res.Test)
	}
	for _, p := range []string{"record/trigger.json", "record/state.json", "tests/from-" + c.Run + ".yaml"} {
		if !exists(filepath.Join(bundle, p)) {
			t.Errorf("no %s", p)
		}
	}
	if got := len(readLines(t, gh)); got != live {
		t.Errorf("the replay reached gh: %d -> %d calls", live, got)
	}

	// The passing test is of the new bundle: the flow activates.
	var a applied
	e.okJSON(&a, "flow", "set", "dependency-watch", "--state", "active", "--yes")
	if a.State != "active" {
		t.Fatalf("activation after from-run: %+v", a)
	}
	// And a plain replay holds the same.
	if tr, r := e.flowTest("dependency-watch"); r.Exit != 0 || !tr.Passed || tr.Held != 5 {
		t.Fatalf("replay: exit %d, %+v", r.Exit, tr)
	}
}
