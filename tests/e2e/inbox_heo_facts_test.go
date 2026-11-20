//go:build !windows

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// The facts the script is handed for heo accounts: the authentication headers of the messages that need them
// (heo/message.raw@1, inbox/authfacts@1) and what its plan reads from the mailbox (heo/plan-facts@1, the facts
// input of inbox/plan@1). The wrappers run for real; heo is a fake over testdata/inbox/heo (reads only), the
// script a fake (testdata/inbox/shims/inbox). No daemon, no account, no token.

func TestInboxFactsNodesDeclareHonestEffects(t *testing.T) {
	heo := loadPackNodes(t, "heo")
	for _, id := range []string{"heo/message.raw@1", "heo/plan-facts@1"} {
		n := heo[id]
		if n == nil {
			t.Fatalf("%s is not in the heo pack", id)
		}
		if n.SideEffect != node.SideEffectRead || n.Privacy != node.PrivacyLocal || n.Idempotent == nil || !*n.Idempotent ||
			!slices.Equal(n.Capabilities, []string{"mail.read"}) || n.Timeout == "" {
			t.Errorf("%s is a local, idempotent mail.read: %+v", id, n)
		}
		if n.ExitCodes == nil || !slices.Contains(n.ExitCodes.Retry, 70) {
			t.Errorf("%s: a missing daemon (70) is retried: %+v", id, n.ExitCodes)
		}
	}
	in := loadPackNodes(t, "inbox")
	af := in["inbox/authfacts@1"]
	if af == nil {
		t.Fatal("inbox/authfacts@1 is not in the inbox pack")
	}
	if af.SideEffect != node.SideEffectWriteLocal || af.Privacy != node.PrivacyLocal || af.Idempotent == nil || !*af.Idempotent {
		t.Errorf("inbox/authfacts@1 writes the script's local store only: %+v", af)
	}
	if _, ok := in["inbox/plan@1"].Inputs["facts"]; !ok {
		t.Error("inbox/plan@1 takes the facts of the heo accounts")
	}
}

func lines(t *testing.T, e *env, name string) []string {
	t.Helper()
	return logOf(e, name)
}

func onlyReads(t *testing.T, e *env) {
	t.Helper()
	for _, l := range lines(t, e, "heo.log") {
		ok := false
		for _, p := range []string{"heo conversation list ", "heo message list ", "heo message raw ", "heo channel get ", "heo provider-label list "} {
			ok = ok || strings.HasPrefix(l, p)
		}
		if !ok {
			t.Errorf("heo was asked for more than a read: %s", l)
		}
	}
}

func TestHeoMessageRawWrapper(t *testing.T) {
	e := heoFetchEnv(t)
	cands := `{"candidates": [
	  {"account": "acme", "message_id": "<a1@vendor.example>", "provider_id": "msg_1"},
	  {"account": "acme", "message_id": "<a3@client.example>", "provider_id": null},
	  {"account": "acme", "message_id": "<gone@vendor.example>", "provider_id": "msg_404"},
	  {"account": "acme", "message_id": "<nowhere@vendor.example>", "provider_id": ""}]}`
	e.resetScratch()
	run := func(stdin string, args ...string) wrapRun {
		return wrap(t, e, stdin, "heo-message-raw", args...)
	}
	r := run(cands)
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.err)
	}
	b, _ := json.Marshal(r.out)
	doc := string(b)
	stats, _ := r.out["stats"].(map[string]any)
	if num(stats["requested"]) != 4 || num(stats["fetched"]) != 2 || num(stats["failed"]) != 2 || num(stats["skipped"]) != 0 {
		t.Errorf("stats: %v", stats)
	}
	msgs := r.out["accounts"].(map[string]any)["acme"].(map[string]any)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages: %v", msgs)
	}
	h1 := msgs[0].(map[string]any)["headers"].(string)
	if msgs[0].(map[string]any)["message_id"] != "<a1@vendor.example>" ||
		!strings.Contains(h1, "Message-ID: <a1@vendor.example>") ||
		!strings.Contains(h1, "Authentication-Results: mx.acme.example;\r\n spf=pass smtp.mailfrom=vendor.example;\r\n dkim=pass header.d=vendor.example") {
		t.Errorf("headers of msg_1 (the folded Authentication-Results is kept whole): %q", h1)
	}
	// only the auth headers: no body, subject, address, date, received
	for _, leak := range []string{"SECRET-BODY", "SECRET-SUBJECT", "billing@vendor", "me@acme", "Received", "Date:"} {
		if strings.Contains(doc, leak) {
			t.Errorf("the output carries %q: %s", leak, doc)
		}
	}
	// a3 had no heo id: found by its exact Message-ID, one hit
	if h3 := msgs[1].(map[string]any)["headers"].(string); !strings.Contains(h3, "dkim=fail") || msgs[1].(map[string]any)["message_id"] != "<a3@client.example>" {
		t.Errorf("headers of the message found by Message-ID: %q", h3)
	}
	if strings.Contains(r.err, "SECRET") || strings.Contains(r.err, "vendor.example") {
		t.Errorf("stderr: %s", r.err)
	}
	onlyReads(t, e)
	hl := strings.Join(lines(t, e, "heo.log"), "\n")
	if !strings.Contains(hl, "heo message raw msg_1 -o -") || !strings.Contains(hl, "heo message list --message-id <a3@client.example>") {
		t.Errorf("heo calls:\n%s", hl)
	}

	// bounds: one message, and only the first bytes of it
	r = run(cands, "--max-messages", "1")
	if st, _ := r.out["stats"].(map[string]any); num(st["fetched"]) != 1 || num(st["skipped"]) != 3 {
		t.Errorf("--max-messages 1: %v", r.out["stats"])
	}
	r = run(cands, "--max-bytes", "60")
	msgs = r.out["accounts"].(map[string]any)["acme"].(map[string]any)["messages"].([]any)
	if h := msgs[0].(map[string]any)["headers"].(string); len(h) > 60 || strings.Contains(h, "dkim=pass") {
		t.Errorf("--max-bytes 60 keeps %d bytes: %q", len(h), h)
	}

	// heo cannot read: counted, not passed on; nothing from heo's error reaches stderr
	e.scratchFlag("heo-raw-fail", true)
	r = run(cands)
	if st, _ := r.out["stats"].(map[string]any); r.exit != 0 || num(st["fetched"]) != 0 || num(st["failed"]) != 4 {
		t.Errorf("raw failures: exit %d %v", r.exit, r.out["stats"])
	}
	if strings.Contains(r.err, "SECRET-ERR") {
		t.Errorf("heo's error text reached stderr: %s", r.err)
	}
	e.scratchFlag("heo-raw-fail", false)

	// no daemon: passed through, the node retries
	e.scratchFlag("heo-down", true)
	if r = run(cands); r.exit != 70 {
		t.Errorf("no daemon: exit %d", r.exit)
	}
	e.scratchFlag("heo-down", false)
	if r = run("not json"); r.exit != 64 {
		t.Errorf("bad stdin: exit %d", r.exit)
	}
}

func TestHeoPlanFactsWrapper(t *testing.T) {
	e := heoFetchEnv(t)
	e.resetScratch()
	run := func(args ...string) wrapRun {
		return wrap(t, e, "", "heo-plan-facts", args...)
	}
	r := run("--account", "acme", "--channel", "acme", "--inbox", "ib-acme", "--email", "me@acme.example")
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.err)
	}
	var facts struct {
		Accounts map[string]struct {
			Labels []string                     `json:"labels"`
			Verbs  map[string]map[string]string `json:"verbs"`
			Inbox  map[string]string            `json:"inbox"`
			Email  string                       `json:"email"`
			Auto   *bool                        `json:"auto"`
		} `json:"accounts"`
	}
	fj, _ := r.out["facts_json"].(string)
	if err := json.Unmarshal([]byte(fj), &facts); err != nil || len(facts.Accounts) != 1 {
		t.Fatalf("facts_json: %q %v", fj, err)
	}
	a := facts.Accounts["acme"]
	for _, l := range []string{"INBOX", "[Gmail]/Trash", "[Gmail]/Spam", "[Gmail]/All Mail", "Finance", "Waiting"} {
		if !slices.Contains(a.Labels, l) {
			t.Errorf("labels %v lack %s", a.Labels, l)
		}
	}
	if a.Verbs["conversation archive"]["state"] != "declared" || a.Verbs["conversation trash"]["state"] != "unsupported" ||
		a.Verbs["conversation trash"]["reason"] != "not_declared" {
		t.Errorf("verbs: %v", a.Verbs)
	}
	if len(a.Inbox) != 2 || a.Inbox["msg_1"] != "<a1@vendor.example>" || a.Inbox["msg_3"] != "<a3@client.example>" || a.Email != "me@acme.example" {
		t.Errorf("inbox %v, email %q", a.Inbox, a.Email)
	}
	if strings.Contains(fj, "\n") {
		t.Errorf("facts_json is one line: %q", fj)
	}
	// canary switch: the facts say auto false unless the account was told true
	if a.Auto == nil || *a.Auto {
		t.Errorf("auto defaults to false: %v", a.Auto)
	}
	autoOf := func(args ...string) string {
		t.Helper()
		r := run(append([]string{"--account", "acme", "--inbox", "ib-acme"}, args...)...)
		if r.exit != 0 {
			t.Fatalf("exit %d\n%s", r.exit, r.err)
		}
		var f struct {
			Accounts map[string]struct {
				Auto any `json:"auto"`
			} `json:"accounts"`
		}
		_ = json.Unmarshal([]byte(r.out["facts_json"].(string)), &f)
		return fmt.Sprint(f.Accounts["acme"].Auto)
	}
	for args, want := range map[string]string{"--auto true": "true", "--auto false": "false", "--auto yes": "false", "--auto ": "false"} {
		if got := autoOf(strings.Split(args, " ")...); got != want {
			t.Errorf("%q: auto %s, want %s", args, got, want)
		}
	}
	onlyReads(t, e)
	if strings.Contains(r.err, "vendor.example") || strings.Contains(r.err, "Invoice") {
		t.Errorf("stderr: %s", r.err)
	}

	// heo refuses: the account is data, no facts (the script then plans it invalid)
	e.scratchFlag("heo-fail", true)
	r = run("--account", "acme", "--inbox", "ib-acme")
	if r.exit != 3 || r.out["facts_json"] != "" || !strings.Contains(r.out["error"].(string), "CONFLICT") {
		t.Errorf("heo failed: exit %d %v", r.exit, r.out)
	}
	e.scratchFlag("heo-fail", false)
	e.scratchFlag("heo-down", true)
	if r = run("--account", "acme", "--inbox", "ib-acme"); r.exit != 70 {
		t.Errorf("no daemon: exit %d", r.exit)
	}
	e.scratchFlag("heo-down", false)
	// an unknown channel fails the account, not the run
	r = run("--account", "nowhere", "--inbox", "ib-acme")
	if r.exit != 3 || r.out["facts_json"] != "" {
		t.Errorf("unknown channel: exit %d %v", r.exit, r.out)
	}
}

func TestInboxFactsWrappers(t *testing.T) {
	e := heoFetchEnv(t)
	t.Run("inbox-authfacts: candidates, then the raw document", func(t *testing.T) {
		e.resetScratch()
		r := e.runShim("", "inbox-authfacts", "--account", "acme")
		var out struct {
			Candidates []map[string]any `json:"candidates"`
		}
		if r.Exit != 0 || json.Unmarshal([]byte(r.Stdout), &out) != nil || len(out.Candidates) != 2 {
			t.Fatalf("candidates: exit %d %s\n%s", r.Exit, r.Stdout, r.Stderr)
		}
		doc := `{"accounts": {"acme": {"messages": [{"message_id": "<a1@vendor.example>", "headers": "Message-ID: <a1@vendor.example>\r\n\r\n"}]}}}`
		r = e.runShim(doc, "inbox-authfacts", "--account", "acme")
		if r.Exit != 0 || !strings.Contains(r.Stdout, `"filled":{"acme":1}`) {
			t.Fatalf("from-raw: exit %d %s\n%s", r.Exit, r.Stdout, r.Stderr)
		}
		got := e.inboxLog()
		want := []string{"authfill --candidates --json --account acme", "authfill --from-raw - --json"}
		if !slices.Equal(got, want) {
			t.Errorf("script calls %v, want %v", got, want)
		}
		// an empty document is not a raw document: the candidates again, nothing stored
		e.resetScratch()
		if r = e.runShim("{}", "inbox-authfacts"); r.Exit != 0 || !strings.Contains(r.Stdout, "candidates") {
			t.Errorf("empty doc: %s", r.Stdout)
		}
		if got := e.inboxCalls("authfill"); len(got) != 1 || strings.Contains(got[0], "--from-raw") {
			t.Errorf("empty doc must not store: %v", got)
		}
	})

	t.Run("inbox-plan: facts on stdin are merged and handed to the script", func(t *testing.T) {
		e.resetScratch()
		if r := e.runShim("", "inbox", "fetch", "--json"); r.Exit != 0 {
			t.Fatal("fake fetch")
		}
		const stamp = "20261118-090001"
		e.runShim("", "inbox", "decide", stamp, "--json")
		f1 := `{"accounts":{"acme":{"labels":["Finance"],"inbox":{"msg_1":"<a1@vendor.example>"}}}}`
		f2 := `{"accounts":{"beta":{"labels":["Legal"]}}}`
		planFile := filepath.Join(t.TempDir(), "plan.json")
		r := e.runShim(f1+"\n"+f2+"\n", "inbox-plan", "--out", planFile, stamp)
		if r.Exit != 0 {
			t.Fatalf("exit %d\n%s", r.Exit, r.Stderr)
		}
		got := e.inboxCalls("plan")
		if len(got) != 1 || got[0] != "plan "+stamp+" --unattended --no-task --json --facts -" {
			t.Errorf("plan call: %v", got)
		}
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "facts.json")) //nolint:gosec // the fake's state
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Accounts map[string]any `json:"accounts"`
		}
		if json.Unmarshal(b, &d) != nil || len(d.Accounts) != 2 || d.Accounts["acme"] == nil || d.Accounts["beta"] == nil {
			t.Errorf("the script's stdin: %s", b)
		}
		// no facts: the plan as before, no --facts, nothing on stdin
		e.resetScratch()
		e.runShim("", "inbox", "fetch", "--json")
		e.runShim("", "inbox", "decide", stamp, "--json")
		if r = e.runShim("", "inbox-plan", "--out", planFile, stamp); r.Exit != 0 {
			t.Fatalf("exit %d\n%s", r.Exit, r.Stderr)
		}
		if got := e.inboxCalls("plan"); len(got) != 1 || got[0] != "plan "+stamp+" --unattended --no-task --json" {
			t.Errorf("plan call without facts: %v", got)
		}
		if exists(filepath.Join(e.scratch, "inbox", "facts.json")) {
			t.Error("facts reached the script with none given")
		}
	})
}

// TestInboxTriageHeoFacts: inbox-triage wired for heo accounts. The authentication facts of the heo account's
// messages are read through heo and handed to the script before the sender facts; the plan is made from facts
// read from heo after the decisions. With heo_accounts empty none of that exists.
func TestInboxTriageHeoFacts(t *testing.T) {
	t.Run("heo account: auth facts, then the plan from facts", func(t *testing.T) {
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
		_, need := e.stepOf(c.Run, "auth-need")
		if cs, _ := need["candidates"].([]any); len(cs) != 2 {
			t.Errorf("auth-need: %v", need)
		}
		_, raw := e.stepOf(c.Run, "auth-raw")
		if st, _ := raw["stats"].(map[string]any); num(st["requested"]) != 2 || num(st["fetched"]) != 2 {
			t.Errorf("auth-raw: %v", raw)
		}
		_, fill := e.stepOf(c.Run, "auth-fill")
		if f, _ := fill["filled"].(map[string]any); num(f["acme"]) != 2 {
			t.Errorf("auth-fill: %v", fill)
		}
		// what the script was handed: auth headers only
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "rawdoc.json")) //nolint:gosec // the fake's state
		if err != nil || !strings.Contains(string(b), "dkim=pass") || strings.Contains(string(b), "SECRET") {
			t.Errorf("the script's raw document: %s %v", b, err)
		}
		// order of the script's calls: fetch, candidates, from-raw, senders, ..., plan with facts
		var order []string
		for _, l := range e.inboxLog() {
			w := strings.Fields(l)
			if len(w) > 0 && slices.Contains([]string{"fetch", "authfill", "senders", "plan"}, w[0]) {
				order = append(order, strings.Join(w[:min(2, len(w))], " "))
			}
		}
		if i, j := slices.Index(order, "senders "+"20261118-090001"), slices.Index(order, "authfill --from-raw"); j < 0 || i < j {
			t.Errorf("the headers are stored before the sender facts are read: %v", order)
		}
		plans := planCalls(e)
		if len(plans) != 1 || !strings.HasSuffix(plans[0], " --facts -") {
			t.Errorf("plan calls: %v", plans)
		}
		fb, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "facts.json")) //nolint:gosec // the fake's state
		var fd struct {
			Accounts map[string]struct {
				Labels []string          `json:"labels"`
				Inbox  map[string]string `json:"inbox"`
				Auto   *bool             `json:"auto"`
			} `json:"accounts"`
		}
		if err != nil || json.Unmarshal(fb, &fd) != nil || len(fd.Accounts) != 1 || !slices.Contains(fd.Accounts["acme"].Labels, "Finance") ||
			len(fd.Accounts["acme"].Inbox) != 2 {
			t.Errorf("the plan's facts: %s %v", fb, err)
		}
		if a := fd.Accounts["acme"].Auto; a == nil || *a {
			t.Errorf("heo_accounts without auto: the plan's facts say auto false (canary), got %v", a)
		}
		onlyReads(t, e)
	})

	t.Run("heo_accounts[].auto reaches the plan's facts", func(t *testing.T) {
		for _, tc := range []struct{ json, want string }{
			{`"auto":true`, "true"}, {`"auto":false`, "false"}, {`"x":1`, "false"}} {
			e := triageEnv(t)
			e.triageActive()
			eng := e.startEngine()
			var c created
			r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var",
				`heo_accounts=[{"account":"acme","inbox":"ib-acme","email":"me@acme.example",`+tc.json+`}]`)
			if r.Exit != core.ExitAwaiting {
				eng.stop()
				t.Fatalf("%s: run: exit %d\n%s", tc.json, r.Exit, r.Stderr)
			}
			fb, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "facts.json")) //nolint:gosec // the fake's state
			var fd struct {
				Accounts map[string]struct {
					Auto any `json:"auto"`
				} `json:"accounts"`
			}
			if err != nil || json.Unmarshal(fb, &fd) != nil || fmt.Sprint(fd.Accounts["acme"].Auto) != tc.want {
				t.Errorf("%s: facts %s %v, want auto %s", tc.json, fb, err, tc.want)
			}
			eng.stop()
		}
	})

	t.Run("heo failed for plan facts: no facts, the plan runs", func(t *testing.T) {
		e := triageEnv(t)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		// the listing at fetch time works; facts fail afterwards: switch heo off once the fetch is done
		var c created
		e.scratchFlag("heo-down", false)
		r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var",
			`heo_accounts=[{"account":"nowhere","inbox":"ib-acme"}]`)
		if r.Exit != core.ExitAwaiting && r.Exit != 0 {
			t.Fatalf("run: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
		}
		_, facts := e.stepOfElement(c.Run, "heo-facts", 0)
		if facts["error"] == nil || facts["facts_json"] != "" {
			t.Errorf("an account heo cannot answer for has no facts: %v", facts)
		}
		if plans := planCalls(e); len(plans) != 1 || strings.Contains(plans[0], "--facts") {
			t.Errorf("no facts at all: the plan is asked as before: %v", plans)
		}
	})

	t.Run("no heo account: none of it", func(t *testing.T) {
		e := triageEnv(t)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		c, _ := e.startTriage()
		for _, step := range []string{"auth-need", "auth-raw", "auth-fill"} {
			if st := e.stepState(c.Run, step); st != "skipped" {
				t.Errorf("step %s: %q, want skipped", step, st)
			}
		}
		if got := e.inboxCalls("authfill"); len(got) != 0 {
			t.Errorf("authfill with no heo account: %v", got)
		}
		plans := e.inboxCalls("plan")
		if len(plans) != 1 || plans[0] != "plan 20261118-090001 --unattended --no-task --json" {
			t.Errorf("the plan is the plan of before: %v", plans)
		}
		if exists(filepath.Join(e.scratch, "inbox", "facts.json")) {
			t.Error("facts with no heo account")
		}
		if n := len(logOf(e, "heo.log")); n != 0 {
			t.Errorf("heo was called with no heo account: %v", logOf(e, "heo.log"))
		}
	})
}

// stepState is the status of a step's record ("" when it has none).
func (e *env) stepState(run, step string) string {
	e.t.Helper()
	recs, err := e.store().Runs.ListSteps(context.Background(), tenant, core.RunID(run))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range recs {
		if r.StepID == step && r.Element == nil {
			return string(r.Status)
		}
	}
	return ""
}

// stepOfElement is the output of one element of a for_each step.
func (e *env) stepOfElement(run, step string, i int) (core.StepRun, map[string]any) {
	e.t.Helper()
	recs, err := e.store().Runs.ListSteps(context.Background(), tenant, core.RunID(run))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range recs {
		if r.StepID == step && r.Element != nil && r.Element.Index == i {
			out, _ := r.Output.(map[string]any)
			return r, out
		}
	}
	e.t.Fatalf("run %s has no element %d of step %s", run, i, step)
	return core.StepRun{}, nil
}

// planCalls are the calls of `inbox plan` that plan: the question of which messages need a raw original
// (--reply-needs) is asked beside them and plans nothing.
func planCalls(e *env) []string {
	var out []string
	for _, l := range e.inboxCalls("plan") {
		if !strings.Contains(l, " --reply-needs ") {
			out = append(out, l)
		}
	}
	return out
}
