//go:build !windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reply path of the plan facts, on its own: heo/plan-facts@1 reads `heo message raw ID -o -` for the messages
// the plan would reply to and hands their header block, never a body, to the plan as `raw`. The wrapper runs for
// real over the fake heo (testdata/inbox/heoshims); no daemon, no account.

// planFacts runs heo-plan-facts for account acme with the reply ids given and returns the raw headers it put in
// the facts, by heo message id.
func planFacts(t *testing.T, e *env, replyIDs string) (wrapRun, map[string]string) {
	t.Helper()
	args := []string{"--account", "acme", "--channel", "acme", "--inbox", "ib-acme", "--email", "me@acme.example"}
	if replyIDs != "" {
		args = append(args, "--reply-ids", replyIDs)
	}
	r := wrap(t, e, "", "heo-plan-facts", args...)
	if r.exit != 0 {
		return r, nil
	}
	var facts struct {
		Accounts map[string]struct {
			Raw map[string]string `json:"raw"`
		} `json:"accounts"`
	}
	fj, _ := r.out["facts_json"].(string)
	if err := json.Unmarshal([]byte(fj), &facts); err != nil || len(facts.Accounts) != 1 {
		t.Fatalf("facts_json: %q %v", fj, err)
	}
	return r, facts.Accounts["acme"].Raw
}

func (e *env) fakeRaw(id, content string) {
	e.t.Helper()
	d := filepath.Join(e.scratch, "heo-raw")
	if err := os.MkdirAll(d, 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, id+".eml"), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func rawCalls(e *env) []string {
	var out []string
	for _, l := range logOf(e, "heo.log") {
		if strings.HasPrefix(l, "heo message raw ") {
			out = append(out, l)
		}
	}
	return out
}

func TestHeoPlanFactsReplyRaw(t *testing.T) {
	e := heoFetchEnv(t)

	t.Run("header block only, never the body", func(t *testing.T) {
		e.resetScratch()
		r, raw := planFacts(t, e, "msg_1,msg_3")
		if r.exit != 0 {
			t.Fatalf("exit %d\n%s", r.exit, r.err)
		}
		if len(raw) != 2 {
			t.Fatalf("raw: %v", raw)
		}
		h1 := raw["msg_1"]
		if !strings.Contains(h1, "Message-ID: <a1@vendor.example>") ||
			!strings.Contains(h1, "Authentication-Results: mx.acme.example;\r\n spf=pass") ||
			!strings.HasSuffix(h1, "Date: Wed, 18 Nov 2026 08:00:00 +0000\r\n\r\n") {
			t.Errorf("header block of msg_1 (folded lines kept, ends at the blank line): %q", h1)
		}
		if !strings.Contains(raw["msg_3"], "dkim=fail") {
			t.Errorf("header block of msg_3: %q", raw["msg_3"])
		}
		b, _ := json.Marshal(r.out)
		for _, leak := range []string{"SECRET-BODY", "the invoice is 42"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("a body is in the facts: %s", b)
			}
		}
		got := rawCalls(e)
		if len(got) != 2 || got[0] != "heo message raw msg_1 -o -" || got[1] != "heo message raw msg_3 -o -" {
			t.Errorf("raw calls: %v", got)
		}
		onlyReads(t, e)
		// subjects and addresses go to the file, never to stderr
		for _, leak := range []string{"vendor.example", "SECRET", "Invoice", "client.example", "acme.example"} {
			if strings.Contains(r.err, leak) {
				t.Errorf("stderr carries %q: %s", leak, r.err)
			}
		}
	})

	t.Run("no reply ids: no raw read, raw is empty", func(t *testing.T) {
		e.resetScratch()
		r, raw := planFacts(t, e, "")
		if r.exit != 0 || len(raw) != 0 || raw == nil {
			t.Fatalf("exit %d, raw %v (want {})", r.exit, raw)
		}
		if got := rawCalls(e); len(got) != 0 {
			t.Errorf("raw read with no reply: %v", got)
		}
	})

	t.Run("a missing message is left out, the others stay", func(t *testing.T) {
		e.resetScratch()
		r, raw := planFacts(t, e, "msg_404,msg_1")
		if r.exit != 0 {
			t.Fatalf("a missing message must not fail the facts: exit %d\n%s", r.exit, r.err)
		}
		if _, ok := raw["msg_404"]; ok || len(raw) != 1 || raw["msg_1"] == "" {
			t.Errorf("raw: %v", raw)
		}
	})

	t.Run("heo cannot read: left out, its error text never leaves", func(t *testing.T) {
		e.resetScratch()
		e.scratchFlag("heo-raw-fail", true)
		r, raw := planFacts(t, e, "msg_1,msg_3")
		if r.exit != 0 || len(raw) != 0 {
			t.Fatalf("exit %d, raw %v", r.exit, raw)
		}
		if strings.Contains(r.err, "SECRET-ERR") {
			t.Errorf("heo's error text reached stderr: %s", r.err)
		}
	})

	t.Run("no daemon: 70, passed through for the retry", func(t *testing.T) {
		e.resetScratch()
		e.scratchFlag("heo-down", true)
		defer e.scratchFlag("heo-down", false)
		if r, _ := planFacts(t, e, "msg_1"); r.exit != 70 {
			t.Errorf("exit %d", r.exit)
		}
	})

	t.Run("the daemon goes away at the raw read: 70, no partial facts, no address on stderr", func(t *testing.T) {
		e.resetScratch()
		e.scratchFlag("heo-raw-down", true)
		defer e.scratchFlag("heo-raw-down", false)
		r, _ := planFacts(t, e, "msg_1,msg_3")
		if r.exit != 70 || r.out != nil || !strings.Contains(r.err, "no daemon") {
			t.Errorf("exit %d, out %v, stderr %q", r.exit, r.out, r.err)
		}
		for _, leak := range []string{"vendor.example", "Invoice"} {
			if strings.Contains(r.err, leak) {
				t.Errorf("stderr carries %q: %s", leak, r.err)
			}
		}
	})

	t.Run("an empty read is left out", func(t *testing.T) {
		e.resetScratch()
		e.fakeRaw("msg_ok", "From: a@b.example\r\nMessage-ID: <ok@b.example>\r\n\r\nbody\r\n")
		e.fakeRaw("msg_dead", "")
		r, raw := planFacts(t, e, "msg_ok,msg_dead")
		if r.exit != 0 || raw["msg_ok"] == "" {
			t.Fatalf("exit %d raw %v", r.exit, raw)
		}
		if _, ok := raw["msg_dead"]; ok {
			t.Errorf("an empty read is no header block: %v", raw)
		}
	})

	t.Run("ids that are not ids are never given to heo", func(t *testing.T) {
		e.resetScratch()
		_, raw := planFacts(t, e, "--help,msg_1;rm,../x,$(id),msg_1")
		if len(raw) != 1 || raw["msg_1"] == "" {
			t.Errorf("raw: %v", raw)
		}
		if got := rawCalls(e); len(got) != 1 || got[0] != "heo message raw msg_1 -o -" {
			t.Errorf("raw calls: %v", got)
		}
	})

	t.Run("Message-ID: the header block is handed over as heo has it, keyed by heo id", func(t *testing.T) {
		// The wrapper does not compare the raw Message-ID with the listing's: the plan reads both (facts.inbox and
		// facts.raw) and decides. What is pinned here is that nothing is rewritten, so the plan can.
		e.resetScratch()
		e.fakeRaw("msg_1", "From: x@other.example\r\nMessage-ID: <other@other.example>\r\n\r\nSECRET-BODY\r\n")
		_, raw := planFacts(t, e, "msg_1")
		if !strings.Contains(raw["msg_1"], "Message-ID: <other@other.example>") || strings.Contains(raw["msg_1"], "a1@vendor.example") {
			t.Errorf("raw: %q", raw["msg_1"])
		}
	})

	t.Run("a header block cut by the size bound is not a header block", func(t *testing.T) {
		// Reply-To past the bound would be missed: the reply would be planned with the From as its recipient and no
		// Reply-To guard. A cut block is left out; the plan holds that reply as invalid.
		e.resetScratch()
		pad := strings.Repeat("X-Pad: "+strings.Repeat("p", 70)+"\r\n", 40)
		e.fakeRaw("msg_big", "From: a@b.example\r\n"+pad+"Reply-To: attacker@elsewhere.example\r\n\r\nSECRET-BODY\r\n")
		e.fakeRaw("msg_small", "From: a@b.example\r\nMessage-ID: <s@b.example>\r\n\r\nSECRET-BODY\r\n")
		e.extra = append(e.extra, "FLO_HEO_RAW_BYTES=1000")
		defer func() { e.extra = nil }()
		r, raw := planFacts(t, e, "msg_big,msg_small")
		if r.exit != 0 {
			t.Fatalf("exit %d\n%s", r.exit, r.err)
		}
		if h, ok := raw["msg_big"]; ok {
			t.Errorf("a header block cut at the bound (%d bytes, no Reply-To in it) is in the facts", len(h))
		}
		if raw["msg_small"] == "" {
			t.Errorf("a block inside the bound is kept: %v", raw)
		}
	})

	t.Run("the bound is a bound: nothing past it is looked at", func(t *testing.T) {
		e.resetScratch()
		e.fakeRaw("msg_hdronly", "From: a@b.example\r\nReply-To: c@d.example")
		e.extra = append(e.extra, "FLO_HEO_RAW_BYTES=1000")
		defer func() { e.extra = nil }()
		_, raw := planFacts(t, e, "msg_hdronly")
		// a message that is only headers (EOF before any blank line, inside the bound) is whole
		if !strings.Contains(raw["msg_hdronly"], "Reply-To: c@d.example") {
			t.Errorf("a header-only message is whole: %v", raw)
		}
	})
}

// inbox/replyneeds@1 has no wrapper: its node runs `inbox plan RUN --reply-needs --json` as it is. These pin the
// node's command line and output schema against what the script's (fake) answer is, and the flow's reaction.
func TestInboxReplyNeedsNode(t *testing.T) {
	spec := loadPackNodes(t, "inbox")["inbox/replyneeds@1"]
	if spec == nil {
		t.Fatal("inbox/replyneeds@1 is not in the inbox pack")
	}
	if strings.Join(spec.Argv, " ") != "inbox plan {{ in.run }} --reply-needs --json" {
		t.Errorf("argv: %v", spec.Argv)
	}
	const stamp = "20261118-090001"
	ask := func(e *env) (result, []map[string]any) {
		r := e.runShim("", "inbox", "plan", stamp, "--reply-needs", "--json")
		var out struct {
			Run        string           `json:"run"`
			Candidates []map[string]any `json:"candidates"`
		}
		if r.Exit == 0 {
			if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || out.Run != stamp || out.Candidates == nil {
				t.Fatalf("answer: %v %s", err, r.Stdout)
			}
		}
		return r, out.Candidates
	}

	t.Run("shape: the reply items as {account, id, message_id}, nothing else", func(t *testing.T) {
		e := heoFetchEnv(t)
		e.resetScratch()
		e.inboxFlag("heo-high", true)
		r, c := ask(e)
		if r.Exit != 0 || len(c) != 2 {
			t.Fatalf("exit %d, candidates %v\n%s", r.Exit, c, r.Stderr)
		}
		for i, id := range []string{"msg_h5", "msg_h6"} {
			if len(c[i]) != 3 || c[i]["id"] != id || c[i]["account"] != "gm1" || !strings.HasSuffix(c[i]["message_id"].(string), "@client.example>") {
				t.Errorf("candidate %d: %v", i, c[i])
			}
		}
		if strings.Contains(r.Stdout, "Thanks, on it") {
			t.Errorf("a reply's text is in the answer: %s", r.Stdout)
		}
		if got := e.inboxCalls("plan"); len(got) != 1 || got[0] != "plan "+stamp+" --reply-needs --json" {
			t.Errorf("script calls: %v", got)
		}
	})

	t.Run("empty: no reply item, candidates is []", func(t *testing.T) {
		e := heoFetchEnv(t)
		e.resetScratch()
		e.inboxFlag("heo-items", true)
		r, c := ask(e)
		if r.Exit != 0 || len(c) != 0 || !strings.Contains(r.Stdout, `"candidates":[]`) {
			t.Errorf("exit %d, %s", r.Exit, r.Stdout)
		}
	})

	t.Run("flow: no candidate, no raw read; a script failure stops the plan", func(t *testing.T) {
		const heoAcct = `heo_accounts=[{"account":"acme","inbox":"ib-acme","email":"me@acme.example"}]`
		e := triageEnv(t)
		e.inboxFlag("heo-items", true)
		e.triageActive()
		eng := e.startEngine()
		var c created
		if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var", heoAcct); c.Run == "" {
			t.Fatalf("run: exit %d\n%s", r.Exit, r.Stderr)
		}
		_, rf := e.stepOfElement(c.Run, "heo-facts", 0)
		var fd struct {
			Accounts map[string]struct {
				Raw map[string]string `json:"raw"`
			} `json:"accounts"`
		}
		fj, _ := rf["facts_json"].(string)
		if json.Unmarshal([]byte(fj), &fd) != nil || fd.Accounts["acme"].Raw == nil || len(fd.Accounts["acme"].Raw) != 0 {
			t.Errorf("no reply to plan, so no raw in the facts: %v", rf)
		}
		eng.stop()

		e = triageEnv(t)
		e.inboxFlag("replyneeds-fail", true)
		e.triageActive()
		eng = e.startEngine()
		defer eng.stop()
		if r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var", heoAcct); r.Exit == 0 || c.Status != "failed" {
			t.Fatalf("run: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
		}
		v := e.show(c.Run)
		if st, _ := v.step("reply-needs"); st != "failed" {
			t.Errorf("reply-needs is %s", st)
		}
		if st, reason := v.step("plan"); st != "skipped" || reason != "upstream_failed" {
			t.Errorf("plan: %s %s", st, reason)
		}
		if got := e.inboxCalls("plan"); len(planCalls(e)) != 0 || len(got) != 1 {
			t.Errorf("the plan was made without the reply needs: %v", got)
		}
	})
}
