//go:build !windows

package flowtest_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/flowtest"
)

// liveExec runs the real process: the tests use sh, which is always there.
func record(t *testing.T) (*flowtest.Tape, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cassettes")
	tp, err := flowtest.NewTape(flowtest.ModeRecord, dir)
	if err != nil {
		t.Fatal(err)
	}
	return tp, dir
}

func replay(t *testing.T, dir string) *flowtest.Tape {
	t.Helper()
	tp, err := flowtest.NewTape(flowtest.ModeReplay, dir)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

// panicExec fails the test when a replay starts a process.
type panicExec struct{ t *testing.T }

func (p panicExec) Run(cmd *exec.Cmd) error {
	p.t.Fatalf("replay started a process: %v", cmd.Args)
	return nil
}
func (p panicExec) Output(cmd *exec.Cmd) ([]byte, error) { return nil, p.Run(cmd) }

func sh(script string, args ...string) *exec.Cmd {
	return exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...) //nolint:gosec // test scripts
}

func TestExecRecordThenReplay(t *testing.T) {
	tp, dir := record(t)
	var out, errOut strings.Builder
	cmd := sh(`echo out; echo err >&2; exit 3`)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := tp.Exec(real.Exec{}).Run(cmd)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("record: err=%v out=%q err=%q", err, out.String(), errOut.String())
	}

	rp := replay(t, dir)
	out.Reset()
	errOut.Reset()
	cmd = sh(`echo out; echo err >&2; exit 3`)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = rp.Exec(panicExec{t}).Run(cmd)
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("replay: err=%v out=%q err=%q", err, out.String(), errOut.String())
	}
	if calls, replayed := rp.Stats(); calls != 1 || replayed != 1 {
		t.Errorf("stats = %d/%d", calls, replayed)
	}
}

func TestExecReplayMissIsNamed(t *testing.T) {
	_, dir := record(t)
	rp := replay(t, dir)
	err := rp.Exec(panicExec{t}).Run(sh(`echo never`))
	if err == nil || !strings.Contains(err.Error(), "no recording for") || !strings.Contains(err.Error(), "echo never") || !strings.Contains(err.Error(), "--record") {
		t.Fatalf("err = %v", err)
	}
	if m := rp.Misses(); len(m) != 1 {
		t.Errorf("misses = %v", m)
	}
}

func TestExecStdinIsPartOfTheMatch(t *testing.T) {
	tp, dir := record(t)
	cmd := sh(`cat`)
	cmd.Stdin = strings.NewReader("one")
	cmd.Stdout = io.Discard
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil {
		t.Fatal(err)
	}
	rp := replay(t, dir)
	cmd = sh(`cat`)
	cmd.Stdin = strings.NewReader("two")
	if err := rp.Exec(panicExec{t}).Run(cmd); err == nil || !strings.Contains(err.Error(), "no recording") {
		t.Fatalf("other stdin must miss: %v", err)
	}
}

// A command node's file output lives in a per-attempt scratch directory whose
// name changes: the cassette matches through a placeholder and brings the file
// back into the new directory.
func TestExecStepDirFilesReplayInANewDirectory(t *testing.T) {
	tp, dir := record(t)
	rec := filepath.Join(t.TempDir(), "flo-step-111")
	if err := os.Mkdir(rec, 0o750); err != nil {
		t.Fatal(err)
	}
	cmd := sh(`printf '{"ok":true}' > "$1/out.json"`, rec)
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil {
		t.Fatal(err)
	}

	rp := replay(t, dir)
	rep := filepath.Join(t.TempDir(), "flo-step-222")
	if err := os.Mkdir(rep, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := rp.Exec(panicExec{t}).Run(sh(`printf '{"ok":true}' > "$1/out.json"`, rep)); err != nil {
		t.Fatalf("replay in another step dir: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(rep, "out.json")) //nolint:gosec // the test's own directory
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("restored file = %q, %v", b, err)
	}
}

func TestExecEnvValuesAreNotRecorded(t *testing.T) {
	tp, dir := record(t)
	cmd := sh(`true`)
	cmd.Env = append(os.Environ(), "FLO_TEST_TOKEN=hunter2-value")
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // the test's own directory
		if strings.Contains(string(b), "hunter2") {
			t.Errorf("%s keeps an env value:\n%s", e.Name(), b)
		}
	}
}

func TestHTTPRecordThenReplay(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "" {
			t.Errorf("the live call lost its Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"message":"label not in set"}`)
	}))
	defer srv.Close()

	tp, dir := record(t)
	do := func(h interface {
		Do(*http.Request) (*http.Response, error)
	}) (int, string, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/contract/invoke", strings.NewReader(`{"contract":"c"}`))
		req.Header.Set("Authorization", "Bearer "+"abcdefghijklmnopqrstuvwxyz0123")
		resp, err := h.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck // test
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	st, ct, body := do(tp.HTTP(real.NewHTTP(nil)))
	if st != 422 || ct != "application/json" || !strings.Contains(body, "label not in set") || calls != 1 {
		t.Fatalf("record: %d %s %s calls=%d", st, ct, body, calls)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // the test's own directory
		if strings.Contains(string(b), "abcdefghijkl") || strings.Contains(strings.ToLower(string(b)), "authorization") {
			t.Errorf("%s keeps the credential:\n%s", e.Name(), b)
		}
	}

	rp := replay(t, dir)
	srv.Close()
	st, ct, body = do(rp.HTTP(real.NewHTTP(nil)))
	if st != 422 || ct != "application/json" || !strings.Contains(body, "label not in set") || calls != 1 {
		t.Fatalf("replay: %d %s %s calls=%d", st, ct, body, calls)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/other", nil)
	if _, err := rp.HTTP(real.NewHTTP(nil)).Do(req); err == nil || !strings.Contains(err.Error(), "no recording for GET http://example.invalid/other") {
		t.Fatalf("a call without a cassette: %v", err)
	}
}
