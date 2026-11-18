//go:build !windows

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
)

// The meeting-brief bundle of flows/ on the shipped binary: a calendar fire at
// start minus 30m, the three sources through the fakes of the brief tests, the
// template backend for the brief, `flo brief show`.

const mbFlow = "meeting-brief"

// mbLead is how far ahead of its fire time (start minus 30m) the test's events
// are when the engine starts: long enough that nothing has run by the time the
// test looks, short enough to wait for.
const mbLead = 8 * time.Second

// mbRecordedEvent is the trigger payload a calendar trigger hands the flow for
// the test's two-attendee event.
const mbRecordedEvent = `{"event":{"uid":"review","recurrence_id":null,"start":"2026-11-18T13:00:00Z","end":"2026-11-18T14:00:00Z",` +
	`"title":"Review","location":"Room 4","status":"CONFIRMED","organizer":"ada@example.org",` +
	`"attendees":[{"email":"ada@example.org","name":"Ada Lovelace","status":"accepted"},{"email":"bob@example.org","name":"","status":""}]}}`

func mbICS(start time.Time) string {
	s := start.UTC().Format("20060102T150405Z")
	common := "DTEND:" + start.Add(time.Hour).UTC().Format("20060102T150405Z") + "\r\nLOCATION:Room 4\r\nORGANIZER:mailto:ada@example.org\r\n"
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//e2e//EN\r\n" +
		vevent("Review", s, common+"ATTENDEE;CN=Ada Lovelace;PARTSTAT=ACCEPTED:mailto:ada@example.org\r\nATTENDEE:mailto:bob@example.org\r\n") +
		vevent("Solo", s, common+"ATTENDEE;CN=Ada Lovelace:mailto:ada@example.org\r\n") + // one attendee: the flow's `when` refuses it
		"END:VCALENDAR\r\n"
}

// mbEnv is an env with the three source packs, the fakes, a ctxt stub, an ICS
// calendar and a config file naming both: how a user sets the flow up.
type mbEnv struct {
	*env
	stub   *ctxtStub
	start  time.Time // of the test's events
	event  string    // file of the recorded trigger payload
	bundle string
}

func newMBEnv(t *testing.T) *mbEnv {
	t.Helper()
	stub := newCtxtStub(t)
	e := briefSourcesEnv(t)
	m := &mbEnv{env: e, stub: stub, start: time.Now().Add(30*time.Minute + mbLead).Truncate(time.Second)}
	ics := filepath.Join(t.TempDir(), "work.ics")
	if err := os.WriteFile(ics, []byte(mbICS(m.start)), 0o600); err != nil {
		t.Fatal(err)
	}
	e.writeConfig("packs:\n  ctxt:\n    url: " + stub.URL + "\n  calendar:\n    url: " + ics + "\n    sync_interval: 1s\nai:\n  generator: template\n")
	m.event = filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(m.event, []byte(mbRecordedEvent), 0o600); err != nil {
		t.Fatal(err)
	}
	m.bundle = filepath.Join(t.TempDir(), mbFlow)
	if out, err := cpR(filepath.Join(repoRoot, "flows", mbFlow), m.bundle); err != nil {
		t.Fatalf("copy bundle: %v\n%s", err, out)
	}
	return m
}

func (m *mbEnv) grant() {
	m.t.Helper()
	for _, c := range []string{"knowledge.read", "mail.read", "tasks.read", "calendar.read"} {
		m.ok("grant", "set", "automation", c, "--yes")
	}
}

// record applies the bundle and records its test against the fakes.
func (m *mbEnv) record() flowtest.Result {
	m.t.Helper()
	m.grant()
	m.ok("flow", "apply", "-f", m.bundle, "--yes")
	res, r := m.flowTest(mbFlow, "--record", "--allow-live", "--trigger-file", m.event, "--yes")
	if r.Exit != 0 || !res.Passed {
		m.t.Fatalf("record: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	return res
}

type mbBrief struct {
	Flows []struct {
		Flow   string `json:"flow"`
		Briefs []struct {
			Run   string `json:"run"`
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"briefs"`
	} `json:"flows"`
}

func (m *mbEnv) briefs() (out []struct{ Run, Title, Body string }) {
	var b mbBrief
	m.okJSON(&b, "brief", "show")
	for _, f := range b.Flows {
		if f.Flow != mbFlow {
			continue
		}
		for _, x := range f.Briefs {
			out = append(out, struct{ Run, Title, Body string }{x.Run, x.Title, x.Body})
		}
	}
	return out
}

func (m *mbEnv) runs() []core.Run {
	m.t.Helper()
	runs, err := m.store().Runs.ListRuns(context.Background(), tenant, core.RunFilter{Flow: mbFlow})
	if err != nil {
		m.t.Fatal(err)
	}
	return runs
}

// TestMeetingBriefEndToEnd: the flow is applied, tested on a recording of the
// fakes, replayed offline, activated and left to the engine. At start minus 30m
// the two-attendee event runs it and the one-attendee event does not; the brief
// holds the four sections.
func TestMeetingBriefEndToEnd(t *testing.T) {
	m := newMBEnv(t)
	m.record()
	if reqs, _ := m.stub.calls(); len(reqs) != 2 { // one search per attendee
		t.Fatalf("the recording searched ctxt %d times: %v", len(reqs), reqs)
	}

	// Replay: nothing is reached. The stub is not called again and the logs of
	// the fakes stay empty.
	m.resetScratch()
	recorded, _ := m.stub.calls()
	res, r := m.flowTest(mbFlow)
	if r.Exit != 0 || !res.Passed || res.Mode != flowtest.ModeReplay || res.Expected == 0 || res.Held != res.Expected {
		t.Fatalf("replay: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	for _, f := range []string{"heo.log", "tlc.log"} {
		if got := logOf(m.env, f); len(got) != 0 {
			t.Errorf("the replay ran %s: %v", f, got)
		}
	}
	if reqs, _ := m.stub.calls(); len(reqs) != len(recorded) {
		t.Errorf("the replay reached ctxt: %d calls after %d", len(reqs), len(recorded))
	}
	reqs0 := recorded

	m.ok("flow", "set", mbFlow, "--state", "active", "--yes")
	m.startEngine()
	if got := m.runs(); len(got) != 0 {
		t.Fatalf("a run before the fire time: %v", got)
	}
	eventuallyWithin(t, mbLead+30*time.Second, "the run at start minus 30m", func() bool {
		rs := m.runs()
		return len(rs) == 1 && rs[0].Status == core.RunSucceeded
	})
	time.Sleep(2500 * time.Millisecond) // two more syncs: the solo event never runs, nothing runs twice
	runs := m.runs()
	if len(runs) != 1 {
		t.Fatalf("runs: %v", runs)
	}
	run := runs[0]
	if run.Trigger.Kind != "calendar" || !run.Trigger.At.Equal(m.start.Add(-30*time.Minute)) {
		t.Errorf("trigger %+v, want calendar at %s", run.Trigger, m.start.Add(-30*time.Minute))
	}
	ev, _ := run.Trigger.Payload["event"].(map[string]any)
	if ev["title"] != "Review" {
		t.Errorf("the run is of event %v", ev["title"])
	}

	// The brief, as `flo brief show` has it.
	bs := m.briefs()
	if len(bs) != 1 || bs[0].Title != "Brief: Review" || bs[0].Run != string(run.ID) {
		t.Fatalf("briefs: %+v", bs)
	}
	body := bs[0].Body
	for _, want := range []string{
		"# Brief: Review", "## Event", "Where: Room 4", "Ada Lovelace <ada@example.org> (accepted)", "bob@example.org <bob@example.org>",
		"## People & notes", "3 of 3 notes", "(obj-1; people, platform)", "Ada runs the platform team.",
		"## Threads", "3 of 3 conversations", "c1 [", "c3 [", "c4 [", "last activity 2026-11-16T10:00:00Z",
		"## Open tasks", "T-0101 [IN_PROGRESS, P1] Quarterly review prep (due 2026-11-20T00:00:00Z)", "T-0102 [TODO, P2] Send review notes to Ada",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the brief lacks %q:\n%s", want, body)
		}
	}
	if regexp.MustCompile(`x{201}`).MatchString(body) || !strings.Contains(body, strings.Repeat("x", 200)) {
		t.Errorf("a note's text is not cut to 200 characters")
	}
	for _, leak := range []string{"long text that must not reach the brief", ctxtToken} {
		if strings.Contains(body, leak) {
			t.Errorf("the brief holds %q", leak)
		}
	}
	if strings.Count(body, "- [note]")+strings.Count(body, "- [url]") != 3 {
		t.Errorf("two attendees returned the same objects: want 3 notes after dedupe:\n%s", body)
	}

	// The sources: a search per attendee by name, else the local part of the
	// address, with the secret; the wrappers' calls; the people merged by id.
	reqs, auths := m.stub.calls()
	if len(reqs) != len(reqs0)+2 {
		t.Fatalf("the run searched ctxt %d times", len(reqs)-len(reqs0))
	}
	var queries []string
	for _, q := range reqs[len(reqs0):] {
		queries = append(queries, q["query"].(string))
		if f, _ := q["filter"].(map[string]any); f["person"] != q["query"] {
			t.Errorf("filter %v for query %v", q["filter"], q["query"])
		}
	}
	slices.Sort(queries)
	if !slices.Equal(queries, []string{"Ada Lovelace", "bob"}) {
		t.Errorf("queries %v", queries)
	}
	if a := auths[len(auths)-1]; a != "Bearer "+ctxtToken {
		t.Errorf("authorization %q", a)
	}
	if want := "tlc task list --all-projects --status TODO,IN_PROGRESS --limit 25 --format json -- Review"; !slices.Contains(logOf(m.env, "tlc.log"), want) {
		t.Errorf("tlc calls: %v", logOf(m.env, "tlc.log"))
	}
	_, notes := m.stepOf(string(run.ID), "notes")
	if items, _ := notes["items"].([]any); len(items) != 3 {
		t.Errorf("notes: %v", notes)
	}
	_, brief := m.stepOf(string(run.ID), "brief")
	if p, _ := brief["prompt"].(map[string]any); p["name"] != "meeting-brief" || num(p["version"]) != 1 || len(p["hash"].(string)) != 64 {
		t.Errorf("prompt: %v", brief["prompt"])
	}
}

// TestMeetingBriefEmptySources: no note on anyone, no conversation, no task
// is a brief with empty sections, not a failed run.
func TestMeetingBriefEmptySources(t *testing.T) {
	m := newMBEnv(t)
	m.record()
	m.ok("flow", "set", mbFlow, "--state", "active", "--yes")
	m.startEngine()

	// ctxt is configured per engine start; an event whose people the stores know
	// nothing about still gets its run: unknown addresses, a title no task has.
	ev := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(ev, []byte(`{"event":{"uid":"x","start":"2026-11-18T13:00:00Z","end":"2026-11-18T14:00:00Z","title":"Zzz","location":"","status":"","organizer":"",`+
		`"attendees":[{"email":"nobody@example.org","name":"","status":""},{"email":"nobody2@example.org","name":"","status":""}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var c created
	if r := m.jsonOf(&c, "run", "create", "--flow", mbFlow, "--trigger-file", ev, "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	var body string
	for _, b := range m.briefs() {
		if b.Run == c.Run {
			body = b.Body
		}
	}
	for _, want := range []string{"# Brief: Zzz", "Where: not set", "No conversation with these people in the last 30 days.", `No open task matches "Zzz".`} {
		if !strings.Contains(body, want) {
			t.Errorf("the brief lacks %q:\n%s", want, body)
		}
	}

	// ctxt gone: its searches are skipped, the other sections stay.
	m.stub.Close()
	var d created
	if r := m.jsonOf(&d, "run", "create", "--flow", mbFlow, "--trigger-file", m.event, "--wait"); r.Exit != 0 || d.Status != "succeeded" {
		t.Fatalf("run with ctxt down: exit %d %+v\n%s", r.Exit, d, r.Stderr)
	}
	for _, b := range m.briefs() {
		if b.Run == d.Run && (!strings.Contains(b.Body, "Nothing in the knowledge store on these people.") || !strings.Contains(b.Body, "c1 [open]") || !strings.Contains(b.Body, "T-0101")) {
			t.Errorf("the brief with ctxt down:\n%s", b.Body)
		}
	}
}

// TestMeetingBriefStaysLocal: V19 holds for the bundle, and a hosted model
// under privacy: local-only is refused when the flow is applied.
func TestMeetingBriefStaysLocal(t *testing.T) {
	m := newMBEnv(t)
	m.grant()
	if r := m.run("flow", "apply", "-f", m.bundle, "--yes"); r.Exit != 0 {
		t.Fatalf("the bundle does not apply: exit %d\n%s", r.Exit, r.Stderr)
	}

	bad := filepath.Join(t.TempDir(), mbFlow)
	if out, err := cpR(m.bundle, bad); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rewrite(t, filepath.Join(bad, "flow.yaml"), "ollama://gemma4:e4b", "anthropic://claude-sonnet-5-5")
	var rep struct {
		Valid  bool `json:"valid"`
		Issues []struct{ Rule, Code, Step, Message string }
	}
	m.okJSON(&rep, "flow", "validate", "-f", m.bundle)
	if !rep.Valid || len(rep.Issues) != 0 {
		t.Errorf("the bundle: %+v", rep)
	}
	if r := m.jsonOf(&rep, "flow", "validate", "-f", bad); r.Exit == 0 || rep.Valid || len(rep.Issues) != 1 ||
		rep.Issues[0].Rule != "V19" || rep.Issues[0].Code != "privacy_violation" || rep.Issues[0].Step != "brief" || !strings.Contains(rep.Issues[0].Message, "anthropic://claude-sonnet-5-5") {
		t.Errorf("a hosted model under local-only: exit %d, %+v", r.Exit, rep)
	}
	r := m.run("flow", "apply", "-f", bad, "--yes")
	if r.Exit != core.ExitPermission && r.Exit == 0 || !strings.Contains(r.Stderr, "privacy_violation") {
		t.Fatalf("apply of a hosted model under local-only: exit %d\n%s", r.Exit, r.Stderr)
	}
}

// TestMeetingBriefCommittedRecording: the recording in flows/meeting-brief/record
// replays, offline and with no fake on the PATH, to the same sections. With
// FLO_E2E_RECORD set it is rewritten from the fakes (loopback ports are
// normalised to ctxt's default so the files do not change from run to run).
func TestMeetingBriefCommittedRecording(t *testing.T) {
	src := filepath.Join(repoRoot, "flows", mbFlow, "record")
	if os.Getenv("FLO_E2E_RECORD") != "" {
		m := newMBEnv(t)
		m.record()
		dst := filepath.Join(m.bundle, "record")
		var normalise func(dir string)
		normalise = func(dir string) {
			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, ent := range ents {
				p := filepath.Join(dir, ent.Name())
				if ent.IsDir() {
					normalise(p)
					continue
				}
				b, err := os.ReadFile(p) //nolint:gosec // a file of the test's own temp bundle
				if err != nil {
					t.Fatal(err)
				}
				b = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`).ReplaceAll(b, []byte("127.0.0.1:8080"))
				if err := os.WriteFile(p, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		normalise(dst)
		if err := os.RemoveAll(src); err != nil {
			t.Fatal(err)
		}
		if out, err := cpR(dst, src); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}

	e := newEnv(t) // no shims, no stub: a call that is not answered from the recording fails
	e.writeConfig("packs:\n  ctxt:\n    url: http://127.0.0.1:8080\nai:\n  generator: template\n")
	for _, p := range []string{"ctxt", "heo", "tlc"} {
		e.installRepoPack(p)
	}
	for _, c := range []string{"knowledge.read", "mail.read", "tasks.read"} {
		e.ok("grant", "set", "automation", c, "--yes")
	}
	bundle := filepath.Join(t.TempDir(), mbFlow)
	if out, err := cpR(filepath.Join(repoRoot, "flows", mbFlow), bundle); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.ok("flow", "apply", "-f", bundle, "--yes")
	res, r := e.flowTest(mbFlow)
	if r.Exit != 0 || !res.Passed || res.Mode != flowtest.ModeReplay || res.Expected == 0 || res.Held != res.Expected {
		t.Fatalf("replay of the committed recording: exit %d, %+v\n%s", r.Exit, res, r.Stderr)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.ID+":"+s.Status)
	}
	if !slices.Contains(steps, "out:succeeded") {
		t.Errorf("steps %v", steps)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{ctxtToken, "Bearer", "/Users/", "/home/"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("the result holds %q", bad)
		}
	}
}
