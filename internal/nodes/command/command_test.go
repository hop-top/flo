package command_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/command"
	"hop.top/flo/internal/nodes/schema"
	"hop.top/flo/spec/node"
)

const (
	githubData = "../../../testdata/github/"
	examples   = "../../../spec/node/testdata/examples/"
)

func spec(t *testing.T, y string) *node.Spec {
	t.Helper()
	s, err := node.Decode([]byte(y), "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func exampleSpec(t *testing.T, name string) *node.Spec {
	t.Helper()
	b, err := os.ReadFile(examples + name) //nolint:gosec // repo fixture
	if err != nil {
		t.Fatal(err)
	}
	return spec(t, string(b))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(githubData + name) //nolint:gosec // repo fixture
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func invoke(s *node.Spec, in map[string]any) core.Invocation {
	return core.Invocation{
		Tenant: "default", Run: core.RunContext{ID: "run_1", Flow: "f", Version: "1"},
		StepID: "s", Kind: core.StepKindNode, Node: s, In: in, RunAs: "noor", Attempt: 1,
	}
}

// ---- Runner over the Exec seam ----

// fakeExec is a scripted sideeffect.Exec: it records the command and writes
// what the script says to the command's streams.
type fakeExec struct {
	calls  [][]string
	envs   [][]string
	stdins []string
	fn     func(cmd *exec.Cmd) error
}

func (f *fakeExec) Run(cmd *exec.Cmd) error {
	f.calls = append(f.calls, slices.Clone(cmd.Args))
	f.envs = append(f.envs, slices.Clone(cmd.Env))
	if cmd.Stdin != nil {
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := cmd.Stdin.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		f.stdins = append(f.stdins, b.String())
	}
	if f.fn == nil {
		return nil
	}
	return f.fn(cmd)
}

func (f *fakeExec) Output(*exec.Cmd) ([]byte, error) { return nil, errors.New("unused") }

// realRunner runs real processes. Its aps binary is a path that does not
// exist, so a test that names a profile by mistake cannot start aps.
func realRunner() *command.Runner {
	return &command.Runner{Exec: real.Exec{}, Aps: "/nonexistent/aps-never-run-in-tests"}
}

func TestRunnerRunsUnderApsProfile(t *testing.T) {
	// node.md §4 example: gh api over an aps profile
	s := spec(t, `
node: gh/sbom@1
kind: command
argv: [gh, api, "repos/{{ in.repo }}/dependency-graph/sbom"]
env: { REPO: "{{ in.repo }}" }
stdin: { json: { repo: "{{ in.repo }}", n: "{{ in.n }}" } }
inputs:
  repo: { type: string, required: true }
  n: { type: integer, default: 2 }
outputs:
  from: stdout
  format: json
  schema: { type: object, required: [sbom] }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
`)
	sbom := fixture(t, "sbom_spf13_cobra.json")
	fx := &fakeExec{fn: func(cmd *exec.Cmd) error {
		_, _ = cmd.Stdout.Write(sbom)
		_, _ = cmd.Stderr.Write([]byte("aps: running as noor\n"))
		return nil
	}}
	x := &command.Executor{Commands: command.NewRunner(fx)}
	inv := invoke(s, map[string]any{"repo": "spf13/cobra", "n": int64(2)})
	inv.IdempotencyKey = "run_1/s/0"
	res, err := x.Execute(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aps", "run", "noor", "--", "gh", "api", "repos/spf13/cobra/dependency-graph/sbom"}
	if len(fx.calls) != 1 || !slices.Equal(fx.calls[0], want) {
		t.Fatalf("exec saw %q, want %q", fx.calls, want)
	}
	if !slices.Contains(fx.envs[0], "REPO=spf13/cobra") || !slices.Contains(fx.envs[0], "FLO_IDEMPOTENCY_KEY=run_1/s/0") {
		t.Fatalf("env missing REPO or FLO_IDEMPOTENCY_KEY: %v", fx.envs[0][len(fx.envs[0])-2:])
	}
	if fx.stdins[0] != `{"n":2,"repo":"spf13/cobra"}` {
		t.Fatalf("stdin = %s", fx.stdins[0])
	}
	if res.Outcome != core.OutcomeOK || res.Effect != core.EffectDone {
		t.Fatalf("res = %+v", res)
	}
	raw := res.Raw.(map[string]any)
	if raw["stderr"] != "aps: running as noor\n" || raw["exit_code"] != int64(0) {
		t.Fatalf("raw = %v (stdout and stderr stay separate)", raw)
	}
	if cerr := schema.CheckOutput(s, res.Output); cerr != nil {
		t.Fatal(cerr)
	}
}

func TestRunnerWithoutProfileRunsDirect(t *testing.T) {
	fx := &fakeExec{}
	_, err := command.NewRunner(fx).Run(context.Background(), core.CommandSpec{Argv: []string{"echo", "hi"}})
	if err != nil || !slices.Equal(fx.calls[0], []string{"echo", "hi"}) {
		t.Fatalf("calls=%v err=%v", fx.calls, err)
	}
}

func TestRunnerStartFailure(t *testing.T) {
	_, err := realRunner().Run(context.Background(), core.CommandSpec{Argv: []string{"/nonexistent/flo-test-binary"}})
	if err == nil {
		t.Fatal("want an error: the process could not be started")
	}
	// and the node reports it as a step failure with no effect
	s := spec(t, "node: x/a@1\nkind: command\nargv: [/nonexistent/flo-test-binary]\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
	x := &command.Executor{Commands: realRunner()}
	inv := invoke(s, nil)
	inv.RunAs = "" // direct, no aps
	res, rerr := x.Execute(context.Background(), inv)
	if rerr != nil || res.Outcome != core.OutcomeFailed || res.Effect != core.EffectNone || res.Err.Code != core.CodeRunFailed {
		t.Fatalf("res=%+v err=%v", res, rerr)
	}
}

func TestRunnerTimeoutKillsProcessGroupKeepsPartial(t *testing.T) {
	r := realRunner()
	start := time.Now()
	res, err := r.Run(context.Background(), core.CommandSpec{
		Argv: []string{"sh", "-c", "echo partial; echo oops >&2; sleep 30; echo done"}, Timeout: 400 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// the child of sh holds the pipes: returning before WaitDelay proves the group died
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %s: the process group was not killed", d)
	}
	if !res.TimedOut || string(res.Stdout) != "partial\n" || string(res.Stderr) != "oops\n" {
		t.Fatalf("res = %+v stdout=%q", res, res.Stdout)
	}
}

func TestRunnerCapsAndFlagsTruncation(t *testing.T) {
	res, err := realRunner().Run(context.Background(), core.CommandSpec{
		Argv: []string{"sh", "-c", "echo 0123456789abcdef; echo err0123456789abcdef >&2"}, MaxOutput: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || string(res.Stdout) != "01234567" || string(res.Stderr) != "err01234" || res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunnerExitCode(t *testing.T) {
	res, err := realRunner().Run(context.Background(), core.CommandSpec{Argv: []string{"sh", "-c", "exit 75"}})
	if err != nil || res.ExitCode != 75 || res.TimedOut {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// ---- Executor over the CommandRunner port ----

func TestDependencyListOverFakeExec(t *testing.T) {
	s := exampleSpec(t, "gh_dependency-list_1.yaml")
	cmds := &coretest.Commands{}
	var seen core.CommandSpec
	cmds.On([]string{"gh-deps"}, func(c core.CommandSpec) (core.CommandResult, error) {
		seen = c
		// the tool writes its JSON to the file named by --out, not to stdout
		i := slices.Index(c.Argv, "--out")
		var sbom struct {
			SBOM struct {
				Packages []struct {
					Name, VersionInfo string
					ExternalRefs      []struct{ ReferenceLocator string }
				}
			}
		}
		if err := json.Unmarshal(fixture(t, "sbom_spf13_cobra.json"), &sbom); err != nil {
			t.Fatal(err)
		}
		var pkgs []any
		for _, p := range sbom.SBOM.Packages {
			if len(p.ExternalRefs) == 0 || !strings.HasPrefix(p.ExternalRefs[0].ReferenceLocator, "pkg:golang/") {
				continue
			}
			pkgs = append(pkgs, map[string]any{"name": p.Name, "ecosystem": "go", "version": p.VersionInfo, "used_by": []any{"spf13/cobra"}})
		}
		b, _ := json.Marshal(map[string]any{"packages": pkgs, "skipped": []any{}})
		if err := os.WriteFile(c.Argv[i+1], b, 0o600); err != nil {
			t.Fatal(err)
		}
		return core.CommandResult{Stdout: []byte("noise on stdout that is not JSON")}, nil
	})
	x := &command.Executor{Commands: cmds}
	in, ierr := schema.ApplyInputs(s.Inputs, map[string]any{"repos": []any{"spf13/cobra", "hop-top/kit"}})
	if ierr != nil {
		t.Fatal(ierr)
	}
	res, err := x.Execute(context.Background(), invoke(s, in))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("res = %+v", res.Err)
	}
	if cerr := schema.CheckOutput(s, res.Output); cerr != nil {
		t.Fatalf("output must validate: %v", cerr)
	}
	pkgs := res.Output.(map[string]any)["packages"].([]any)
	if len(pkgs) == 0 {
		t.Fatal("no packages")
	}
	// list-valued whole-element placeholder expanded; step_dir was real; profile passed
	wantTail := []string{"--", "spf13/cobra", "hop-top/kit"}
	if !slices.Equal(seen.Argv[len(seen.Argv)-3:], wantTail) || seen.Profile != "noor" || seen.Argv[2] != "sbom" {
		t.Fatalf("argv = %q profile=%q", seen.Argv, seen.Profile)
	}
	dir := filepath.Dir(seen.Argv[slices.Index(seen.Argv, "--out")+1])
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("step dir %s must be removed after the output is read", dir)
	}
}

func TestEmptyReleasesIsEmptyList(t *testing.T) {
	s := spec(t, `
node: gh/releases@1
kind: command
argv: [gh, api, "repos/{{ in.repo }}/releases"]
inputs: { repo: { type: string, required: true } }
outputs: { from: stdout, format: json, schema: { type: array } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
`)
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"gh", "api"}, core.CommandResult{Stdout: fixture(t, "releases_empty.json")})
	res, err := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, map[string]any{"repo": "x/y"}))
	if err != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	list, ok := res.Output.([]any)
	if !ok || list == nil || len(list) != 0 {
		t.Fatalf("output = %#v, want []", res.Output)
	}
	if cerr := schema.CheckOutput(s, res.Output); cerr != nil {
		t.Fatal(cerr)
	}
}

const failSpec = `
node: x/check@1
kind: command
argv: [tool, "{{ in.arg }}"]
exit_codes: { ok: [0, 2], retry: [75] }
inputs: { arg: { type: [string, 'null'] } }
outputs:
  from: stdout
  format: json
  fail_when: "{{ out.errors > 0 }}"
  schema: { type: object }
side_effect: write-shared
privacy: local
capabilities: [tasks.write]
`

func TestFailWhenKeepsOutput(t *testing.T) {
	s := spec(t, failSpec)
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"tool"}, core.CommandResult{Stdout: []byte(`{"errors": 2, "detail": "x"}`)})
	res, err := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, map[string]any{"arg": nil}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeNodeReportedFailure || res.Effect != core.EffectDone {
		t.Fatalf("res = %+v", res)
	}
	if res.Output.(map[string]any)["detail"] != "x" {
		t.Fatalf("output must be kept: %v", res.Output)
	}
	if got := cmds.Calls()[0].Argv; !slices.Equal(got, []string{"tool", ""}) {
		t.Fatalf("null renders as empty string: %q", got)
	}
	// exit 0 with no reported errors passes
	cmds2 := &coretest.Commands{}
	cmds2.Reply([]string{"tool"}, core.CommandResult{Stdout: []byte(`{"errors": 0}`)})
	res, _ = (&command.Executor{Commands: cmds2}).Execute(context.Background(), invoke(s, map[string]any{"arg": "a"}))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("res = %+v", res)
	}
}

func TestExitCodes(t *testing.T) {
	s := spec(t, failSpec)
	run := func(code int, stdout string) core.Result {
		cmds := &coretest.Commands{}
		cmds.Reply([]string{"tool"}, core.CommandResult{ExitCode: code, Stdout: []byte(stdout), Stderr: []byte("boom")})
		res, err := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, map[string]any{"arg": "a"}))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(2, `{"errors":0}`); res.Outcome != core.OutcomeOK {
		t.Fatalf("exit 2 is listed ok: %+v", res.Err)
	}
	retry := run(75, "")
	if retry.Outcome != core.OutcomeFailed || !retry.Err.Retryable() || retry.Effect != core.EffectNone {
		t.Fatalf("retry code is a transient failure of no effect: %+v", retry)
	}
	perm := run(1, "")
	if perm.Outcome != core.OutcomeFailed || perm.Err.Retryable() || perm.Err.Code != core.CodeRunFailed || perm.Effect != core.EffectDone {
		t.Fatalf("other code is permanent: %+v", perm)
	}
	if !strings.Contains(perm.Err.Message, "boom") {
		t.Fatalf("message should carry stderr: %q", perm.Err.Message)
	}
}

func TestTimeoutIsTransientUnknownAndKeepsPartial(t *testing.T) {
	s := spec(t, "node: x/slow@1\nkind: command\nargv: [sh, -c, 'echo partial; sleep 30']\ntimeout: 300ms\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: write-shared\nprivacy: local\n")
	x := &command.Executor{Commands: realRunner()}
	inv := invoke(s, nil)
	inv.RunAs = ""
	res, err := x.Execute(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeTimeout || !res.Err.Retryable() || res.Effect != core.EffectUnknown {
		t.Fatalf("res = %+v", res)
	}
	raw := res.Raw.(map[string]any)
	if raw["stdout"] != "partial\n" || raw["timed_out"] != true {
		t.Fatalf("partial output must be kept: %v", raw)
	}
}

func TestOutputsFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		outputs string
		want    any
	}{
		"stderr":    {"{ from: stderr, schema: {} }", "err\n"},
		"lines":     {"{ from: stdout, format: lines, schema: {} }", []any{"a", "b"}},
		"text":      {"{ from: stdout, format: text, schema: {} }", "a\n\nb\n"},
		"exit_code": {"{ from: exit_code, schema: { type: integer } }", int64(0)},
		"map":       {"{ from: stdout, format: lines, map: \"{{ out.size() }}\", schema: {} }", int64(2)},
	} {
		t.Run(name, func(t *testing.T) {
			s := spec(t, "node: x/o@1\nkind: command\nargv: [t]\ninputs: {}\noutputs: "+tc.outputs+"\ncapabilities: []\nside_effect: read\nprivacy: local\n")
			cmds := &coretest.Commands{}
			cmds.Reply([]string{"t"}, core.CommandResult{Stdout: []byte("a\n\nb\n"), Stderr: []byte("err\n")})
			res, err := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, nil))
			if err != nil || res.Outcome != core.OutcomeOK {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if !jsonEqual(res.Output, tc.want) {
				t.Fatalf("output = %#v, want %#v", res.Output, tc.want)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestExecOutputIsRawResult(t *testing.T) {
	s := spec(t, "node: x/e@1\nkind: command\nargv: [t]\ninputs: {}\noutputs: { from: exec, schema: { type: object, required: [exit_code, stdout, stderr, duration_ms, truncated] } }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"t"}, core.CommandResult{Stdout: []byte("o"), Duration: 5 * time.Millisecond})
	res, _ := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, nil))
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("res = %+v", res)
	}
	if cerr := schema.CheckOutput(s, res.Output); cerr != nil {
		t.Fatal(cerr)
	}
}

func TestOutputFileMissingOrOutside(t *testing.T) {
	for name, from := range map[string]string{"missing": "file:nope.json", "escape": "file:../x.json"} {
		t.Run(name, func(t *testing.T) {
			s := spec(t, "node: x/f@1\nkind: command\nargv: [t]\ninputs: {}\noutputs: { from: '"+from+"', format: json, schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
			cmds := &coretest.Commands{}
			cmds.Reply([]string{"t"}, core.CommandResult{})
			res, _ := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, nil))
			if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid {
				t.Fatalf("res = %+v", res)
			}
		})
	}
}

func TestStepDirPerAttempt(t *testing.T) {
	s := spec(t, "node: x/d@1\nkind: command\nargv: [t, '{{ run.step_dir }}']\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"t"}, core.CommandResult{})
	x := &command.Executor{Commands: cmds}
	for i := 0; i < 2; i++ {
		if _, err := x.Execute(context.Background(), invoke(s, nil)); err != nil {
			t.Fatal(err)
		}
	}
	calls := cmds.Calls()
	if calls[0].Argv[1] == calls[1].Argv[1] || calls[0].Argv[1] == "" {
		t.Fatalf("each attempt gets its own step dir: %q %q", calls[0].Argv[1], calls[1].Argv[1])
	}
}

func TestUnresolvedReferenceFailsWithoutRunning(t *testing.T) {
	s := spec(t, "node: x/u@1\nkind: command\nargv: [t, '{{ in.missing }}']\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
	cmds := &coretest.Commands{}
	res, _ := (&command.Executor{Commands: cmds}).Execute(context.Background(), invoke(s, nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeUnresolvedRef || res.Effect != core.EffectNone || len(cmds.Calls()) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestPlanRendersWithoutRunning(t *testing.T) {
	s := exampleSpec(t, "gh_dependency-list_1.yaml")
	in, _ := schema.ApplyInputs(s.Inputs, map[string]any{"repos": []any{"a/b"}})
	cmds := &coretest.Commands{}
	plan, err := (&command.Executor{Commands: cmds}).Plan(invoke(s, in))
	if err != nil {
		t.Fatal(err)
	}
	argv := plan["argv"].([]string)
	if argv[0] != "gh-deps" || argv[len(argv)-1] != "a/b" || len(cmds.Calls()) != 0 || plan["profile"] != "noor" {
		t.Fatalf("plan = %v", plan)
	}
}

// A step an external backend approved reads its proposal and the key that lets
// the target run it once as admission.*, and the key is also its idempotency
// key in the environment; with no approval the namespace is empty.
func TestAdmissionBindingIsReadAsAdmissionNamespace(t *testing.T) {
	s := spec(t, `
node: bos/pay@1
kind: command
argv: [bos, pay, "{{ admission.proposal }}", "{{ admission.idempotency_key }}", "{{ admission.proposal != '' ? 'bound' : 'free' }}"]
inputs: {}
outputs: { from: stdout, format: text }
side_effect: write-shared
privacy: local
capabilities: [billing.pay]
`)
	x := &command.Executor{Commands: command.NewRunner(&fakeExec{fn: func(cmd *exec.Cmd) error { return nil }})}
	inv := invoke(s, map[string]any{})
	inv.Admission = &core.AdmissionBinding{Proposal: "agent_proposals_1", ExecutionKey: "exec_1"}
	p, perr := x.Plan(inv)
	if perr != nil {
		t.Fatal(perr)
	}
	if got := strings.Join(p["argv"].([]string), " "); got != "bos pay agent_proposals_1 exec_1 bound" {
		t.Errorf("bound argv = %q", got)
	}
	p, perr = x.Plan(invoke(s, map[string]any{}))
	if perr != nil {
		t.Fatal(perr)
	}
	if got := strings.Join(p["argv"].([]string), " "); got != "bos pay   free" {
		t.Errorf("unbound argv = %q", got)
	}
}
