//go:build !windows

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/flo/cmd"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/spec/node"
)

// The ctxt, heo and tlc packs of packs/ that feed the meeting brief, against
// a stub of ctxt's find endpoint, a fake heo and a fake tlc. Nothing reaches a
// real tool.

const ctxtToken = "stub-" + "token-for-tests"

func loadPackNodes(t *testing.T, name string) map[string]*node.Spec {
	t.Helper()
	lp, err := node.LoadPack(os.DirFS(filepath.Join(repoRoot, "packs")), name+"/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]*node.Spec{}
	for _, n := range lp.Nodes {
		byType[n.Node] = n
	}
	return byType
}

// TestBriefSourcePacksAreLocalReads pins what every brief source declares: a
// read, local, idempotent, with its own timeout and capability.
func TestBriefSourcePacksAreLocalReads(t *testing.T) {
	for _, c := range []struct{ pack, ref, capability string }{
		{"ctxt", "ctxt/find@1", "knowledge.read"},
		{"heo", "heo/conversation.list@1", "mail.read"},
		{"tlc", "tlc/task.list@1", "tasks.read"},
	} {
		n := loadPackNodes(t, c.pack)[c.ref]
		if n == nil {
			t.Fatalf("%s is not in the %s pack", c.ref, c.pack)
		}
		if n.SideEffect != node.SideEffectRead || n.Privacy != node.PrivacyLocal || n.Idempotent == nil || !*n.Idempotent || n.Timeout == "" {
			t.Errorf("%s: side_effect %q, privacy %q, idempotent %v, timeout %q", c.ref, n.SideEffect, n.Privacy, n.Idempotent, n.Timeout)
		}
		if !slices.Equal(n.Capabilities, []string{c.capability}) {
			t.Errorf("%s capabilities %v", c.ref, n.Capabilities)
		}
	}
	if x := loadPackNodes(t, "heo")["heo/conversation.list@1"].ExitCodes; x == nil || !slices.Equal(x.Retry, []int{70}) || !slices.Equal(x.OK, []int{0}) {
		t.Errorf("heo exit codes: %+v", x)
	}
	if a := loadPackNodes(t, "ctxt")["ctxt/find@1"].Request.Auth; a == nil || a.Secret != "ctxt_token" || a.Scheme != "bearer" {
		t.Errorf("ctxt auth: %+v", a)
	}
}

// ctxtStub answers POST /api/v1/find the way dpkms does: bearer required, the
// body decoded with unknown fields refused, the whole knowledge object in each
// result.
type ctxtStub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	auths    []string
	failures int // answer 503 this many times first
}

func newCtxtStub(t *testing.T) *ctxtStub {
	t.Helper()
	s := &ctxtStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/find" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+ctxtToken {
			http.Error(w, `{"error":{"code":"UNAUTHORIZED"}}`, http.StatusUnauthorized)
			return
		}
		var req struct {
			Query  string `json:"query"`
			Mode   string `json:"mode"`
			Limit  int    `json:"limit"`
			Filter struct {
				MetaType   string `json:"meta_type"`
				Topic      string `json:"topic"`
				Person     string `json:"person"`
				SourceType string `json:"source_type"`
				Since      string `json:"since"`
				Until      string `json:"until"`
			} `json:"filter"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || req.Query == "" {
			http.Error(w, `{"error":{"code":"INVALID_REQUEST"}}`, http.StatusBadRequest)
			return
		}
		var raw map[string]any
		b, _ := json.Marshal(req)
		_ = json.Unmarshal(b, &raw)
		s.requests = append(s.requests, raw)
		if s.failures > 0 {
			s.failures--
			http.Error(w, `{"error":{"code":"INTERNAL_ERROR"}}`, http.StatusServiceUnavailable)
			return
		}
		long := strings.Repeat("x", 700)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":` + jsonString(req.Query) + `,"mode":"hybrid","total":3,"diagnostics":{"semantic":{"status":"ok"}},"objects":[
		 {"id":"obj-1","type":"note","source":"manual","raw_content":"` + long + `","text_content":"full text","summaries":["Ada runs the platform team.","second"],
		  "metadata":{"people":["Ada Lovelace"]},"tags":[{"label":"people","weight":0.9},{"label":"platform"}],"created_at":"2026-10-01T09:00:00Z","updated_at":"2026-10-02T09:00:00Z","fts_indexed":true},
		 {"id":"obj-2","type":"url","text_content":"` + long + `","created_at":"2026-10-03T09:00:00Z","updated_at":"2026-10-03T09:00:00Z","fts_indexed":true},
		 {"id":"obj-3","type":"note","text_content":"short","created_at":"2026-10-04T09:00:00Z","updated_at":"2026-10-04T09:00:00Z","fts_indexed":true}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func (s *ctxtStub) calls() ([]map[string]any, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests), slices.Clone(s.auths)
}

// briefSourcesEnv is an env with the three packs installed, the fake heo and
// the heo wrapper on the PATH of the fakes, and ctxt's token stored for the
// flow's profile.
func briefSourcesEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	needJQ(t)
	e := newEnv(t, append([]string{
		"FLO_NOW=2026-11-18T12:00:00Z",
		// the secret ctxt_token of profile automation in tenant default
		"FLO_SECRET_DEFAULT_AUTOMATION_CTXT_TOKEN=" + ctxtToken,
	}, extra...)...)
	for _, p := range []string{"ctxt", "heo", "tlc"} {
		e.installRepoPack(p)
	}
	for _, dir := range []string{filepath.Join(repoRoot, "packs", "heo", "wrappers"), testdata("brief", "shims")} {
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

// briefEngine is the engine in this process, over the env's HOME, XDG tree,
// PATH of fakes and secrets: the shipped binary has no way yet to set
// packs.ctxt.url (kit's config is not read from a file or the environment), so
// the pack config goes in through serve.Config as the calendar test does.
type briefEngine struct {
	e   *env
	eng *serve.Engine
}

func (e *env) startBriefEngine(ctxtURL string) *briefEngine {
	e.t.Helper()
	for _, kv := range e.environ() {
		k, v, _ := strings.Cut(kv, "=")
		e.t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	eng, err := serve.Open(ctx, serve.Config{
		Owner: "jad", Interval: 20 * time.Millisecond, NoSync: true,
		ExprConfig: map[string]any{"ctxt": map[string]any{"url": ctxtURL}},
	})
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- eng.Serve(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			e.t.Errorf("engine serve: %v", err)
		}
		_ = eng.Close(context.Background())
	})
	return &briefEngine{e: e, eng: eng}
}

// flo runs one command of the real tree over the engine; --format json.
func (b *briefEngine) flo(args ...string) (string, error) {
	r := cmd.NewRootOver(func(context.Context) (*serve.Engine, error) { return b.eng, nil })
	var out, errOut bytes.Buffer
	r.Cmd.SetOut(&out)
	r.Cmd.SetErr(&errOut)
	r.SetArgs(append(args, "--format", "json"))
	if err := r.Execute(context.Background()); err != nil {
		return out.String(), &execError{err: err, stderr: errOut.String()}
	}
	return out.String(), nil
}

func (b *briefEngine) must(args ...string) string {
	b.e.t.Helper()
	out, err := b.flo(args...)
	if err != nil {
		b.e.t.Fatalf("flo %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// activate grants the sources' capabilities, applies the sources flow, records
// a test of it (the fakes it calls are cleared after) and activates it.
func (b *briefEngine) activate() {
	b.e.t.Helper()
	for _, c := range []string{"knowledge.read", "mail.read", "tasks.read"} {
		b.must("grant", "set", "automation", c, "--yes")
	}
	b.must("flow", "apply", "-f", copyBundle(b.e.t, "brief-sources"), "--yes")
	b.must("flow", "test", "brief-sources", "--record", "--allow-live", "--yes")
	b.e.resetScratch()
	b.must("flow", "set", "brief-sources", "--state", "active", "--yes")
}

// run starts the flow and waits for it.
func (b *briefEngine) run() (created, error) {
	var c created
	out, err := b.flo("run", "create", "--flow", "brief-sources", "--wait")
	_ = json.Unmarshal([]byte(out), &c)
	return c, err
}

func logOf(e *env, name string) []string { return readLines(e.t, filepath.Join(e.scratch, name)) }

func TestBriefSourcesFlow(t *testing.T) {
	stub := newCtxtStub(t)
	e := briefSourcesEnv(t)
	b := e.startBriefEngine(stub.URL)
	b.activate()
	reqs0, _ := stub.calls()

	c, err := b.run()
	if err != nil || c.Status != "succeeded" {
		t.Fatalf("run: %v %+v", err, c)
	}

	// ctxt: bearer from the secret, the body the server accepts, a projection of
	// each object (no raw content), the summary or the head of the text.
	reqs, auths := stub.calls()
	if len(reqs) != len(reqs0)+1 || auths[len(auths)-1] != "Bearer "+ctxtToken {
		t.Fatalf("ctxt calls %v, auth %v", reqs, auths)
	}
	last := reqs[len(reqs)-1]
	if last["query"] != "Ada Lovelace" || last["mode"] != "hybrid" || last["limit"] != float64(5) {
		t.Errorf("ctxt request: %v", last)
	}
	if f, _ := last["filter"].(map[string]any); f["person"] != "Ada Lovelace" {
		t.Errorf("ctxt filter: %v", last["filter"])
	}
	_, people := e.stepOf(c.Run, "people")
	objs, _ := people["objects"].([]any)
	if num(people["total"]) != 3 || len(objs) != 3 {
		t.Fatalf("people: %v", people)
	}
	o1, o2, o3 := objs[0].(map[string]any), objs[1].(map[string]any), objs[2].(map[string]any)
	if o1["summary"] != "Ada runs the platform team." || !slices.Equal(anyStrings(o1["tags"]), []string{"people", "platform"}) || o1["source"] != "manual" || o1["created_at"] != "2026-10-01T09:00:00Z" {
		t.Errorf("object 1: %v", o1)
	}
	if s, _ := o2["summary"].(string); len(s) != 600 || o2["source"] != "" || len(anyStrings(o2["tags"])) != 0 {
		t.Errorf("object 2 (no summary, long text, no tags): %v", o2)
	}
	if o3["summary"] != "short" {
		t.Errorf("object 3: %v", o3)
	}
	for _, o := range objs {
		for k := range o.(map[string]any) {
			if !slices.Contains([]string{"id", "type", "source", "created_at", "summary", "tags"}, k) {
				t.Errorf("the projection leaks %q", k)
			}
		}
	}

	// heo: the wrapper's matches, newest first, with the address that matched.
	_, threads := e.stepOf(c.Run, "threads")
	if got := threadIDs(threads); !slices.Equal(got, []string{"c1", "c3", "c4"}) || num(threads["scanned"]) != 5 || threads["truncated"] != false {
		t.Errorf("threads: %v", threads)
	}
	heo := logOf(e, "heo.log")
	if !slices.Contains(heo, "heo conversation list --updated-since 2026-10-19T12:00:00Z --sort last-activity --snoozed include --limit 100 --format json --no-hints") {
		t.Errorf("heo calls: %v", heo)
	}

	// tlc: across all projects, open statuses, the query after --, a projection.
	rec, _ := e.stepOf(c.Run, "open")
	tasks, _ := rec.Output.([]any)
	if len(tasks) != 3 { // review, open statuses only: the DONE one and the unrelated one stay out
		t.Fatalf("open tasks: %v", rec.Output)
	}
	t12, t13 := tasks[0].(map[string]any), tasks[1].(map[string]any)
	if t12["id"] != "T-0101" || t12["status"] != "IN_PROGRESS" || t12["priority"] != "P1" || t12["due_at"] != "2026-11-20T00:00:00Z" || len(anyStrings(t12["tags"])) != 1 || len(t12) != 6 {
		t.Errorf("task 1: %v", t12)
	}
	if t13["id"] != "T-0102" || t13["due_at"] != nil || len(anyStrings(t13["tags"])) != 0 {
		t.Errorf("task 2 (no due date, null tags): %v", t13)
	}
	if want := "tlc task list --all-projects --status TODO,IN_PROGRESS --limit 25 --format json -- review"; !slices.Contains(logOf(e, "tlc.log"), want) {
		t.Errorf("tlc calls: %v\nwant %q", logOf(e, "tlc.log"), want)
	}
}

func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func threadIDs(out map[string]any) []string {
	var ids []string
	l, _ := out["conversations"].([]any)
	for _, c := range l {
		ids = append(ids, c.(map[string]any)["id"].(string))
	}
	return ids
}

// TestBriefSourcesRetries: heo's exit 70 (no daemon) and a 5xx of ctxt are
// transient; the steps run again and the run succeeds.
func TestBriefSourcesRetries(t *testing.T) {
	stub := newCtxtStub(t)
	e := briefSourcesEnv(t)
	b := e.startBriefEngine(stub.URL)
	b.activate()
	stub.mu.Lock()
	stub.failures = 1
	stub.mu.Unlock()
	if err := os.WriteFile(filepath.Join(e.scratch, "heo-down"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() { // the daemon comes up after heo's first refusal
		for i := 0; i < 400; i++ {
			if slices.ContainsFunc(logOf(e, "heo.log"), func(l string) bool { return strings.HasPrefix(l, "heo identity list") }) {
				_ = os.Remove(filepath.Join(e.scratch, "heo-down"))
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	if c, err := b.run(); err != nil || c.Status != "succeeded" {
		t.Fatalf("run: %v %+v", err, c)
	}
	n := 0
	for _, l := range logOf(e, "heo.log") {
		if strings.HasPrefix(l, "heo identity list") {
			n++
		}
	}
	if n < 2 {
		t.Errorf("heo exit 70 was not retried: %v", logOf(e, "heo.log"))
	}
	if reqs, _ := stub.calls(); len(reqs) < 3 { // the recording's call, the 503, the retry
		t.Errorf("the 503 of ctxt was not retried: %d requests", len(reqs))
	}
}

// The heo wrapper on its own, as a node runs it minus the engine.
func TestHeoConversationsWrapper(t *testing.T) {
	e := briefSourcesEnv(t)
	run := func(args ...string) wrapRun {
		e.resetScratch()
		return wrap(t, e, "", "heo-conversations", args...)
	}
	ids := func(r wrapRun) []string {
		var out []string
		for _, c := range r.list("conversations") {
			out = append(out, c["id"].(string))
		}
		return out
	}

	// Addresses match case-insensitively on the identity's source id or any
	// attribute; a participant missing from the listing is looked up; both
	// listings are followed to their last page.
	r := run("--since", "30d", "--", "ADA@example.org", "bob@example.org")
	if r.exit != 0 || !slices.Equal(ids(r), []string{"c1", "c3", "c4"}) || r.out["scanned"] != float64(5) || r.out["matched_identities"] != float64(3) || r.out["truncated"] != false {
		t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
	}
	c3 := r.list("conversations")[1]
	if ps, _ := c3["participants"].([]any); len(ps) != 1 || ps[0].(map[string]any)["email"] != "bob@example.org" || ps[0].(map[string]any)["identity"] != "id-bob" {
		t.Errorf("c3 participants: %v", c3["participants"])
	}
	if c3["inbox"] != "ib1" || c3["status"] != "open" || c3["last_activity"] != "2026-11-16T10:00:00Z" {
		t.Errorf("c3: %v", c3)
	}
	calls := logOf(e, "heo.log")
	if len(calls) != 5 || !strings.HasPrefix(calls[1], "heo identity list --limit 100 --cursor i2") || !strings.Contains(calls[3], "--cursor c2") || !strings.HasPrefix(calls[4], "heo participant list --conversation c3") {
		t.Errorf("heo calls: %v", calls)
	}

	// since: a lookback becomes an RFC 3339 time; a time or a date goes as is.
	for since, want := range map[string]string{
		"2w": "2026-11-04T12:00:00Z", "36h": "2026-11-17T00:00:00Z", "90m": "2026-11-18T10:30:00Z",
		"2026-10-01": "2026-10-01", "2026-10-01T09:00:00Z": "2026-10-01T09:00:00Z",
	} {
		if r := run("--since", since, "--", "ada@example.org"); r.exit != 0 || !strings.Contains(strings.Join(logOf(e, "heo.log"), "\n"), "--updated-since "+want+" ") {
			t.Errorf("--since %s: exit %d, calls %v", since, r.exit, logOf(e, "heo.log"))
		}
	}

	// status goes through; no match, no conversation read.
	run("--status", "open,pending", "--", "ada@example.org")
	if !strings.Contains(strings.Join(logOf(e, "heo.log"), "\n"), "--status open,pending") {
		t.Errorf("status: %v", logOf(e, "heo.log"))
	}
	if r := run("--", "nobody@example.org"); r.exit != 0 || len(ids(r)) != 0 || r.out["matched_identities"] != float64(0) || r.out["scanned"] != float64(0) ||
		slices.ContainsFunc(logOf(e, "heo.log"), func(l string) bool { return strings.HasPrefix(l, "heo conversation") }) {
		t.Errorf("no match: %v, calls %v", r.out, logOf(e, "heo.log"))
	}

	// limit: stops reading once it has enough and says it stopped early.
	if r := run("--limit", "1", "--", "ada@example.org"); !slices.Equal(ids(r), []string{"c1"}) || r.out["truncated"] != true || r.out["scanned"] != float64(2) {
		t.Errorf("--limit 1: %v", r.out)
	}
	if r := run("--limit", "2", "--", "ada@example.org", "bob@example.org"); !slices.Equal(ids(r), []string{"c1", "c3"}) || r.out["truncated"] != true {
		t.Errorf("--limit 2: %v", r.out)
	}

	// heo's own failure is the wrapper's: 70 without a daemon, 3 for a
	// conversation it does not know; usage is 64.
	if err := os.WriteFile(filepath.Join(e.scratch, "heo-down"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := wrap(t, e, "", "heo-conversations", "--", "ada@example.org"); r.exit != 70 || !strings.Contains(r.err, "no daemon") || r.out != nil {
		t.Errorf("no daemon: exit %d %v\n%s", r.exit, r.out, r.err)
	}
	if r := wrap(t, e, "", "heo-conversations", "--since", "30d"); r.exit != 64 {
		t.Errorf("no address: exit %d", r.exit)
	}
}

// num reads a number of a step's output, whatever integer or float type it
// took on the way.
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}
