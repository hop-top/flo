//go:build !windows

package e2e

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
)

type engineSection struct {
	Title string `json:"title"`
	Data  struct {
		State string `json:"state"`
		PID   int    `json:"pid"`
	} `json:"data"`
}

func (e *env) engineState() engineSection {
	e.t.Helper()
	var st struct {
		Sections []engineSection `json:"sections"`
	}
	e.okJSON(&st, "status")
	for _, s := range st.Sections {
		if s.Title == "engine" {
			return s
		}
	}
	e.t.Fatalf("no engine section in status: %+v", st)
	return engineSection{}
}

// TestEngineStateAndDrain: flo status reports the engine (F8), a second engine
// on the same root is refused, SIGTERM stops the engine cleanly and a killed
// engine shows as stale.
func TestEngineStateAndDrain(t *testing.T) {
	e := newEnv(t)
	if s := e.engineState(); s.Data.State != "stopped" {
		t.Fatalf("before serve: %+v", s)
	}

	eng := e.startEngine()
	if r := e.run("run", "create", "--flow", "nothing", "--wait"); r.Exit != core.ExitNotFound {
		t.Errorf("run create of an unknown flow: exit %d\n%s", r.Exit, r.Stderr)
	}
	if s := e.engineState(); s.Data.State != "running" || s.Data.PID != eng.cmd.Process.Pid {
		t.Errorf("while serving: %+v", s)
	}
	if r := e.run("engine", "serve"); r.Exit != core.ExitConflict || !strings.Contains(r.Stderr, "duplicate") {
		t.Errorf("a second engine: exit %d\n%s", r.Exit, r.Stderr)
	}
	eng.stop()
	if !strings.Contains(eng.logs.String(), "engine stopped") {
		t.Errorf("no clean stop in the log:\n%s", eng.logs.String())
	}
	if s := e.engineState(); s.Data.State != "stopped" {
		t.Errorf("after SIGTERM: %+v", s)
	}

	killed := e.startEngine()
	killed.kill()
	if s := e.engineState(); s.Data.State != "stale" {
		t.Errorf("after a kill: %+v", s)
	}
	// run create --wait does not wait for an engine that is not there
	if r := e.run("run", "create", "--flow", "nothing", "--wait"); r.Exit != core.ExitTransient || !strings.Contains(r.Stderr, "engine_unreachable") {
		t.Errorf("--wait without an engine: exit %d\n%s", r.Exit, r.Stderr)
	}
	// and a new engine takes over from the dead one
	again := e.startEngine()
	again.stop()
}

// TestNoToolDependencies: the module graph of flo has no heo, ctxt or tlc in
// it. Tools are reached through declared commands, never imported.
func TestNoToolDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	tool := regexp.MustCompile(`(^|[/.])(heo|ctxt|tlc)(/|$)`)
	var hits []string
	for _, p := range strings.Fields(string(out)) {
		if tool.MatchString(p) {
			hits = append(hits, p)
		}
	}
	if len(hits) > 0 {
		t.Errorf("flo imports tool packages: %v", hits)
	}
	if !strings.Contains(string(out), "hop.top/flo/internal/engine/serve") {
		t.Errorf("the check did not see the engine assembly: it lists %d packages", len(strings.Fields(string(out))))
	}
}
