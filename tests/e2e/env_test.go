//go:build !windows

// Package e2e runs the assembled flo end to end: the built binary against a
// real SQLite store, real jobs and a real run log, in a temporary XDG tree,
// with fake aps, gh, tlc and inbox executables first on PATH. Nothing here
// reaches a real tool or the network.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/store/sqlite"
)

var (
	repoRoot string
	flo      string // the built binary
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	repoRoot = filepath.Dir(filepath.Dir(filepath.Dir(file)))
	dir, err := os.MkdirTemp("", "flo-e2e-bin-")
	if err != nil {
		panic(err)
	}
	flo = filepath.Join(dir, "flo")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", flo, "hop.top/flo") //nolint:gosec // go build into a temp directory
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		panic("go build: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// shell scripts of the fake tools. They log what they were asked to
// $FLO_E2E_DIR and answer from testdata or a state directory there.
var shims = map[string]string{
	// aps run <profile> -- <argv...>: run the argv, as a profile would.
	"aps": `#!/bin/sh
[ "$1" = run ] || { echo "fake aps: unsupported: $*" >&2; exit 64; }
profile=$2
shift 2
[ "$1" = -- ] && shift
echo "$profile $*" >> "$FLO_E2E_DIR/aps.log"
exec "$@"
`,
	// gh api <path>: serves testdata/github. An advisory query for crypto
	// gets other ids than the one for net; the releases call fails while
	// $FLO_E2E_DIR/fail-releases exists.
	"gh": `#!/bin/sh
echo "gh $*" >> "$FLO_E2E_DIR/gh.log"
[ "$1" = api ] || { echo "fake gh: unsupported: $*" >&2; exit 64; }
td="$FLO_E2E_TESTDATA/github"
case "$2" in
  /advisories*crypto*) sed 's/GHSA-/GHSB-/g' "$td/advisories_go_x_net.json" ;;
  /advisories*) cat "$td/advisories_go_x_net.json" ;;
  /repos/*/releases*)
    [ -e "$FLO_E2E_DIR/fail-releases" ] && { echo "gh: HTTP 502" >&2; exit 1; }
    cat "$td/releases_cli_cli.json" ;;
  *) echo "fake gh: unsupported: $*" >&2; exit 64 ;;
esac
`,
	// inbox list|apply: one message waits; apply records what it archived.
	"inbox": `#!/bin/sh
case "$1" in
  list) [ "$2" = --out ] || exit 64
    printf '{"messages":[{"id":"m1","subject":"Invoice","from":"a@example.org"}]}' > "$3" ;;
  apply) [ "$3" = --out ] || exit 64
    echo "applied $2" >> "$FLO_E2E_DIR/applied.log"
    printf '{"applied":"%s"}' "$2" > "$4" ;;
  *) echo "fake inbox: unsupported: $*" >&2; exit 64 ;;
esac
`,
	// tlc [-C dir] task create|show|complete|skip: tasks are files.
	"tlc": `#!/bin/sh
d="$FLO_E2E_DIR/tlc"; mkdir -p "$d"
[ "$1" = -C ] && shift 2
[ "$1" = task ] || { echo "fake tlc: unsupported: $*" >&2; exit 64; }
case "$2" in
  create) n=$(( $(cat "$d/n" 2>/dev/null || echo 0) + 1 )); echo $n > "$d/n"
    id=$(printf 'T-%04d' $n); echo TODO > "$d/$id"; echo "Created task $id: $3" ;;
  show) [ -f "$d/$3" ] || { echo "task $3 not found" >&2; exit 1; }
    printf '{"task":{"status":"%s","title":"t"},"logs":[]}' "$(cat "$d/$3")" ;;
  complete) echo DONE > "$d/$3" ;;
  skip) echo SKIPPED > "$d/$3" ;;
  *) echo "fake tlc: unsupported: $*" >&2; exit 64 ;;
esac
`,
}

// env is one hermetic flo installation: its own HOME, XDG tree, PATH of fakes
// and scratch directory.
type env struct {
	t       *testing.T
	home    string
	data    string
	cfg     string
	shims   string
	scratch string
	extra   []string
}

func newEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{
		t: t, home: filepath.Join(root, "home"), data: filepath.Join(root, "data"), cfg: filepath.Join(root, "config"),
		shims: filepath.Join(root, "shims"), scratch: filepath.Join(root, "scratch"), extra: extra,
	}
	for _, d := range []string{e.home, e.data, e.cfg, e.shims, e.scratch} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(e.shims, name), []byte(body), 0o755); err != nil { //nolint:gosec // an executable fake
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) environ() []string {
	return append([]string{
		"HOME=" + e.home, "XDG_DATA_HOME=" + e.data, "XDG_CONFIG_HOME=" + e.cfg,
		"XDG_STATE_HOME=" + filepath.Join(e.home, "state"), "XDG_CACHE_HOME=" + filepath.Join(e.home, "cache"),
		"PATH=" + e.shims + ":/usr/bin:/bin", "USER=jad", "FLO_PROFILE=jad",
		"FLO_E2E_DIR=" + e.scratch, "FLO_E2E_TESTDATA=" + filepath.Join(repoRoot, "testdata"),
		"FLO_ENGINE_INTERVAL=20ms", "FLO_APPROVAL_POLL=1s",
	}, e.extra...)
}

// root is the engine's data root.
func (e *env) root() string { return filepath.Join(e.data, "flo") }

// result is one flo invocation.
type result struct {
	Stdout, Stderr string
	Exit           int
}

func (e *env) run(args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, flo, args...) //nolint:gosec // the binary this test built
	cmd.Env = e.environ()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	res := result{Stdout: out.String(), Stderr: errOut.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Exit = ee.ExitCode()
	default:
		e.t.Fatalf("flo %s: %v", strings.Join(args, " "), err)
	}
	return res
}

// ok runs flo and fails the test unless it exits 0.
func (e *env) ok(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.Exit != 0 {
		e.t.Fatalf("flo %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Exit, r.Stdout, r.Stderr)
	}
	return r
}

// jsonOf runs flo with --format json and decodes the stdout into v.
func (e *env) jsonOf(v any, args ...string) result {
	e.t.Helper()
	r := e.run(append(args, "--format", "json")...)
	if r.Stdout != "" {
		if err := json.Unmarshal([]byte(r.Stdout), v); err != nil {
			e.t.Fatalf("flo %s: stdout is not JSON: %v\n%s", strings.Join(args, " "), err, r.Stdout)
		}
	}
	return r
}

// okJSON is jsonOf that also requires exit 0.
func (e *env) okJSON(v any, args ...string) {
	e.t.Helper()
	if r := e.jsonOf(v, args...); r.Exit != 0 {
		e.t.Fatalf("flo %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Exit, r.Stdout, r.Stderr)
	}
}

// store opens the engine's database for reading what the CLI does not show.
func (e *env) store() *sqlite.Store {
	e.t.Helper()
	s := sqlite.Open(e.root(), sqlite.Options{})
	e.t.Cleanup(func() { _ = s.Close() })
	return s
}

// testdata returns a path under the repository's testdata.
func testdata(parts ...string) string {
	return filepath.Join(append([]string{repoRoot, "testdata"}, parts...)...)
}

// installPack copies a testdata pack into the engine's packs directory. The
// engine reads packs when it starts.
func (e *env) installPack(name string) {
	e.t.Helper()
	cp := exec.Command("cp", "-R", testdata("packs", name), filepath.Join(e.root(), "packs", name)) //nolint:gosec // a pack of this repository's testdata
	if err := os.MkdirAll(filepath.Join(e.root(), "packs"), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if out, err := cp.CombinedOutput(); err != nil {
		e.t.Fatalf("install pack %s: %v\n%s", name, err, out)
	}
}

// syncBuf is a bytes.Buffer safe for a process writing while a test reads.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// engine is a running `flo engine serve`.
type engine struct {
	t    *testing.T
	cmd  *exec.Cmd
	logs *syncBuf
	exit chan struct{} // closed when the process has exited
}

// startEngine runs `flo engine serve` and returns once it is serving.
func (e *env) startEngine() *engine {
	e.t.Helper()
	p := &engine{t: e.t, logs: &syncBuf{}, exit: make(chan struct{})}
	p.cmd = exec.Command(flo, "engine", "serve")
	p.cmd.Env = e.environ()
	p.cmd.Stderr, p.cmd.Stdout = p.logs, p.logs
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() { _ = p.cmd.Wait(); close(p.exit) }()
	e.t.Cleanup(func() { p.kill() })
	eventually(e.t, "the engine to serve", func() bool {
		select {
		case <-p.exit:
			e.t.Fatalf("the engine exited\n%s", p.logs.String())
		default:
		}
		return strings.Contains(p.logs.String(), "engine serving")
	})
	return p
}

// stop sends SIGTERM and waits for the drain.
func (p *engine) stop() {
	p.t.Helper()
	select {
	case <-p.exit:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exit:
	case <-time.After(30 * time.Second):
		p.t.Fatalf("the engine did not stop on SIGTERM\n%s", p.logs.String())
	}
}

// kill ends the engine at once, as a crash does.
func (p *engine) kill() {
	select {
	case <-p.exit:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.exit:
	case <-time.After(10 * time.Second):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, 20*time.Second, what, cond)
}

func eventuallyWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// runView is the part of `flo run show --format json` the tests read.
type runView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Trigger string `json:"trigger"`
	Failure *struct {
		Code string `json:"code"`
	} `json:"failure"`
	Steps []struct {
		Step   string `json:"step"`
		Status string `json:"status"`
		Reason string `json:"reason"`
		Index  *int   `json:"index"`
	} `json:"steps"`
	Outputs []struct {
		Items int `json:"items"`
	} `json:"outputs"`
}

func (v runView) step(id string) (status, reason string) {
	for _, s := range v.Steps {
		if s.Step == id && s.Index == nil {
			return s.Status, s.Reason
		}
	}
	return "", ""
}

// created is the part of `flo run create --format json` the tests read.
type created struct {
	Run       string   `json:"run"`
	Status    string   `json:"status"`
	Created   bool     `json:"created"`
	WaitingOn []string `json:"waiting_on"`
}

func (e *env) show(run string) runView {
	e.t.Helper()
	var v runView
	e.okJSON(&v, "run", "show", run)
	return v
}

// tenant is the tenant the tests run in.
const tenant = core.DefaultTenant

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a file of the test's scratch directory
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// cpR copies a directory tree.
func cpR(src, dst string) (string, error) {
	out, err := exec.Command("cp", "-R", src, dst).CombinedOutput() //nolint:gosec // paths of this repository's testdata and a temp directory
	return string(out), err
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// rewrite replaces old by new in the file, once.
func rewrite(t *testing.T, path, old, replacement string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a copy of a testdata flow in a temp directory
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s has no %q", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, replacement, 1)), 0o600); err != nil { //nolint:gosec // same file
		t.Fatal(err)
	}
}
