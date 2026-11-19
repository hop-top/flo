//go:build !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"hop.top/flo/internal/core"
)

// The judgment of inbox-triage: two classify steps over the headers of each
// message, then the script's decider policy applied by a wrapper. Everything
// here is synthetic mail (testdata/inbox) and a fake model; the real script,
// a real mailbox and a real model never take part.

const triageModelName = "qwen3:4b-instruct-2507-q4_K_M"

// triageModel is a fake ollama: /api/tags lists one model, /api/chat answers a
// choice question by the subject of the message in the prompt.
type triageModel struct {
	host string // 127.0.0.1:port
	mu   sync.Mutex
	asks []string // "label|<subject>" or "weigh|<subject>", in order of arrival
	gone bool     // the daemon lists no model
}

// hide makes the daemon list no model (it was never pulled, or is stopped).
// The asks are cleared: what a test counts after it is what the run asked.
func (m *triageModel) hide(gone bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gone, m.asks = gone, nil
}

// fakeTriageModel starts the fake daemon for the test.
func fakeTriageModel(t *testing.T) *triageModel {
	t.Helper()
	m := &triageModel{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		gone := m.gone
		m.mu.Unlock()
		models := []any{map[string]any{"name": triageModelName, "model": triageModelName}}
		if gone {
			models = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		prompt := req.Messages[len(req.Messages)-1].Content
		subject := triageSubjectOf(prompt)
		// the question is named label in both steps; the weigh one offers "irrelevant"
		var value string
		conf := 0.97
		if strings.Contains(prompt, "- irrelevant\n") {
			value = triageWeightOf(subject)
			m.record("weigh", subject)
		} else {
			value, conf = triageLabelOf(subject)
			m.record("label", subject)
		}
		answer, _ := json.Marshal(map[string]any{"label": map[string]any{"value": value, "confidence": conf, "why": "fake model: " + subject}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": triageModelName, "done": true, "done_reason": "stop",
			"message":           map[string]any{"role": "assistant", "content": string(answer)},
			"prompt_eval_count": 100, "eval_count": 20,
		})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	m.host = ln.Addr().String()
	return m
}

func (m *triageModel) record(kind, subject string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asks = append(m.asks, kind+"|"+subject)
}

// asked is what the model was asked, "label|<subject>" or "weigh|<subject>".
func (m *triageModel) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.asks)
}

func triageSubjectOf(prompt string) string {
	_, rest, ok := strings.Cut(prompt, "State:\n")
	if !ok {
		return ""
	}
	state, _, _ := strings.Cut(rest, "\n\nQuestions:")
	var item struct {
		Subject string `json:"subject"`
	}
	_ = json.Unmarshal([]byte(state), &item)
	return item.Subject
}

// triageLabelOf is the fake's filing label and its confidence.
func triageLabelOf(subject string) (string, float64) {
	switch {
	case strings.Contains(subject, "digest"):
		return "Subscriptions", 0.97
	case strings.Contains(subject, "Invoice"):
		return "Finance", 0.95
	case strings.Contains(subject, "Contract"):
		return "Legal", 0.6 // under the floor: Needs Review
	}
	return "Reference", 0.92
}

func triageWeightOf(subject string) string {
	switch {
	case strings.Contains(subject, "digest"):
		return "irrelevant"
	case strings.Contains(subject, "Invoice"), strings.Contains(subject, "Contract"):
		return "high"
	}
	return "normal"
}

// bundle is a copy of the shipped flow with the model pointed at the fake
// daemon and no recording (a recording is bound to the exact bundle).
func (m *triageModel) bundle(t *testing.T) string {
	t.Helper()
	dst := triageBundle(t)
	if err := os.RemoveAll(filepath.Join(dst, "record")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dst, "flow.yaml")
	b, err := os.ReadFile(p) //nolint:gosec // a copy of a file of this repository
	if err != nil {
		t.Fatal(err)
	}
	const real = "ollama://" + triageModelName
	if strings.Count(string(b), real) != 2 {
		t.Fatalf("flow.yaml names the model %d times, want 2 (label and weigh)", strings.Count(string(b), real))
	}
	out := strings.ReplaceAll(string(b), real, "ollama://"+m.host+"/"+triageModelName)
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil { //nolint:gosec // same copy
		t.Fatal(err)
	}
	return dst
}

// snapshotDir writes the fake script's snapshot of the default scenario into a
// run folder and returns it: what inbox-rows and inbox-decisions read.
func snapshotDir(t *testing.T) string {
	t.Helper()
	rows, err := os.ReadFile(testdata("inbox", "rows.json")) //nolint:gosec // a fixture of this repository
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	snap := fmt.Sprintf(`{"run": "20261118-090001", "rows": %s}`, rows)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(snap), 0o600); err != nil { //nolint:gosec // a temp dir
		t.Fatal(err)
	}
	return dir
}

type decisionsFile struct {
	Decisions []struct {
		N       int    `json:"n"`
		Actions []any  `json:"actions"`
		Why     string `json:"why"`
	} `json:"decisions"`
}

// TestInboxRowsAndDecisions: the wrappers of the judgment, over synthetic mail.
func TestInboxRowsAndDecisions(t *testing.T) {
	e := triageEnv(t)
	dir := snapshotDir(t)
	out := filepath.Join(t.TempDir(), "rows.json")

	// rows: headers only, one per message, no body field
	if r := e.runShim("", "inbox-rows", "--out", out, dir); r.Exit != 0 {
		t.Fatalf("inbox-rows: %+v", r)
	}
	var rows struct {
		Decided bool             `json:"decided"`
		Items   []map[string]any `json:"items"`
	}
	b, _ := os.ReadFile(out) //nolint:gosec // a temp file
	if err := json.Unmarshal(b, &rows); err != nil || rows.Decided || len(rows.Items) != 4 {
		t.Fatalf("rows: %v %s", err, b)
	}
	for _, it := range rows.Items {
		if _, has := it["body"]; has {
			t.Errorf("a body in the rows: %v", it)
		}
	}
	// carried rows (decided in an earlier run) and rows answered by hand are not asked again
	if err := os.WriteFile(filepath.Join(dir, "carried.json"), []byte(`{"decisions": [{"n": 2, "actions": ["archive"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.runShim("", "inbox-rows", "--out", out, dir); r.Exit != 0 {
		t.Fatalf("inbox-rows with carried: %+v", r)
	}
	b, _ = os.ReadFile(out) //nolint:gosec // a temp file
	_ = json.Unmarshal(b, &rows)
	if len(rows.Items) != 3 || rows.Items[1]["n"] != float64(3) {
		t.Errorf("carried row is still asked: %s", b)
	}
	_ = os.Remove(filepath.Join(dir, "carried.json"))

	// decisions: the script's policy over the verdicts
	item := func(n int, from string) map[string]any {
		return map[string]any{"n": n, "sender": map[string]any{"address": from}}
	}
	lab := func(n int, from, label string, conf float64) map[string]any {
		var l any = label
		if label == "" {
			l = nil
		}
		return map[string]any{"item": item(n, from), "label": l, "confidence": conf, "why": "w"}
	}
	wt := func(n int, from, label string) map[string]any {
		return map[string]any{"item": item(n, from), "label": label}
	}
	in := map[string]any{
		"labels": []any{
			lab(1, "news@example.org", "Subscriptions", 0.97), // role address, quiet: filed, read, archived
			lab(2, "billing@example.org", "Finance", 0.95),    // important: filed only
			lab(3, "sam@example.org", "Reference", 0.99),      // a person: never filed
			lab(4, "noreply@example.org", "Legal", 0.5),       // under the floor: Needs Review
			lab(5, "info@example.org", "", 0),                 // abstained: left for a person
			lab(6, "team@example.org", "Waiting", 0.99),       // held label: never archived
		},
		"weights": []any{
			wt(1, "news@example.org", "irrelevant"), wt(2, "billing@example.org", "high"), wt(3, "sam@example.org", "low"),
			wt(4, "noreply@example.org", "low"), wt(5, "info@example.org", "normal"), wt(6, "team@example.org", "low"),
		},
		"sop_labels":     []any{"Subscriptions", "Finance", "Legal", "Reference", "Waiting"},
		"hold_labels":    []any{"Action Required", "Waiting"},
		"quiet_weights":  []any{"irrelevant", "low"},
		"min_confidence": 0.9, "engine": "local", "model": "ollama://m",
	}
	body, _ := json.Marshal(in)
	sum := filepath.Join(t.TempDir(), "sum.json")
	if r := e.runShim(string(body), "inbox-decisions", "--out", sum, dir); r.Exit != 0 {
		t.Fatalf("inbox-decisions: %+v", r)
	}
	var d decisionsFile
	b, err := os.ReadFile(filepath.Join(dir, "decisions.json")) //nolint:gosec // a temp file
	if err != nil || json.Unmarshal(b, &d) != nil || len(d.Decisions) != 6 {
		t.Fatalf("decisions: %v %s", err, b)
	}
	want := map[int]string{
		1: `[{"label":"Subscriptions"},"read","archive"]`,
		2: `[{"label":"Finance"}]`,
		3: `[{"attention":"from a person, not ruled out: label Reference conf 0.99, importance low; left in INBOX"}]`,
		4: `[{"label":"Needs Review"}]`,
		5: `[{"attention":"importance normal; the judge could not settle routine handling, needs your judgment; left in INBOX"}]`,
		6: `[{"label":"Waiting"}]`,
	}
	for _, dec := range d.Decisions {
		got, _ := json.Marshal(dec.Actions)
		if string(got) != want[dec.N] {
			t.Errorf("decision %d: %s, want %s", dec.N, got, want[dec.N])
		}
		for _, a := range dec.Actions {
			if m, ok := a.(map[string]any); ok && (m["trash"] != nil || m["reply"] != nil || m["unsubscribe"] != nil) {
				t.Errorf("a destructive action from a judge: %v", dec)
			}
		}
	}
	var s map[string]any
	b, _ = os.ReadFile(sum) //nolint:gosec // a temp file
	_ = json.Unmarshal(b, &s)
	if s["asked"] != float64(6) || s["attention"] != float64(2) || s["needs_review"] != float64(1) || s["already_decided"] != false {
		t.Errorf("summary: %s", b)
	}

	// an existing decisions file is kept
	before, _ := os.ReadFile(filepath.Join(dir, "decisions.json")) //nolint:gosec // a temp file
	if r := e.runShim(`{"labels": [], "weights": []}`, "inbox-decisions", "--out", sum, dir); r.Exit != 0 {
		t.Fatalf("second inbox-decisions: %+v", r)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "decisions.json")) //nolint:gosec // a temp file
	if string(before) != string(after) {
		t.Error("decisions were rewritten")
	}
	b, _ = os.ReadFile(sum) //nolint:gosec // a temp file
	if !strings.Contains(string(b), `"already_decided": true`) {
		t.Errorf("summary of a kept file: %s", b)
	}
	// bad input is a usage error, nothing is written
	if r := e.runShim("not json", "inbox-decisions", "--out", sum, snapshotDir(t)); r.Exit == 0 {
		t.Error("garbage on stdin was accepted")
	}
}

// TestInboxDecisionsUseSenderFacts: the guard "mail from a person is never
// filed away" follows the script's facts (contact, muted, ruled out, person);
// only a row without facts falls back to the address. Synthetic senders.
func TestInboxDecisionsUseSenderFacts(t *testing.T) {
	e := triageEnv(t)
	dir := snapshotDir(t)
	item := func(n int, from string) map[string]any {
		return map[string]any{"n": n, "sender": map[string]any{"address": from}}
	}
	lab := func(n int, from string) map[string]any {
		return map[string]any{"item": item(n, from), "label": "Reference", "confidence": 0.99, "why": "w"}
	}
	wt := func(n int, from string) map[string]any {
		return map[string]any{"item": item(n, from), "label": "irrelevant"}
	}
	facts := func(n int, f map[string]any) map[string]any {
		f["n"], f["in_store"] = n, true
		return f
	}
	from := map[int]string{
		1: "role@example.org",    // role address, but a contact: guarded
		2: "pat@example.org",     // person-looking and muted: filed
		3: "sam@example.org",     // person-looking and ruled out: filed
		4: "alex@example.org",    // person: guarded
		5: "robot@example.org",   // unknown, not a person: filed
		6: "kim@example.org",     // no facts, person-looking address: guarded
		7: "noreply@example.org", // no facts, role address: filed
		8: "lee@example.org",     // facts say not a person (bulk headers), person-looking address: filed
	}
	var labels, weights []any
	for n := 1; n <= 8; n++ {
		labels, weights = append(labels, lab(n, from[n])), append(weights, wt(n, from[n]))
	}
	senders := []any{
		facts(1, map[string]any{"is_contact": true, "person": false, "muted": false, "ruled_out": false}),
		facts(2, map[string]any{"is_contact": false, "person": true, "muted": true, "ruled_out": false}),
		facts(3, map[string]any{"is_contact": false, "person": true, "muted": false, "ruled_out": true}),
		facts(4, map[string]any{"is_contact": false, "person": true, "muted": false, "ruled_out": false}),
		facts(5, map[string]any{"is_contact": false, "person": false, "muted": false, "ruled_out": false}),
		map[string]any{"n": 6, "in_store": false},
		map[string]any{"n": 7, "in_store": false},
		facts(8, map[string]any{"is_contact": false, "person": false, "muted": false, "ruled_out": false}),
	}
	body, _ := json.Marshal(map[string]any{
		"labels": labels, "weights": weights, "senders": senders,
		"sop_labels": []any{"Reference"}, "quiet_weights": []any{"irrelevant"},
		"min_confidence": 0.9, "engine": "local",
	})
	sum := filepath.Join(t.TempDir(), "sum.json")
	if r := e.runShim(string(body), "inbox-decisions", "--out", sum, dir); r.Exit != 0 {
		t.Fatalf("inbox-decisions: %+v", r)
	}
	var d decisionsFile
	b, err := os.ReadFile(filepath.Join(dir, "decisions.json")) //nolint:gosec // a temp file
	if err != nil || json.Unmarshal(b, &d) != nil || len(d.Decisions) != 8 {
		t.Fatalf("decisions: %v %s", err, b)
	}
	guarded := map[int]bool{1: true, 4: true, 6: true}
	for _, dec := range d.Decisions {
		got, _ := json.Marshal(dec.Actions)
		held := strings.HasPrefix(string(got), `[{"attention":"from a person, not ruled out:`)
		if held != guarded[dec.N] {
			t.Errorf("decision %d (%s): %s, guarded want %v", dec.N, from[dec.N], got, guarded[dec.N])
		}
		if !guarded[dec.N] && string(got) != `[{"label":"Reference"},"read","archive"]` {
			t.Errorf("decision %d is not filed by the normal policy: %s", dec.N, got)
		}
	}
}

// TestInboxDecisionsAuthAndVendors: auth mail (login code, reset, magic link) is
// unrequested: labelled and archived only for a customer vendor whose mail
// authenticates, otherwise kept in the inbox, flagged; mail claiming a customer
// vendor that fails authentication is flagged; a customer vendor's mail is never
// deleted. Without a security label every auth mail stays. Synthetic senders.
func TestInboxDecisionsAuthAndVendors(t *testing.T) {
	e := triageEnv(t)
	item := func(n int) map[string]any {
		return map[string]any{"n": n, "sender": map[string]any{"address": fmt.Sprintf("s%d@example.org", n)}}
	}
	facts := func(n int, f map[string]any) map[string]any {
		f["n"], f["in_store"] = n, true
		return f
	}
	senders := []any{
		facts(1, map[string]any{"auth_class": "reset", "auth_route": "archive_security", "customer_vendor": true, "auth": "pass"}),
		facts(2, map[string]any{"auth_class": "code", "auth_route": "keep_flagged", "customer_vendor": true, "auth": "fail"}),
		facts(3, map[string]any{"auth_class": "link", "auth_route": "keep_flagged", "customer_vendor": false, "auth": "pass"}),
		facts(4, map[string]any{"customer_vendor": true, "auth": "fail", "spoof_suspect": true}),
		facts(5, map[string]any{"customer_vendor": true, "auth": "pass"}),
		facts(6, map[string]any{"auth_class": "code", "auth_route": "archive_security", "customer_vendor": true, "auth": "pass"}),
	}
	var labels, weights []any
	for n := 1; n <= 6; n++ {
		labels = append(labels, map[string]any{"item": item(n), "label": "Reference", "confidence": 0.99, "why": "w"})
		weights = append(weights, map[string]any{"item": item(n), "label": "irrelevant"})
	}
	run := func(secLabel any) map[int]string {
		dir := snapshotDir(t)
		body, _ := json.Marshal(map[string]any{
			"labels": labels, "weights": weights, "senders": senders, "security_label": secLabel,
			"sop_labels": []any{"Reference"}, "quiet_weights": []any{"irrelevant"}, "min_confidence": 0.9, "engine": "local",
		})
		if r := e.runShim(string(body), "inbox-decisions", "--out", filepath.Join(t.TempDir(), "sum.json"), dir); r.Exit != 0 {
			t.Fatalf("inbox-decisions: %+v", r)
		}
		var d decisionsFile
		b, err := os.ReadFile(filepath.Join(dir, "decisions.json")) //nolint:gosec // a temp file
		if err != nil || json.Unmarshal(b, &d) != nil || len(d.Decisions) != 6 {
			t.Fatalf("decisions: %v %s", err, b)
		}
		got := map[int]string{}
		for _, dec := range d.Decisions {
			a, _ := json.Marshal(dec.Actions)
			got[dec.N] = string(a)
			if (dec.N >= 4) && (strings.Contains(string(a), `"trash"`) || strings.Contains(string(a), `"spam"`)) {
				t.Errorf("decision %d: a customer vendor's mail has a delete action: %s", dec.N, a)
			}
		}
		return got
	}
	got := run("security")
	if got[1] != `[{"label":"security"},"archive"]` || got[6] != `[{"label":"security"},"archive"]` {
		t.Errorf("authenticated customer vendor: want security + archive: %s / %s", got[1], got[6])
	}
	for n, want := range map[int]string{2: "does not authenticate", 3: "not a customer vendor", 4: "spoof-suspect"} {
		if !strings.HasPrefix(got[n], `[{"attention":`) || !strings.Contains(got[n], want) || strings.Contains(got[n], "archive") {
			t.Errorf("decision %d: want kept in the inbox, flagged (%s): %s", n, want, got[n])
		}
	}
	if got[5] != `[{"label":"Reference"},"read","archive"]` {
		t.Errorf("an ordinary customer-vendor mail follows the normal policy: %s", got[5])
	}
	for n, g := range run(nil) {
		if (n == 1 || n == 6) && (!strings.HasPrefix(g, `[{"attention":`) || !strings.Contains(g, "no security label")) {
			t.Errorf("decision %d without a security label stays in the inbox, flagged: %s", n, g)
		}
	}
}

// TestInboxTriageJudgesLocally: the run asks the model two questions per
// message, over headers only, and the decisions the script plans from are the
// script's policy over the answers.
func TestInboxTriageJudgesLocally(t *testing.T) {
	e := triageEnv(t)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()
	c, _ := e.startTriage()

	asked := e.model.asked()
	// the recording made one pass; the run itself made another: count the second
	if len(asked) < 8 {
		t.Fatalf("the model was asked %d times: %v", len(asked), asked)
	}
	for _, subject := range []string{"Weekly digest", "Invoice 2291", "Lunch on Friday?", "Contract renewal"} {
		for _, kind := range []string{"label", "weigh"} {
			if !slices.Contains(asked, kind+"|"+subject) {
				t.Errorf("%s was not asked about %q", kind, subject)
			}
		}
	}
	_, lab := e.stepOf(c.Run, "label")
	if lab["engine"] != "local" || !strings.Contains(fmt.Sprint(lab["model"]), triageModelName) {
		t.Errorf("label output: engine %v, model %v", lab["engine"], lab["model"])
	}
	if strings.Contains(fmt.Sprint(lab), "openrouter") || strings.Contains(fmt.Sprint(lab["model"]), "jev") {
		t.Errorf("a cloud judge answered: %v", lab)
	}
	_, dec := e.stepOf(c.Run, "decisions")
	if fmt.Sprint(dec["asked"]) != "4" || dec["engine"] != "local" || dec["already_decided"] != false {
		t.Errorf("decisions output: %v", dec)
	}

	// the decisions file the script plans from
	var d decisionsFile
	b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "runs", "20261118-090001", "decisions.json")) //nolint:gosec // the fake's state
	if err != nil || json.Unmarshal(b, &d) != nil || len(d.Decisions) != 4 {
		t.Fatalf("decisions file: %v %s", err, b)
	}
	got := map[int]string{}
	for _, dec := range d.Decisions {
		a, _ := json.Marshal(dec.Actions)
		got[dec.N] = string(a)
		if !strings.HasPrefix(dec.Why, "Judge (local): ") {
			t.Errorf("decision %d why: %s", dec.N, dec.Why)
		}
	}
	if got[1] != `[{"label":"Subscriptions"},"read","archive"]` || got[2] != `[{"label":"Finance"}]` {
		t.Errorf("filing decisions: %v", got)
	}
	// a message from a person (sam, legal: no role address) is left in the inbox
	for _, n := range []int{3, 4} {
		if !strings.HasPrefix(got[n], `[{"attention":`) {
			t.Errorf("message %d was filed away: %s", n, got[n])
		}
	}
}

// TestInboxTriageGuardFollowsScriptFacts: the flow asks the script who the
// senders are and the person guard follows its answer. Message 1 (a role
// address) is a contact, so it is left in the inbox; message 3 (person-looking)
// is muted, so the normal policy files it; message 4 is a person.
func TestInboxTriageGuardFollowsScriptFacts(t *testing.T) {
	e := triageEnv(t)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()
	e.inboxFlag("sender-contact-m1", true)
	e.inboxFlag("sender-muted-m3", true)
	c, _ := e.startTriage()

	calls := e.inboxCalls("senders")
	if len(calls) != 1 || !strings.Contains(calls[0], "--json") || !strings.Contains(calls[0], "20261118-090") {
		t.Errorf("senders calls: %v", calls)
	}
	_, out := e.stepOf(c.Run, "senders")
	if fmt.Sprint(out["count"]) != "4" || fmt.Sprint(out["missing"]) != "0" {
		t.Errorf("senders output: %v", out)
	}
	var d decisionsFile
	b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "runs", "20261118-090001", "decisions.json")) //nolint:gosec // the fake's state
	if err != nil || json.Unmarshal(b, &d) != nil || len(d.Decisions) != 4 {
		t.Fatalf("decisions file: %v %s", err, b)
	}
	got := map[int]string{}
	for _, dec := range d.Decisions {
		a, _ := json.Marshal(dec.Actions)
		got[dec.N] = string(a)
	}
	for _, n := range []int{1, 4} {
		if !strings.HasPrefix(got[n], `[{"attention":"from a person, not ruled out:`) {
			t.Errorf("message %d was filed away: %s", n, got[n])
		}
	}
	if got[3] != `[{"label":"Reference"}]` || got[2] != `[{"label":"Finance"}]` {
		t.Errorf("muted and unknown senders follow the normal policy: %v", got)
	}
}

// TestInboxTriageWithoutFactsFallsBackToTheAddress: a message the script's
// store does not hold has no facts; the address decides, towards the inbox.
func TestInboxTriageWithoutFactsFallsBackToTheAddress(t *testing.T) {
	e := triageEnv(t)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()
	e.inboxFlag("sender-nofacts", true)
	c, _ := e.startTriage()
	_, out := e.stepOf(c.Run, "senders")
	if fmt.Sprint(out["missing"]) != "4" {
		t.Errorf("senders output: %v", out)
	}
	var d decisionsFile
	b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "runs", "20261118-090001", "decisions.json")) //nolint:gosec // the fake's state
	if err != nil || json.Unmarshal(b, &d) != nil || len(d.Decisions) != 4 {
		t.Fatalf("decisions file: %v %s", err, b)
	}
	for _, dec := range d.Decisions {
		a, _ := json.Marshal(dec.Actions)
		held := strings.HasPrefix(string(a), `[{"attention":`)
		if held != (dec.N == 3 || dec.N == 4) {
			t.Errorf("message %d: %s", dec.N, a)
		}
	}
}

// TestInboxTriageNeverFallsToJev: mail is private. When the local model is
// gone the label step fails (privacy_violation: Jev follows it in the list, the
// local-only ceiling refuses it); nothing is decided, planned or applied, and
// no request goes anywhere else.
func TestInboxTriageNeverFallsToJev(t *testing.T) {
	e := triageEnv(t)
	e.triageActive()
	eng := e.startEngine()
	defer eng.stop()
	e.model.hide(true) // the daemon no longer lists the model
	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait"); r.Exit == 0 || c.Status != "failed" {
		t.Fatalf("run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	v := e.show(c.Run)
	if st, _ := v.step("label"); st != "failed" {
		t.Errorf("label is %s", st)
	}
	rec, _ := e.stepOf(c.Run, "label")
	if rec.Failure == nil || rec.Failure.Code != core.CodePrivacyViolation || !strings.Contains(rec.Failure.Message, "jev") {
		t.Errorf("label failure: %+v", rec.Failure)
	}
	for _, s := range []string{"decisions", "plan", "approve", "apply"} {
		if st, _ := v.step(s); st != "skipped" {
			t.Errorf("%s is %s", s, st)
		}
	}
	if n := len(e.inboxCalls("plan")) + len(e.inboxCalls("apply")); n != 0 || len(e.applied()) != 0 {
		t.Errorf("work went on without a judge: %v", e.inboxLog())
	}
	if got := e.model.asked(); len(got) != 0 {
		t.Errorf("the model answered while hidden: %v", got)
	}
}
