//go:build !windows

package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// anonStub is a ctxt server that accepts anyone and records the
// Authorization header of each call.
type anonStub struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newAnonStub(t *testing.T) *anonStub {
	t.Helper()
	s := &anonStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"q","mode":"hybrid","total":0,"objects":[]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *anonStub) headers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.auths...)
}

// writeConfig writes the user config file of an env: $XDG_CONFIG_HOME/flo/config.yaml.
func (e *env) writeConfig(body string) string {
	e.t.Helper()
	p := filepath.Join(e.cfg, "flo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// ctxtConfigEnv is an env with the ctxt pack installed and the flow that
// searches it applied. The flow's step reaches the server named by
// packs.ctxt.url, wherever that is set.
func ctxtConfigEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	e := newEnv(t, extra...)
	e.installRepoPack("ctxt")
	e.ok("grant", "set", "automation", "knowledge.read", "--yes")
	e.ok("flow", "apply", "-f", copyBundle(t, "config-ctxt"), "--yes")
	return e
}

// TestConfigPackURLFromFileEnvAndFlag: the shipped binary reads packs.ctxt.url
// from the config file, from FLO_PACKS_CTXT_URL and from -c, and the ctxt node
// posts to it; the flag beats the environment, which beats the file.
func TestConfigPackURLFromFileEnvAndFlag(t *testing.T) {
	const dead = "http://127.0.0.1:1" // nothing listens: a call that reaches it fails
	type source struct {
		name  string
		extra func(url string) []string
		file  func(url string) string
		args  func(url string) []string
	}
	for _, s := range []source{
		{name: "file", file: func(u string) string { return "packs:\n  ctxt:\n    url: " + u + "\n" }},
		{name: "env", extra: func(u string) []string { return []string{"FLO_PACKS_CTXT_URL=" + u} }},
		{name: "flag", args: func(u string) []string { return []string{"-c", "packs.ctxt.url=" + u} }},
		{name: "env over file", extra: func(u string) []string { return []string{"FLO_PACKS_CTXT_URL=" + u} },
			file: func(string) string { return "packs:\n  ctxt:\n    url: " + dead + "\n" }},
		{name: "flag over env and file", args: func(u string) []string { return []string{"-c", "packs.ctxt.url=" + u} },
			extra: func(string) []string { return []string{"FLO_PACKS_CTXT_URL=" + dead} },
			file:  func(string) string { return "packs:\n  ctxt:\n    url: " + dead + "\n" }},
	} {
		t.Run(s.name, func(t *testing.T) {
			stub := newCtxtStub(t)
			var extra []string
			if s.extra != nil {
				extra = s.extra(stub.URL)
			}
			e := ctxtConfigEnv(t, append(extra, "FLO_SECRET_DEFAULT_AUTOMATION_CTXT_TOKEN="+ctxtToken)...)
			if s.file != nil {
				e.writeConfig(s.file(stub.URL))
			}
			args := []string{"flow", "test", "config-ctxt", "--record", "--allow-live", "--yes"}
			if s.args != nil {
				args = append(args, s.args(stub.URL)...)
			}
			r := e.ok(args...)
			reqs, auths := stub.calls()
			if len(reqs) != 1 || auths[0] != "Bearer "+ctxtToken {
				t.Fatalf("the ctxt node reached the server %d times, auth %v\n%s", len(reqs), auths, r.Stdout+r.Stderr)
			}
		})
	}
}

// without any source of packs.ctxt.url the node has no server: the test fails
// (the CLI no longer sees an empty config silently as it did before).
func TestConfigPackURLUnset(t *testing.T) {
	e := ctxtConfigEnv(t)
	if r := e.run("flow", "test", "config-ctxt", "--record", "--allow-live", "--yes"); r.Exit == 0 {
		t.Fatalf("flow test passed without packs.ctxt.url\n%s", r.Stdout)
	}
}

// the ctxt token is optional: unset, the node sends no Authorization header;
// set, a bearer. The secret never comes from the config file.
func TestConfigCtxtTokenOptional(t *testing.T) {
	stub := newAnonStub(t)
	e := ctxtConfigEnv(t, "FLO_PACKS_CTXT_URL="+stub.URL)
	e.ok("flow", "test", "config-ctxt", "--record", "--allow-live", "--yes")
	if got := stub.headers(); len(got) != 1 || got[0] != "" {
		t.Fatalf("no secret: Authorization headers %q, want one empty", got)
	}

	// a secret-looking key in the file is not a secret of the store
	e.writeConfig("secrets:\n  ctxt_token: from-the-file\nctxt_token: from-the-file\npacks:\n  ctxt:\n    token: from-the-file\n")
	e.ok("flow", "test", "config-ctxt", "--record", "--allow-live", "--yes")
	if got := stub.headers(); len(got) != 2 || got[1] != "" {
		t.Fatalf("a token in the config file reached the wire: %q", got)
	}

	// the store's secret is used when present
	tok := "stub-" + "optional-token"
	e2 := ctxtConfigEnv(t, "FLO_PACKS_CTXT_URL="+stub.URL, "FLO_SECRET_DEFAULT_AUTOMATION_CTXT_TOKEN="+tok)
	e2.ok("flow", "test", "config-ctxt", "--record", "--allow-live", "--yes")
	if got := stub.headers(); len(got) != 3 || got[2] != "Bearer "+tok {
		t.Fatalf("with a secret: %q", got)
	}
}

// TestConfigStatusShowsSources: `flo status` shows the merged config and where
// it came from.
func TestConfigStatusShowsSources(t *testing.T) {
	e := newEnv(t, "FLO_PACKS_CALENDAR_URL=/env/cal.ics")
	file := e.writeConfig("packs:\n  ctxt:\n    url: http://file.example\n  calendar:\n    url: /file/cal.ics\n")
	var st struct {
		Sections []struct {
			Title string         `json:"title"`
			Data  map[string]any `json:"data"`
		} `json:"sections"`
	}
	e.okJSON(&st, "status", "-c", "tenant=acme")
	got := map[string]map[string]any{}
	for _, s := range st.Sections {
		got[s.Title] = s.Data
	}
	eff := got["effective-config"]
	packs, _ := eff["packs"].(map[string]any)
	ctxt, _ := packs["ctxt"].(map[string]any)
	cal, _ := packs["calendar"].(map[string]any)
	if ctxt["url"] != "http://file.example" || cal["url"] != "/env/cal.ics" || eff["tenant"] != "acme" {
		t.Errorf("effective-config: file, environment and flag in turn: %v", eff)
	}
	src := got["config"]
	files, _ := src["files"].([]any)
	if len(files) != 1 || files[0] != file || src["env_prefix"] != "FLO_" {
		t.Errorf("config sources: %v, want the user file %s", src, file)
	}
}

// icsAt is an ICS calendar of one two-attendee event starting at t.
func icsAt(t time.Time) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//e2e//EN\r\n" +
		vevent("standup", t.UTC().Format("20060102T150405Z"),
			"ATTENDEE:mailto:a@example.com\r\nATTENDEE:mailto:b@example.com\r\n") +
		"END:VCALENDAR\r\n"
}

// TestConfigCalendarFromFileAndEnv: `flo engine serve` reads packs.calendar.url
// from the config file or FLO_PACKS_CALENDAR_URL, syncs the calendar and runs
// the flow of an event whose fire time (start minus 30m) has passed.
func TestConfigCalendarFromFileAndEnv(t *testing.T) {
	for _, via := range []string{"file", "env"} {
		t.Run(via, func(t *testing.T) {
			ics := filepath.Join(t.TempDir(), "work.ics")
			if err := os.WriteFile(ics, []byte(icsAt(time.Now().Add(10*time.Minute))), 0o600); err != nil {
				t.Fatal(err)
			}
			var extra []string
			if via == "env" {
				extra = []string{"FLO_PACKS_CALENDAR_URL=" + ics, "FLO_PACKS_CALENDAR_SYNC_INTERVAL=5m"}
			}
			e := newEnv(t, extra...)
			if via == "file" {
				e.writeConfig("packs:\n  calendar:\n    url: " + ics + "\n    sync_interval: 5m\n")
			}
			event := filepath.Join(t.TempDir(), "event.json")
			if err := os.WriteFile(event, []byte(`{"event":{"title":"standup","start":"2026-11-18T13:00:00Z","attendees":["a@example.org","b@example.org"]}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			bundle := copyBundle(t, "calendar-event")
			for _, args := range [][]string{
				{"grant", "set", "automation", "calendar.read", "--yes"},
				{"flow", "apply", "-f", bundle, "--yes"},
				{"flow", "test", "calendar-event", "--record", "--allow-live", "--trigger-file", event, "--yes"},
				{"flow", "set", "calendar-event", "--state", "active", "--yes"},
			} {
				e.ok(args...)
			}
			e.startEngine()
			eventually(t, "the engine to run the flow of the event the calendar url names", func() bool {
				var runs []struct {
					Status string `json:"status"`
				}
				e.okJSON(&runs, "run", "list", "--flow", "calendar-event")
				return len(runs) == 1 && runs[0].Status == "succeeded"
			})
			if !strings.Contains(e.ok("run", "list", "--flow", "calendar-event").Stdout, "calendar-event") {
				t.Errorf("no run listed")
			}
		})
	}
}
