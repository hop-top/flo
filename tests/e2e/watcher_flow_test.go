//go:build !windows

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
)

// The dependency-watch flow of flows/ over the github pack of packs/: the
// wrappers run for real, gh is a fake that serves testdata/github and
// testdata/watcher (testdata/watcher/shims/gh), aps is the harness shim.

func needJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the gh wrappers need jq")
	}
}

// watcherEnv is an env with the github pack installed, its wrappers and the
// fake gh on the PATH of the fakes, and the rules classifier configured.
func watcherEnv(t *testing.T) *env {
	t.Helper()
	needJQ(t)
	e := newEnv(t, "FLO_AI_CLASSIFIER=rules")
	pack := filepath.Join(repoRoot, "packs", "github")
	if err := os.MkdirAll(filepath.Join(e.root(), "packs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := cpR(pack, filepath.Join(e.root(), "packs", "github")); err != nil {
		t.Fatalf("install pack: %v\n%s", err, out)
	}
	for _, dir := range []string{filepath.Join(pack, "wrappers"), testdata("watcher", "shims")} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range ents {
			b, err := os.ReadFile(filepath.Join(dir, f.Name())) //nolint:gosec // a file of this repository
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.shims, f.Name()), b, 0o755); err != nil { //nolint:gosec // an executable fake
				t.Fatal(err)
			}
		}
	}
	return e
}

func (e *env) flag(name string, on bool) {
	e.t.Helper()
	p := filepath.Join(e.scratch, name)
	if on {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
}

// watchFlow is the shipped flow with its example repositories swapped for
// the ones the fake gh serves.
func watchFlow(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "dependency-watch.yaml")
	b, err := os.ReadFile(filepath.Join(repoRoot, "flows", "dependency-watch.yaml")) //nolint:gosec // a file of this repository
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil { //nolint:gosec // a file in the test's temp directory
		t.Fatal(err)
	}
	rewrite(t, dst, "[example-org/api, example-org/web, example-org/worker]", "[example-org/api, example-org/web, example-org/off, example-org/gone]")
	return dst
}

type finding struct {
	Title  string   `json:"title"`
	Label  string   `json:"label"`
	Detail string   `json:"detail"`
	URL    string   `json:"url"`
	Refs   []string `json:"refs"`
}

func findingsOf(t *testing.T, e *env, run string) []finding {
	t.Helper()
	outs, err := e.store().Outputs.ListOutputs(context.Background(), tenant, core.OutputFilter{Run: core.RunID(run)})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) == 0 {
		return nil
	}
	if len(outs) != 1 || outs[0].Kind != core.OutputFindings {
		t.Fatalf("outputs of %s: %+v", run, outs)
	}
	var fs []finding
	for _, it := range outs[0].Items {
		b, _ := json.Marshal(it)
		var f finding
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		fs = append(fs, f)
	}
	return fs
}

func titles(fs []finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Title
	}
	slices.Sort(out)
	return out
}

func labelOf(fs []finding, title string) string {
	for _, f := range fs {
		if f.Title == title {
			return f.Label
		}
	}
	return "<absent>"
}

func state(t *testing.T, e *env) map[string]any {
	t.Helper()
	s, err := e.store().State.Snapshot(context.Background(), tenant, "dependency-watch")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDependencyWatchFlow(t *testing.T) {
	e := watcherEnv(t)
	flow := watchFlow(t)

	// The flow resolves every gh node from the installed pack.
	if r := e.run("flow", "validate", "-f", flow); r.Exit != 0 {
		t.Fatalf("validate: exit %d\n%s%s", r.Exit, r.Stdout, r.Stderr)
	}
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	var a applied
	e.okJSON(&a, "flow", "apply", "-f", flow)
	e.okJSON(&a, "flow", "set", "dependency-watch", "--version", a.Hash, "--state", "active", "--yes")
	var trig []triggerRow
	e.okJSON(&trig, "trigger", "list", "--flow", "dependency-watch")
	i := slices.IndexFunc(trig, func(x triggerRow) bool { return x.Kind == "schedule" })
	if i < 0 || trig[i].Next == "" || trig[i].State != "active" {
		t.Fatalf("the schedule trigger registers with a next fire: %+v", trig)
	}

	eng := e.startEngine()
	defer eng.stop()

	// First run: advisories and every release newer than what is installed.
	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("first run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	first := findingsOf(t, e, c.Run)
	want := []string{
		"github.com/cli/cli/v2 2.104.0", "github.com/cli/cli/v2 2.105.0",
		"github.com/example-org/oldlib 2.9.0", "github.com/example-org/oldlib 3.0.0",
		"github.com/hop-top/poly-kit/kit 0.5.0-beta.13", "github.com/hop-top/poly-kit/kit 0.5.0-beta.14",
		"golang.org/x/net 0.17.0 GHSA-5cv4-jp36-h3mw", "golang.org/x/net 0.17.0 GHSA-vvgc-356p-c3xw",
		"golang.org/x/net 0.61.0", "golang.org/x/net 0.62.0", "golang.org/x/net 0.63.0",
		"left-pad 1.3.0 GHSA-dddd-eeee-ffff",
	}
	if got := titles(first); !slices.Equal(got, want) {
		t.Fatalf("first run findings:\n got %q\nwant %q", got, want)
	}
	for title, label := range map[string]string{
		"left-pad 1.3.0 GHSA-dddd-eeee-ffff":            "security",
		"golang.org/x/net 0.17.0 GHSA-5cv4-jp36-h3mw":   "security",
		"github.com/example-org/oldlib 3.0.0":           "breaking",
		"github.com/example-org/oldlib 2.9.0":           "routine",
		"github.com/hop-top/poly-kit/kit 0.5.0-beta.14": "routine",
	} {
		if got := labelOf(first, title); got != label {
			t.Errorf("%s labelled %q, want %q", title, got, label)
		}
	}
	for _, f := range first {
		if len(f.Refs) == 0 {
			t.Errorf("%s names no repository that uses it", f.Title)
		}
	}
	s := state(t, e)
	if len(asStrings(s["seen"])) != 9 || len(asStrings(s["seen_adv"])) != 3 || s["cursor"] == nil {
		t.Fatalf("seen was not committed: %v", s)
	}
	// Packages are asked about once, not once per repository that uses them.
	log := strings.Join(readLines(t, filepath.Join(e.scratch, "gh.log")), "\n")
	if n := strings.Count(log, "dependency-graph/sbom"); n != 4 {
		t.Errorf("one SBOM call per repository, got %d\n%s", n, log)
	}
	if n := strings.Count(log, "affects=golang.org/x/net@") + strings.Count(log, ",golang.org/x/net@"); n != 1 {
		t.Errorf("x/net asked about %d times\n%s", n, log)
	}

	// Second run over the same data: everything was seen.
	var c2 created
	if r := e.jsonOf(&c2, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 || c2.Status != "succeeded" {
		t.Fatalf("second run: exit %d %+v\n%s", r.Exit, c2, r.Stderr)
	}
	if status, reason := e.show(c2.Run).step("findings"); status != "skipped" || reason != "when_false" {
		t.Errorf("findings of the deduped run: %s %s", status, reason)
	}
	if fs := findingsOf(t, e, c2.Run); len(fs) != 0 {
		t.Errorf("a deduped run published %q", titles(fs))
	}

	// A failing gh call fails the run and leaves state untouched, even with a
	// new release waiting; the next good run reports exactly that release.
	before := state(t, e)
	e.flag("new-cli-release", true)
	e.flag("fail-advisories", true)
	var c3 created
	if r := e.jsonOf(&c3, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != core.ExitGeneral || c3.Status != "failed" {
		t.Fatalf("failing run: exit %d %+v\n%s", r.Exit, c3, r.Stderr)
	}
	if after := state(t, e); !jsonEqual(before, after) {
		t.Fatalf("a failed run changed state:\nbefore %v\nafter  %v", before, after)
	}
	e.flag("fail-advisories", false)
	var c4 created
	if r := e.jsonOf(&c4, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != 0 || c4.Status != "succeeded" {
		t.Fatalf("run after the failure: exit %d %+v\n%s", r.Exit, c4, r.Stderr)
	}
	if got := titles(findingsOf(t, e, c4.Run)); !slices.Equal(got, []string{"github.com/cli/cli/v2 2.106.0"}) {
		t.Fatalf("a new release is exactly one finding, got %q", got)
	}
	if n := len(asStrings(state(t, e)["seen"])); n != 10 {
		t.Errorf("seen after the new release: %d", n)
	}
	if lines := readLines(t, filepath.Join(e.scratch, "aps.log")); len(lines) == 0 || !strings.HasPrefix(lines[0], "automation gh-deps ") {
		t.Errorf("the wrappers ran as their profile through aps: %v", lines)
	}
}

// A watcher with no classifier backend configured fails at the classify step:
// nothing quietly stands in for the missing judge.
func TestDependencyWatchWithoutClassifier(t *testing.T) {
	e := watcherEnv(t)
	e.extra = nil
	flow := watchFlow(t)
	e.ok("grant", "set", "automation", "vcs.read", "--yes")
	var a applied
	e.okJSON(&a, "flow", "apply", "-f", flow)
	e.okJSON(&a, "flow", "set", "dependency-watch", "--version", a.Hash, "--state", "active", "--yes")
	eng := e.startEngine()
	defer eng.stop()
	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "dependency-watch", "--wait"); r.Exit != core.ExitGeneral || c.Status != "failed" {
		t.Fatalf("run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	if status, _ := e.show(c.Run).step("judge"); status != "failed" {
		t.Errorf("judge status %q", status)
	}
	if out := e.ok("run", "show", c.Run, "--format", "json").Stdout; !strings.Contains(out, `"code": "engine_feature"`) {
		t.Errorf("the run does not name the missing backend:\n%s", out)
	}
	if s := state(t, e); len(asStrings(s["seen"])) != 0 {
		t.Errorf("state after a failed run: %v", s)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
