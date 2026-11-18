//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

// ghExec answers `aps run <profile> -- sh -c 'gh api ...'` from testdata.
type ghExec struct{ calls int }

func (g *ghExec) Run(cmd *exec.Cmd) error {
	g.calls++
	args := cmd.Args
	path, out := args[len(args)-2], args[len(args)-1]
	src := "advisories_go_x_net.json"
	if strings.Contains(path, "/releases") {
		src = "releases_cli_cli.json"
	}
	b, err := os.ReadFile(filepath.Join("..", "testdata", "github", src))
	if err != nil {
		return err //nolint:wrapcheck // test
	}
	return os.WriteFile(out, b, 0o600) //nolint:gosec // a temp step directory
}
func (g *ghExec) Output(cmd *exec.Cmd) ([]byte, error) { return nil, g.Run(cmd) }

type flowRig struct {
	t      *testing.T
	eng    *serve.Engine
	live   *ghExec
	bundle string
}

func newFlowRig(t *testing.T) *flowRig {
	t.Helper()
	t.Setenv(EnvProfile, "jad")
	t.Setenv(EnvTenant, "")
	old := promptSource
	promptSource = func() *kitcli.PromptTTY { return nil }
	t.Cleanup(func() { promptSource = old })
	r := &flowRig{t: t, live: &ghExec{}, bundle: filepath.Join(t.TempDir(), "dependency-watch")}
	err := filepath.WalkDir(filepath.Join("..", "testdata", "flows", "dependency-watch"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join("..", "testdata", "flows", "dependency-watch"), p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(r.bundle, rel), 0o750)
		}
		b, err := os.ReadFile(p) //nolint:gosec // testdata
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(r.bundle, rel), b, 0o600) //nolint:gosec // a temp directory
	})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := serve.Open(context.Background(), serve.Config{Root: t.TempDir(), Owner: "jad", NoSync: true, Exec: r.live})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	r.eng = eng
	if _, err := eng.Grants.Set(context.Background(), core.DefaultTenant, "automation", "vcs.read"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *flowRig) run(args ...string) (stdout, stderr string, err error) {
	r.t.Helper()
	root := NewRootOver(func(context.Context) (*serve.Engine, error) { return r.eng, nil })
	var out, errOut bytes.Buffer
	root.Cmd.SetOut(&out)
	root.Cmd.SetErr(&errOut)
	root.Cmd.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err = root.Execute(context.Background())
	return out.String(), errOut.String(), err
}

func (r *flowRig) stored() int {
	r.t.Helper()
	vs, err := r.eng.Services.Flows.ListVersions(context.Background(), core.DefaultTenant, "dependency-watch")
	if err != nil && !core.IsCode(err, core.CodeFlowNotFound) {
		r.t.Fatal(err)
	}
	return len(vs)
}

func wantCode(t *testing.T, err error, code core.Code) {
	t.Helper()
	if !core.IsCode(err, code) || ExitCode(err) != code.Exit() {
		t.Fatalf("err = %v (exit %d), want %s", err, ExitCode(err), code)
	}
}

// F6: a flow apply is a write, so it asks, as flow set and run cancel do.
func TestFlowApplyNeedsConfirmation(t *testing.T) {
	r := newFlowRig(t)
	_, _, err := r.run("flow", "apply", "-f", r.bundle)
	wantCode(t, err, core.CodeConfirmationRequired)
	if r.stored() != 0 {
		t.Fatal("an unconfirmed apply stored a version")
	}

	promptSource = kitcli.PromptSourceFromReadWriter(strings.NewReader("n\n"), io.Discard)
	_, _, err = r.run("flow", "apply", "-f", r.bundle)
	wantCode(t, err, core.CodeConfirmationRequired)
	if r.stored() != 0 {
		t.Fatal("a declined apply stored a version")
	}

	var q bytes.Buffer
	promptSource = kitcli.PromptSourceFromReadWriter(strings.NewReader("y\n"), &q)
	if _, _, err = r.run("flow", "apply", "-f", r.bundle); err != nil || !strings.Contains(q.String(), "dependency-watch") {
		t.Fatalf("a confirmed apply: %v (asked %q)", err, q.String())
	}
	if r.stored() != 1 {
		t.Fatal("a confirmed apply stored nothing")
	}
}

func TestFlowApplyYesAndDryRun(t *testing.T) {
	r := newFlowRig(t)
	if _, _, err := r.run("flow", "apply", "-f", r.bundle, "--dry-run"); err != nil || r.stored() != 0 {
		t.Fatalf("--dry-run asks nothing and stores nothing: %v, stored %d", err, r.stored())
	}
	if _, _, err := r.run("flow", "apply", "-f", r.bundle, "--yes"); err != nil || r.stored() != 1 {
		t.Fatalf("--yes: %v, stored %d", err, r.stored())
	}
}

func TestFlowTestAndActivation(t *testing.T) {
	r := newFlowRig(t)
	if _, _, err := r.run("flow", "apply", "-f", r.bundle, "--yes"); err != nil {
		t.Fatal(err)
	}
	// activation before any test
	_, _, err := r.run("flow", "set", "dependency-watch", "--state", "active", "--yes")
	wantCode(t, err, core.CodeFlowNotTested)

	// a replay with no recording fails (exit 1) and says what was missing
	out, _, err := r.run("flow", "test", "dependency-watch", "--format", "json")
	wantCode(t, err, core.CodeRunFailed)
	var res flowtest.Result
	decodeJSON(t, out, &res)
	if res.Passed || res.Mode != flowtest.ModeReplay || !slices.ContainsFunc(res.Problems, func(p flowtest.Problem) bool { return p.Kind == flowtest.ProblemCassette }) {
		t.Fatalf("replay without recording: %+v", res)
	}

	// recording asks first: it makes live calls and writes into the bundle
	_, _, err = r.run("flow", "test", "dependency-watch", "--record")
	wantCode(t, err, core.CodeConfirmationRequired)
	if r.live.calls != 0 {
		t.Fatal("an unconfirmed recording made live calls")
	}
	if _, _, err = r.run("flow", "test", "dependency-watch", "--record", "--dry-run"); err != nil || r.live.calls != 0 {
		t.Fatalf("--dry-run: %v, live calls %d", err, r.live.calls)
	}
	out, _, err = r.run("flow", "test", "dependency-watch", "--record", "--yes", "--format", "json")
	if err != nil {
		t.Fatalf("record: %v\n%s", err, out)
	}
	res = flowtest.Result{}
	decodeJSON(t, out, &res)
	if !res.Passed || res.Mode != flowtest.ModeRecord || r.live.calls != 2 {
		t.Fatalf("record: %+v, live calls %d", res, r.live.calls)
	}

	out, _, err = r.run("flow", "test", "dependency-watch", "--replay", "--format", "json")
	if err != nil || r.live.calls != 2 {
		t.Fatalf("replay: %v, live calls %d", err, r.live.calls)
	}
	res = flowtest.Result{}
	decodeJSON(t, out, &res)
	if !res.Passed || res.Replayed != 2 {
		t.Fatalf("replay: %+v", res)
	}

	var set struct{ State string }
	out, _, err = r.run("flow", "set", "dependency-watch", "--state", "active", "--yes", "--format", "json")
	if err != nil {
		t.Fatalf("activation after a passing test: %v", err)
	}
	decodeJSON(t, out, &set)
	if set.State != "active" {
		t.Errorf("set = %+v", set)
	}
}

func TestFlowTestFlags(t *testing.T) {
	r := newFlowRig(t)
	if _, _, err := r.run("flow", "apply", "-f", r.bundle, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run("flow", "test", "dependency-watch", "--record", "--replay", "--yes"); err == nil {
		t.Error("--record and --replay together")
	}
	_, _, err := r.run("flow", "test", "dependency-watch", "--trigger-file", filepath.Join(t.TempDir(), "nope.json"))
	wantCode(t, err, core.CodeUsage)
	_, _, err = r.run("flow", "test", "dependency-watch", "--var", "nope=1")
	wantCode(t, err, core.CodeInvalidVar)
	_, _, err = r.run("flow", "test", "no-such-flow")
	wantCode(t, err, core.CodeFlowNotFound)
}
