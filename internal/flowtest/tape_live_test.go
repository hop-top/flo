//go:build !windows

package flowtest_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/flowtest"
)

func live(t *testing.T, scrub func(any) any) (*flowtest.Tape, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cassettes")
	tp, err := flowtest.NewTapeWith(flowtest.ModeRecord, dir, flowtest.Options{Live: true, Scrub: scrub})
	if err != nil {
		t.Fatal(err)
	}
	return tp, dir
}

func readAll(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a temp cassette
		if err != nil {
			t.Fatal(err)
		}
		if info, _ := e.Info(); info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", e.Name(), info.Mode())
		}
		b.Write(data)
	}
	return b.String()
}

// A live tape stores no header that can hold a credential, and no env value.
func TestLiveTapeKeepsNoCredentialHeadersAndNoEnvValues(t *testing.T) {
	const bearer, cookie, envVal = "Bearer abcdefghijklmnop0123", "sid=cookievalue12345", "env-value-7f3a9c"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", cookie)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	tp, dir := live(t, nil)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := tp.HTTP(real.NewHTTP(nil)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` || resp.Header.Get("Set-Cookie") != cookie {
		t.Fatalf("the caller must get the response whole: %q %v", body, resp.Header)
	}

	cmd := sh(`echo hi`)
	cmd.Env = append(os.Environ(), "FLO_TEST_VALUE="+envVal)
	cmd.Stdout = io.Discard
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil {
		t.Fatal(err)
	}

	text := readAll(t, dir)
	for _, secret := range []string{bearer, "abcdefghijklmnop0123", "cookievalue12345", envVal} {
		if strings.Contains(text, secret) {
			t.Errorf("a cassette keeps %q", secret)
		}
	}
	if !strings.Contains(text, "application/json") || !strings.Contains(text, "FLO_TEST_VALUE") {
		t.Errorf("what is safe was dropped:\n%s", text)
	}
	if calls, _ := tp.Stats(); calls != 2 || len(tp.Partial()) != 0 {
		t.Errorf("calls %d, partial %v", calls, tp.Partial())
	}
}

// XRR_REDACT_DISABLE cannot switch the credential redaction off for a live
// recording.
func TestLiveTapeIgnoresTheRedactionKillSwitch(t *testing.T) {
	t.Setenv("XRR_REDACT_DISABLE", "1")
	tp, dir := live(t, nil)
	cmd := sh(`echo ghp_` + strings.Repeat("a", 30))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ghp_") {
		t.Fatal("the caller did not get the output")
	}
	if strings.Contains(readAll(t, dir), "ghp_"+strings.Repeat("a", 30)) {
		t.Error("a token pattern was kept with XRR_REDACT_DISABLE set")
	}
}

func TestLiveTapeAppliesTheScrub(t *testing.T) {
	tp, dir := live(t, func(v any) any { return map[string]any{"scrubbed": true} })
	cmd := sh(`echo secret-output`)
	cmd.Stdout = io.Discard
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil {
		t.Fatal(err)
	}
	if text := readAll(t, dir); strings.Contains(text, "secret-output") || !strings.Contains(text, "scrubbed") {
		t.Errorf("cassettes:\n%s", text)
	}
}

// Recording never changes the run: a body over the limit reaches the caller
// whole, and the recording says it is partial.
func TestLiveTapeNeverFailsOrCutsACall(t *testing.T) {
	big := strings.Repeat("x", 8<<20+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, big) }))
	t.Cleanup(srv.Close)
	tp, dir := live(t, nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := tp.HTTP(real.NewHTTP(nil)).Do(req)
	if err != nil {
		t.Fatalf("a live call failed because of the recording: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(body) != len(big) {
		t.Errorf("the caller got %d bytes of %d", len(body), len(big))
	}
	if p := tp.Partial(); len(p) != 1 || !strings.Contains(p[0], "over the size") {
		t.Errorf("partial = %v", p)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("a call that was not kept left %d file(s)", len(ents))
	}
}

func TestLiveTapeNotesACassetteThatCannotBeWritten(t *testing.T) {
	tp, dir := live(t, nil)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("a file where the directory was"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := sh(`echo fine`)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := tp.Exec(real.Exec{}).Run(cmd); err != nil || out.String() != "fine\n" {
		t.Fatalf("the run was changed by a recording fault: %v %q", err, out.String())
	}
	if len(tp.Partial()) != 1 {
		t.Errorf("partial = %v", tp.Partial())
	}
}
