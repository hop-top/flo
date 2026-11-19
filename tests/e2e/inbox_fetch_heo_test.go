//go:build !windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// heo/inbox.list@1 and inbox/ingest@1: an account's inbox listed through heo
// (read only) and snapshotted by the script's envelope seam. The wrapper runs
// for real; heo is a fake over testdata/inbox/heo (testdata/inbox/heoshims),
// the script a fake (testdata/inbox/shims/inbox). No daemon, no account, no
// token.

// heoFetchEnv is an env with the heo and inbox packs installed, the heo
// wrapper, the inbox wrappers and both fakes on the PATH of the fakes.
func heoFetchEnv(t *testing.T) *env {
	t.Helper()
	needJQ(t)
	e := newEnv(t)
	e.installRepoPack("inbox")
	e.installRepoPack("heo")
	for _, dir := range []string{
		filepath.Join(repoRoot, "packs", "inbox", "wrappers"), testdata("inbox", "shims"),
		filepath.Join(repoRoot, "packs", "heo", "wrappers"), testdata("inbox", "heoshims"),
	} {
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

func (e *env) scratchFlag(name string, on bool) {
	e.t.Helper()
	p := filepath.Join(e.scratch, name)
	if !on {
		_ = os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func TestInboxIngestNodesDeclareHonestEffects(t *testing.T) {
	list := loadPackNodes(t, "heo")["heo/inbox.list@1"]
	if list == nil {
		t.Fatal("heo/inbox.list@1 is not in the heo pack")
	}
	if list.SideEffect != node.SideEffectRead || list.Privacy != node.PrivacyLocal || list.Idempotent == nil || !*list.Idempotent ||
		!slices.Equal(list.Capabilities, []string{"mail.read"}) || list.Timeout == "" {
		t.Errorf("heo/inbox.list@1 is a local, idempotent mail.read: %+v", list)
	}
	if list.ExitCodes == nil || !slices.Contains(list.ExitCodes.Retry, 70) || !slices.Contains(list.ExitCodes.OK, 3) {
		t.Errorf("heo/inbox.list@1 exit codes (3 is a failed account, 70 is retried): %+v", list.ExitCodes)
	}
	ing := loadPackNodes(t, "inbox")["inbox/ingest@1"]
	if ing == nil {
		t.Fatal("inbox/ingest@1 is not in the inbox pack")
	}
	if ing.SideEffect != node.SideEffectWriteLocal || ing.Privacy != node.PrivacyLocal || ing.Idempotent == nil || !*ing.Idempotent ||
		!slices.Equal(ing.Capabilities, []string{"mail.read"}) {
		t.Errorf("inbox/ingest@1 writes the run folder and store only: %+v", ing)
	}
	if ing.ExitCodes == nil || !slices.Equal(ing.ExitCodes.OK, []int{0, 3}) {
		t.Errorf("inbox/ingest@1 exit codes: %+v", ing.ExitCodes)
	}
	// the legacy node stays beside it
	if loadPackNodes(t, "inbox")["inbox/fetch@1"] == nil {
		t.Error("inbox/fetch@1 must stay for accounts that are not on heo")
	}
}

type envelope struct {
	ID           string `json:"id"`
	MessageID    string `json:"message-id"`
	Date         string `json:"date"`
	Subject      string `json:"subject"`
	InReplyTo    string `json:"in-reply-to"`
	HasAttach    bool   `json:"has-attachment"`
	Conversation string `json:"conversation"`
	From         struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"from"`
	To []struct {
		Email string `json:"email"`
	} `json:"to"`
	Flags []struct {
		Iana string `json:"iana"`
	} `json:"flags"`
}

func (r wrapRun) envelopes(acct string) []envelope {
	r.t.Helper()
	accts, _ := r.out["accounts"].(map[string]any)
	a, _ := accts[acct].(map[string]any)
	b, _ := json.Marshal(a["envelopes"])
	var out []envelope
	if err := json.Unmarshal(b, &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// TestHeoInboxListWrapper: the wrapper on its own, as the node runs it minus
// the engine.
func TestHeoInboxListWrapper(t *testing.T) {
	e := heoFetchEnv(t)
	run := func(args ...string) wrapRun {
		e.resetScratch()
		return wrap(t, e, "", "heo-inbox-list", args...)
	}

	// Public mail from others in a conversation still in the inbox, newest
	// first; both listings are followed to their last page.
	r := run("--account", "acme", "--inbox", "ib-acme", "--email", "me@acme.example")
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.err)
	}
	envs := r.envelopes("acme")
	var ids []string
	for _, x := range envs {
		ids = append(ids, x.ID)
	}
	if !slices.Equal(ids, []string{"msg_3", "msg_1"}) {
		t.Fatalf("ids %v: archived, trashed, own, desk, non-public and orphan messages are out", ids)
	}
	m1, m3 := envs[1], envs[0]
	if m1.MessageID != "<a1@vendor.example>" || m1.Subject != "Invoice 42" || m1.From.Name != "Vendor Billing" || m1.From.Email != "billing@vendor.example" ||
		m1.Date != "2026-11-18T08:00:00Z" || m1.InReplyTo != "<z0@acme.example>" || m1.Conversation != "c1" || m1.HasAttach {
		t.Errorf("msg_1: %+v", m1)
	}
	if len(m1.To) != 1 || m1.To[0].Email != "me@acme.example" {
		t.Errorf("msg_1 To is the account address: %+v", m1.To)
	}
	if len(m1.Flags) != 0 {
		t.Errorf("msg_1 is in an unread, unstarred conversation: %+v", m1.Flags)
	}
	// metadata read from a structured part; read + starred conversation; an attachment part
	if m3.MessageID != "<a3@client.example>" || m3.Subject != "Contract draft" || m3.From.Name != "Pat" || !m3.HasAttach {
		t.Errorf("msg_3: %+v", m3)
	}
	if len(m3.Flags) != 2 || m3.Flags[0].Iana != "seen" || m3.Flags[1].Iana != "flagged" {
		t.Errorf("msg_3 flags: %+v", m3.Flags)
	}

	// Read only: two pages of each listing, the inbox named, nothing else asked.
	calls := logOf(e, "heo.log")
	want := []string{
		"heo conversation list --inbox ib-acme --limit 100 --format json --no-hints",
		"heo conversation list --inbox ib-acme --limit 100 --cursor cur2 --format json --no-hints",
		"heo message list --inbox ib-acme --limit 100 --format json --no-hints",
		"heo message list --inbox ib-acme --limit 100 --cursor cur2 --format json --no-hints",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("heo calls:\n%v\nwant\n%v", calls, want)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "heo conversation list ") && !strings.HasPrefix(c, "heo message list ") {
			t.Errorf("a call that is not a listing: %s", c)
		}
	}
	for _, s := range []string{"Invoice 42", "Contract draft", "billing@vendor.example"} {
		if strings.Contains(r.err, s) {
			t.Errorf("stderr carries mail content %q: %s", s, r.err)
		}
	}

	// --limit keeps the newest; no --email leaves To out; --inbox defaults to the account.
	r = run("--account", "ib-acme", "--limit", "1")
	if got := r.envelopes("ib-acme"); r.exit != 0 || len(got) != 1 || got[0].ID != "msg_3" || len(got[0].To) != 0 {
		t.Errorf("limit 1, no email: exit %d %+v", r.exit, got)
	}

	// heo's daemon is down: its own exit, the node retries it; no file.
	e.scratchFlag("heo-down", true)
	if r := wrap(t, e, "", "heo-inbox-list", "--account", "acme", "--inbox", "ib-acme"); r.exit != 70 || r.out != nil {
		t.Errorf("daemon down: exit %d out %v", r.exit, r.out)
	}
	e.scratchFlag("heo-down", false)

	// heo refuses: the account fails as data (exit 3), with heo's code and message and no mail content.
	e.scratchFlag("heo-fail", true)
	r = wrap(t, e, "", "heo-inbox-list", "--account", "acme", "--inbox", "ib-acme")
	accts, _ := r.out["accounts"].(map[string]any)
	acme, _ := accts["acme"].(map[string]any)
	msg, _ := acme["error"].(string)
	if r.exit != 3 || acme["envelopes"] != nil || !strings.Contains(msg, "CONFLICT") || !strings.Contains(msg, "inbox is being rebuilt") {
		t.Errorf("heo refused: exit %d %v", r.exit, r.out)
	}
	e.scratchFlag("heo-fail", false)

	// an unknown inbox is the same: that account's error
	r = wrap(t, e, "", "heo-inbox-list", "--account", "acme", "--inbox", "ib-nope")
	if accts, _ := r.out["accounts"].(map[string]any); r.exit != 3 || accts["acme"].(map[string]any)["error"] == nil {
		t.Errorf("unknown inbox: exit %d %v", r.exit, r.out)
	}

	// usage
	if r := wrap(t, e, "", "heo-inbox-list", "--inbox", "ib-acme"); r.exit != 64 {
		t.Errorf("no --account: exit %d", r.exit)
	}
	if r := wrap(t, e, "", "heo-inbox-list", "--account", "acme", "--limit", "x"); r.exit != 64 {
		t.Errorf("bad --limit: exit %d", r.exit)
	}
}

// TestInboxFetchThroughHeo: heo/inbox.list@1 then inbox/ingest@1 in a flow,
// through the engine: what the fake script receives on stdin is what heo
// listed, the run is the script's, and nothing but listings reached heo.
func TestInboxFetchThroughHeo(t *testing.T) {
	e := heoFetchEnv(t)
	e.ok("grant", "set", "automation", "mail.read", "--yes")
	e.applyTested("inbox-fetch-heo")
	e.ok("flow", "set", "inbox-fetch-heo", "--state", "active", "--yes")
	eng := e.startEngine()
	defer eng.stop()

	var c created
	if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-fetch-heo", "--wait"); r.Exit != 0 || c.Status != "succeeded" {
		t.Fatalf("run: exit %d %+v\n%s", r.Exit, c, r.Stderr)
	}
	_, list := e.stepOf(c.Run, "list")
	_, ing := e.stepOf(c.Run, "ingest")
	if num(ing["rows"]) != 2 || num(ing["new"]) != 2 || ing["run"] == "" {
		t.Fatalf("ingest: %v", ing)
	}
	accts, _ := ing["accounts"].(map[string]any)
	if acme, _ := accts["acme"].(map[string]any); acme["status"] != "ok" || num(acme["count"]) != 2 {
		t.Errorf("accounts: %v", accts)
	}
	if lines := e.inboxCalls("fetch"); len(lines) != 1 || lines[0] != "fetch --json --envelopes - --account acme" {
		t.Errorf("the script was called %v: the seam is `fetch --json --envelopes - --account acme`", lines)
	}
	// what the script read on stdin is what heo/inbox.list@1 produced
	b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "envelopes.json")) //nolint:gosec // the fake's state
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	lb, _ := json.Marshal(map[string]any{"accounts": list["accounts"]})
	if err := json.Unmarshal(lb, &want); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, want) {
		t.Errorf("stdin of the script:\n%s\nlisted by heo:\n%s", b, lb)
	}
	var ids []string
	for _, x := range got["accounts"].(map[string]any)["acme"].(map[string]any)["envelopes"].([]any) {
		ids = append(ids, x.(map[string]any)["id"].(string))
	}
	if !slices.Equal(ids, []string{"msg_3", "msg_1"}) {
		t.Errorf("envelope ids %v", ids)
	}
	for _, l := range logOf(e, "heo.log") {
		if !strings.HasPrefix(l, "heo conversation list ") && !strings.HasPrefix(l, "heo message list ") {
			t.Errorf("heo was asked for more than a listing: %s", l)
		}
	}
	if len(logOf(e, "heo.log")) != 4 {
		t.Errorf("heo calls: %v", logOf(e, "heo.log"))
	}

	// heo refuses the inbox: the account is an error in the run; every account
	// failed, so the flow fails (as inbox/fetch@1 does), and nothing else ran.
	e.scratchFlag("heo-fail", true)
	var f created
	r := e.jsonOf(&f, "run", "create", "--flow", "inbox-fetch-heo", "--wait")
	if f.Status != "failed" {
		t.Fatalf("every account failed: exit %d status %q\n%s", r.Exit, f.Status, r.Stderr)
	}
	_, list = e.stepOf(f.Run, "list")
	if a, _ := list["accounts"].(map[string]any); a["acme"].(map[string]any)["error"] == nil {
		t.Errorf("the account error is data in the listing: %v", list)
	}
	rec, _ := e.stepOf(f.Run, "ingest")
	if rec.Failure == nil || rec.Failure.Code != core.CodeNodeReportedFailure {
		t.Errorf("ingest failure: %+v", rec.Failure)
	}
	// the script was told that account failed
	if b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "envelopes.json")); err != nil || //nolint:gosec // the fake's state
		!strings.Contains(string(b), `"error":"heo conversation list failed: CONFLICT: inbox is being rebuilt"`) {
		t.Errorf("stdin of the script: %s %v", b, err)
	}
}

// TestInboxTriageHeoAccounts: inbox-triage with heo-listed accounts makes ONE
// run per cycle. Each account in var heo_accounts is listed by heo (read
// only); inbox/ingest@1 hands the merged listings to the script, which
// fetches every other account through its own backend into the same run; the
// steps below see that one run. With heo_accounts empty the script gets an
// empty document, which it treats as a plain fetch.
func TestInboxTriageHeoAccounts(t *testing.T) {
	t.Run("a heo account and a backend account: one run", func(t *testing.T) {
		e := triageEnv(t)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()

		var c created
		r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var",
			`heo_accounts=[{"account":"acme","inbox":"ib-acme","email":"me@acme.example"}]`)
		if r.Exit != core.ExitAwaiting {
			t.Fatalf("run: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
		}
		_, list := e.stepOf(c.Run, "heo-list")
		if list != nil {
			t.Errorf("the for_each parent has no output of its own: %v", list)
		}
		_, fetch := e.stepOf(c.Run, "fetch")
		accts, _ := fetch["accounts"].(map[string]any)
		acme, _ := accts["acme"].(map[string]any)
		beta, _ := accts["beta"].(map[string]any)
		if len(accts) != 2 || acme["status"] != "ok" || num(acme["count"]) != 2 || beta["status"] != "ok" || num(beta["count"]) != 1 ||
			num(fetch["rows"]) != 3 || fetch["run"] == "" {
			t.Fatalf("one run holds both accounts (acme from heo, beta from the script's backend): %v", fetch)
		}
		// everything after the fetch works on that run
		_, rows := e.stepOf(c.Run, "rows")
		_, senders := e.stepOf(c.Run, "senders")
		_, plan := e.stepOf(c.Run, "plan")
		for name, out := range map[string]map[string]any{"rows": rows, "senders": senders, "plan": plan} {
			if out["run"] != fetch["run"] {
				t.Errorf("%s works on run %v, the fetch made %v", name, out["run"], fetch["run"])
			}
		}
		if got := e.inboxCalls("fetch"); len(got) != 1 || got[0] != "fetch --json --envelopes -" {
			t.Errorf("one fetch call, no listing by the script: %v", got)
		}
		// the script was handed only the heo account's envelopes
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "envelopes.json")) //nolint:gosec // the fake's state
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Accounts map[string]struct {
				Envelopes []struct{ ID string } `json:"envelopes"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(b, &doc); err != nil || len(doc.Accounts) != 1 || len(doc.Accounts["acme"].Envelopes) != 2 {
			t.Errorf("stdin of the script: %s %v", b, err)
		}
		if n := len(logOf(e, "heo.log")); n != 4 {
			t.Errorf("heo was listed %d times, want 4 (two pages of two listings): %v", n, logOf(e, "heo.log"))
		}
		for _, l := range logOf(e, "heo.log") {
			if !strings.HasPrefix(l, "heo conversation list ") && !strings.HasPrefix(l, "heo message list ") {
				t.Errorf("heo was asked for more than a listing: %s", l)
			}
		}
	})

	t.Run("no heo account: a plain fetch", func(t *testing.T) {
		e := triageEnv(t)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()

		c, _ := e.startTriage()
		_, fetch := e.stepOf(c.Run, "fetch")
		accts, _ := fetch["accounts"].(map[string]any)
		if len(accts) != 2 || num(fetch["rows"]) != 4 || num(fetch["new"]) != 4 {
			t.Errorf("the default scenario of the script, four messages: %v", fetch)
		}
		if got := e.inboxCalls("fetch"); len(got) != 1 || got[0] != "fetch --json --envelopes -" {
			t.Errorf("fetch calls: %v", got)
		}
		// an empty document: the script's plain fetch, no heo at all
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "envelopes.json")) //nolint:gosec // the fake's state
		if err != nil || strings.TrimSpace(string(b)) != `{"accounts":{}}` {
			t.Errorf("stdin of the script: %s %v", b, err)
		}
		if n := len(logOf(e, "heo.log")); n != 0 {
			t.Errorf("heo was called with no heo account: %v", logOf(e, "heo.log"))
		}
	})
}
