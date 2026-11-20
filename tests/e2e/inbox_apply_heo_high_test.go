//go:build !windows

package e2e

import (
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

// Apply through heo, the high-blast ops: trash, spam, unsubscribe, reply (draft and send) and a move to another
// account. The wrappers run for real over a fake heo and a fake script. What these tests pin: a deleting op reaches heo
// only for an item the script cleared (and the vendor item never does), every write names the one message, the
// cross-account move forwards first, finds the copy by its own Message-ID and trashes the source last, and nothing
// is lost or repeated when a step fails.

const (
	moveStdin = `{"n": 7, "provider_id": "msg_h7", "channel": "gm1", "guard": "cleared", "mailbox": "Finance",
  "dest": {"account": "gm2", "channel": "gm2", "inbox": "ib-gm2", "email": "me@gm2.example"}}`
	destinations = `[{"account": "gm1", "channel": "gm1", "inbox": "ib-gm1", "email": "me@gm1.example"},
  {"account": "gm2", "channel": "gm2", "inbox": "ib-gm2", "email": "me@gm2.example"}]`
)

func heoHighEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	return heoActEnv(t, append([]string{"FLO_HEO_ARRIVAL_POLL=0.01", "FLO_HEO_ARRIVAL_TIMEOUT=1"}, extra...)...)
}

func setScratch(t *testing.T, e *env, name, val string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.scratch, name), []byte(val), 0o600); err != nil {
		t.Fatal(err)
	}
}

// heoCalls are the lines of heo.log that start with prefix.
func heoCalls(e *env, prefix string) []string {
	var out []string
	for _, l := range logOf(e, "heo.log") {
		if strings.HasPrefix(l, "heo "+prefix) {
			out = append(out, l)
		}
	}
	return out
}

func indexOfCall(e *env, sub string) int {
	for i, l := range logOf(e, "heo.log") {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

func TestHeoHighNodesDeclareHonestEffects(t *testing.T) {
	heo := loadPackNodes(t, "heo")
	n := heo["heo/conversation.act-high@1"]
	if n == nil {
		t.Fatal("heo/conversation.act-high@1 is not in the heo pack")
	}
	if n.SideEffect != node.SideEffectIrreversible || n.Approval != "required" || n.Privacy != node.PrivacyLocal ||
		n.Idempotent == nil || !*n.Idempotent || !slices.Equal(n.Capabilities, []string{"mail.write", "mail.send"}) || n.Timeout == "" {
		t.Errorf("act-high: irreversible, approval required, local, idempotent, mail.write + mail.send: %+v", n)
	}
	if n.ExitCodes == nil || !slices.Equal(n.ExitCodes.Retry, []int{6, 70}) || !slices.Equal(n.ExitCodes.OK, []int{0, 3}) {
		t.Errorf("act-high exit codes (8 outcome unknown is neither): %+v", n.ExitCodes)
	}
	if !n.Inputs["provider_id"].Required.Flag {
		t.Error("act-high requires provider_id")
	}
	// the low node keeps its six verbs: nothing destructive was added to it
	low := heo["heo/conversation.act@1"].Inputs["ops"].Items.Properties["verb"].Enum
	if len(low) != 6 {
		t.Errorf("the low act node grew: %v", low)
	}
	in := loadPackNodes(t, "inbox")
	if r := in["inbox/replyneeds@1"]; r == nil || r.SideEffect != node.SideEffectRead {
		t.Errorf("inbox/replyneeds@1: %+v", r)
	}
}

func TestHeoMessageMoveWrapper(t *testing.T) {
	move := func(e *env, stdin string) wrapRun { return wrap(t, e, stdin, "heo-message-move") }

	t.Run("forward, find the copy by its own Message-ID, file it, trash the source last", func(t *testing.T) {
		e := heoHighEnv(t)
		setScratch(t, e, "heo-copy-after", "2") // the copy is seen on the third look
		r := move(e, moveStdin)
		if r.exit != 0 || r.out["status"] != "ok" {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		fwd := indexOfCall(e, "message forward msg_h7 --to me@gm2.example ")
		find := indexOfCall(e, "message list --inbox ib-gm2 --message-id <fwd-")
		tag := indexOfCall(e, "conversation tag c_copy_")
		arch := indexOfCall(e, "conversation archive c_copy_")
		trash := indexOfCall(e, "conversation trash c_msg_h7 --message msg_h7 ")
		if fwd < 0 || fwd >= find || find >= tag || tag >= arch || arch >= trash {
			t.Fatalf("order forward %d, find %d, tag %d, archive %d, trash %d\n%s", fwd, find, tag, arch, trash, strings.Join(logOf(e, "heo.log"), "\n"))
		}
		if n := len(heoCalls(e, "message list --inbox ib-gm2 --message-id ")); n != 3 {
			t.Errorf("the arrival was looked for %d times, want 3 (two empty, one hit)", n)
		}
		all := strings.Join(logOf(e, "heo.log"), "\n")
		if strings.Contains(all, "--references") {
			t.Errorf("--references finds replies too: it must never be used to find the copy\n%s", all)
		}
		// the copy is labelled and archived on its own message, in the destination; the label is the move_to mailbox
		if !strings.Contains(all, "heo conversation tag c_copy_") || !strings.Contains(all, "--add Finance --message msg_copy_") ||
			!strings.Contains(all, "conversation archive c_copy_") || !strings.Contains(all, "--message msg_copy_") {
			t.Errorf("the copy is not filed by its own message:\n%s", all)
		}
		// the copy found carries the forward's own id, taken from the operation's provider references
		if !strings.Contains(all, "--message-id <fwd-") || !strings.Contains(all, "@copy.example>") {
			t.Errorf("the copy was not looked for by the forward's Message-ID:\n%s", all)
		}
		if r.out["copy"] == nil || r.out["forward"] == nil {
			t.Errorf("output lacks forward/copy: %v", r.out)
		}
	})

	t.Run("a rerun replays the same forward: no second copy, whatever the run", func(t *testing.T) {
		e := heoHighEnv(t)
		move(e, moveStdin)
		move(e, moveStdin)
		keys := keysOf(heoCalls(e, "message forward "))
		if len(keys) != 2 || keys[0] != keys[1] {
			t.Errorf("forward keys: %v", keys)
		}
		if strings.Contains(keys[0], "20261118") {
			t.Errorf("the forward key names a run: %s", keys[0])
		}
	})

	t.Run("the copy never appears: held, the source is not trashed, not tagged", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-copy-never", true)
		r := move(e, moveStdin)
		if r.exit != 0 || r.out["status"] != "held" || !strings.Contains(r.out["reason"].(string), "did not appear") ||
			!strings.Contains(r.out["reason"].(string), "source kept") {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		for _, bad := range []string{"conversation trash", "conversation tag", "conversation archive"} {
			if indexOfCall(e, bad) >= 0 {
				t.Errorf("%s after a copy that never arrived:\n%s", bad, strings.Join(logOf(e, "heo.log"), "\n"))
			}
		}
		if len(heoCalls(e, "message forward ")) != 1 {
			t.Error("the forward is sent once")
		}
	})

	t.Run("the destination label cannot be set: failed, the source is kept, the copy is where it landed", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-submit-fail-tag", true)
		r := move(e, moveStdin)
		if r.exit != 3 || r.out["status"] != "failed" || !strings.Contains(r.out["error"].(string), "could not be labelled") ||
			!strings.Contains(r.out["error"].(string), "source kept") {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if indexOfCall(e, "conversation trash") >= 0 {
			t.Error("the source was trashed though the copy was not filed")
		}
		if r.out["copy"] == nil {
			t.Error("the output does not say where the copy is")
		}
	})

	t.Run("the archive of the copy fails: the source is kept", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-submit-fail-archive", true)
		r := move(e, moveStdin)
		if r.exit != 3 || !strings.Contains(r.out["error"].(string), "could not be archived") || indexOfCall(e, "conversation trash") >= 0 {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, strings.Join(logOf(e, "heo.log"), "\n"))
		}
	})

	t.Run("the source cannot be trashed after the copy is filed: failed, the mail exists twice, nothing is lost", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-submit-fail-trash", true)
		r := move(e, moveStdin)
		if r.exit != 3 || !strings.Contains(r.out["error"].(string), "exists twice") {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if indexOfCall(e, "conversation archive c_copy_") < 0 {
			t.Error("the copy was not filed first")
		}
	})

	t.Run("forward unsupported (outlook): refused before anything is sent", func(t *testing.T) {
		e := heoHighEnv(t)
		r := move(e, strings.Replace(moveStdin, `"channel": "gm1"`, `"channel": "ol"`, 1))
		if r.exit != 3 || r.out["status"] != "failed" || !strings.Contains(r.out["error"].(string), "forward is not available") {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if len(writes(e)) != 0 {
			t.Errorf("a write on a channel that cannot forward: %v", writes(e))
		}
	})

	t.Run("the destination has no such label: nothing is forwarded", func(t *testing.T) {
		e := heoHighEnv(t)
		r := move(e, strings.Replace(moveStdin, `"Finance"`, `"Nope"`, 1))
		if r.exit != 3 || !strings.Contains(r.out["error"].(string), "does not exist in the destination") || len(writes(e)) != 0 {
			t.Fatalf("exit %d %v writes %v", r.exit, r.out, writes(e))
		}
	})

	t.Run("an outcome heo cannot confirm for the forward parks it: exit 8, nothing trashed", func(t *testing.T) {
		e := heoHighEnv(t)
		setScratch(t, e, "heo-delivery-forward", "outcome_unknown")
		r := move(e, moveStdin)
		if r.exit != 8 || r.out["status"] != "outcome_unknown" || indexOfCall(e, "conversation trash") >= 0 {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if !strings.Contains(r.err, "op_") {
			t.Errorf("the operation id is not on stderr: %q", r.err)
		}
	})

	t.Run("a forward that fails is a failure, the source stays", func(t *testing.T) {
		e := heoHighEnv(t)
		setScratch(t, e, "heo-delivery-forward", "failed")
		r := move(e, moveStdin)
		if r.exit != 3 || r.out["status"] != "failed" || len(heoCalls(e, "message list --inbox")) != 0 || indexOfCall(e, "conversation trash") >= 0 {
			t.Fatalf("exit %d %v", r.exit, r.out)
		}
	})

	t.Run("heo reports no Message-ID for the copy: held, source kept", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-forward-noref", true)
		r := move(e, moveStdin)
		if r.exit != 0 || r.out["status"] != "held" || indexOfCall(e, "conversation trash") >= 0 || len(heoCalls(e, "message list --inbox")) != 0 {
			t.Fatalf("exit %d %v", r.exit, r.out)
		}
	})

	t.Run("two messages with the copy's id: failed, source kept", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-copy-twice", true)
		r := move(e, moveStdin)
		if r.exit != 3 || !strings.Contains(r.out["error"].(string), "more than one") || indexOfCall(e, "conversation trash") >= 0 {
			t.Fatalf("exit %d %v", r.exit, r.out)
		}
	})

	t.Run("without the guard mark nothing is called", func(t *testing.T) {
		for name, stdin := range map[string]string{
			"no guard":      strings.Replace(moveStdin, `"guard": "cleared"`, `"guard": ""`, 1),
			"other guard":   strings.Replace(moveStdin, `"cleared"`, `"ok"`, 1),
			"no address":    strings.Replace(moveStdin, `"me@gm2.example"`, `""`, 1),
			"two addresses": strings.Replace(moveStdin, `"me@gm2.example"`, `"a@b.example,c@d.example"`, 1),
			"no message":    strings.Replace(moveStdin, `"msg_h7"`, `""`, 1),
		} {
			e := heoHighEnv(t)
			if r := move(e, stdin); r.exit != 64 {
				t.Errorf("%s: exit %d, want 64\n%s", name, r.exit, r.err)
			}
			if got := logOf(e, "heo.log"); len(got) != 0 {
				t.Errorf("%s: heo was called: %v", name, got)
			}
		}
	})

	t.Run("daemon down: 70, retried, nothing written", func(t *testing.T) {
		e := heoHighEnv(t)
		e.scratchFlag("heo-down", true)
		if r := move(e, moveStdin); r.exit != 70 || len(writes(e)) != 0 {
			t.Errorf("exit %d, writes %v", r.exit, writes(e))
		}
	})
}

func TestHeoConversationActHigh(t *testing.T) {
	act := func(e *env, stdin string) wrapRun { return wrap(t, e, stdin, "heo-conversation-act", "--high") }
	one := func(extra string) string {
		return `{"n": 2, "account": "gm1", "channel": "gm1", "provider_id": "msg_h2", "guard": "cleared", "destinations": ` + destinations + `, "ops": [` + extra + `]}`
	}
	noGuard := func(s string) string { return strings.Replace(s, `"guard": "cleared"`, `"guard": ""`, 1) }

	t.Run("trash and spam name the one message", func(t *testing.T) {
		e := heoHighEnv(t)
		r := act(e, one(`{"verb": "trash", "key": "k-trash"}`))
		if r.exit != 0 || r.out["status"] != "ok" {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if got := heoCalls(e, "conversation trash "); len(got) != 1 || !strings.HasPrefix(got[0], "heo conversation trash c_msg_h2 --message msg_h2 --idempotency-key k-trash") {
			t.Errorf("trash: %v", got)
		}
		e = heoHighEnv(t)
		r = act(e, one(`{"verb": "spam", "key": "k-spam"}`))
		if got := heoCalls(e, "conversation move "); r.exit != 0 || len(got) != 1 || !strings.Contains(got[0], "--role junk") || !strings.Contains(got[0], "--message msg_h2 ") {
			t.Errorf("spam: exit %d %v", r.exit, got)
		}
	})

	t.Run("a deleting op without the guard mark is refused before any call", func(t *testing.T) {
		for name, ops := range map[string]string{
			"trash":   `{"verb": "trash", "key": "k"}`,
			"spam":    `{"verb": "spam", "key": "k"}`,
			"move_to": `{"verb": "move_to", "key": "k", "account": "gm2", "mailbox": "Finance"}`,
			"mixed":   `{"verb": "read", "key": "k1"}, {"verb": "trash", "key": "k"}`,
		} {
			e := heoHighEnv(t)
			if r := act(e, noGuard(one(ops))); r.exit != 64 || !strings.Contains(r.err, "guard") {
				t.Errorf("%s: exit %d, want 64\n%s", name, r.exit, r.err)
			}
			if got := logOf(e, "heo.log"); len(got) != 0 {
				t.Errorf("%s: heo was called: %v", name, got)
			}
		}
		// the guard mark is not needed for what does not delete
		e := heoHighEnv(t)
		if r := act(e, noGuard(one(`{"verb": "read", "key": "k1"}, {"verb": "unsubscribe", "key": "k2"}`))); r.exit != 0 {
			t.Errorf("read + unsubscribe without a guard: exit %d\n%s", r.exit, r.err)
		}
	})

	t.Run("unsubscribe is informational: it never fails the item and the trash after it runs", func(t *testing.T) {
		for name, setup := range map[string]func(*env){
			"accepted":         func(e *env) {},
			"refused by heo":   func(e *env) { setScratch(t, e, "heo-delivery-unsubscribe", "failed") },
			"submit conflict":  func(e *env) { e.scratchFlag("heo-submit-fail-unsubscribe", true) },
			"outcome unknown":  func(e *env) { setScratch(t, e, "heo-delivery-unsubscribe", "outcome_unknown") },
			"submit unknown":   func(e *env) { e.scratchFlag("heo-submit-unknown-unsubscribe", true) },
			"delivery timeout": func(e *env) { setScratch(t, e, "heo-delivery-unsubscribe", "queued") },
		} {
			e := heoHighEnv(t, "FLO_HEO_SETTLE_TIMEOUT=1")
			setup(e)
			r := act(e, one(`{"verb": "unsubscribe", "key": "k-u"}, {"verb": "trash", "key": "k-t"}`))
			if r.exit != 0 || r.out["status"] != "ok" {
				t.Errorf("%s: exit %d %v\n%s", name, r.exit, r.out, r.err)
				continue
			}
			note, _ := r.out["note"].(string)
			if !strings.HasPrefix(note, "unsubscribe ") {
				t.Errorf("%s: note %q", name, note)
			}
			if got := heoCalls(e, "conversation unsubscribe "); len(got) != 1 || !strings.Contains(got[0], "--message msg_h2 ") {
				t.Errorf("%s: unsubscribe calls %v", name, got)
			}
			if len(heoCalls(e, "conversation trash ")) != 1 {
				t.Errorf("%s: the trash did not run after the unsubscribe", name)
			}
		}
	})

	t.Run("reply: a draft and a send, the script's text, recipients as planned, the key replays", func(t *testing.T) {
		e := heoHighEnv(t)
		draft := `{"verb": "reply", "key": "k-r", "mode": "draft", "text": "Thanks, on it.\n", "reply_all": true, "cc": ["lead@client.example"]}`
		r := act(e, strings.Replace(one(draft), "msg_h2", "msg_h5", 1))
		if r.exit != 0 || r.out["status"] != "ok" {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		got := heoCalls(e, "message reply-draft ")
		if len(got) != 1 || !strings.Contains(got[0], "c_msg_h5 --text Thanks, on it.") || !strings.Contains(got[0], "--reply-all") ||
			!strings.Contains(got[0], "--cc lead@client.example") || !strings.Contains(got[0], "--idempotency-key k-r") {
			t.Errorf("draft: %v", got)
		}
		e = heoHighEnv(t)
		send := `{"verb": "reply", "key": "k-s", "mode": "send", "text": "Thanks, on it.\n"}`
		in := strings.Replace(one(send), "msg_h2", "msg_h6", 1)
		act(e, in)
		act(e, in) // a rerun of the step: the same key, heo replays it, one operation
		sent := heoCalls(e, "message reply ")
		if len(sent) != 2 || keysOf(sent)[0] != keysOf(sent)[1] || keysOf(sent)[0] != "k-s" {
			t.Errorf("send: %v", sent)
		}
		for _, l := range sent {
			if strings.Contains(l, "--reply-all") || strings.Contains(l, "--cc") {
				t.Errorf("recipients beyond the plan: %s", l)
			}
		}
		ops, _ := os.ReadDir(filepath.Join(e.scratch, "heo-ops"))
		n := 0
		for _, f := range ops {
			if strings.HasSuffix(f.Name(), ".verb") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d operations queued for one send", n)
		}
	})

	t.Run("a reply is not sent when a newer message is in the conversation", func(t *testing.T) {
		e := heoHighEnv(t)
		r := act(e, strings.Replace(one(`{"verb": "reply", "key": "k-s", "mode": "send", "text": "Hi\n"}`), "msg_h2", "msg_h10", 1))
		if r.exit != 3 || r.out["status"] != "failed" || len(heoCalls(e, "message reply ")) != 0 {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if !strings.Contains(r.out["results"].([]any)[0].(map[string]any)["error"].(string), "newer message") {
			t.Errorf("the reason is not given: %v", r.out)
		}
	})

	t.Run("a move to an account that is not served fails, one that is runs the move", func(t *testing.T) {
		e := heoHighEnv(t)
		r := act(e, one(`{"verb": "move_to", "key": "k", "account": "nowhere", "mailbox": "Finance"}`))
		if r.exit != 3 || len(writes(e)) != 0 {
			t.Errorf("exit %d writes %v", r.exit, writes(e))
		}
		e = heoHighEnv(t)
		e.scratchFlag("heo-copy-never", true)
		r = act(e, strings.Replace(one(`{"verb": "read", "key": "k0"}, {"verb": "move_to", "key": "k", "account": "gm2", "mailbox": "Finance"}, {"verb": "archive", "key": "k1"}`), "msg_h2", "msg_h7", 1))
		if r.exit != 0 || r.out["status"] != "held" || !strings.Contains(r.out["reason"].(string), "did not appear") {
			t.Fatalf("a held move: exit %d %v\n%s", r.exit, r.out, r.err)
		}
		if indexOfCall(e, "conversation archive") >= 0 || indexOfCall(e, "conversation trash") >= 0 {
			t.Error("an op ran after the held move")
		}
	})

	t.Run("the low mode still has no destructive verb", func(t *testing.T) {
		e := heoHighEnv(t)
		r := wrap(t, e, one(`{"verb": "trash", "key": "k"}`), "heo-conversation-act")
		if r.exit != 64 || len(logOf(e, "heo.log")) != 0 {
			t.Errorf("the low act took a trash: exit %d", r.exit)
		}
	})

	t.Run("malformed high ops are refused before any call", func(t *testing.T) {
		for name, ops := range map[string]string{
			"reply no text":   `{"verb": "reply", "key": "k", "mode": "send"}`,
			"reply no mode":   `{"verb": "reply", "key": "k", "text": "x"}`,
			"cc flag":         `{"verb": "reply", "key": "k", "mode": "send", "text": "x", "cc": ["--bcc"]}`,
			"cc two":          `{"verb": "reply", "key": "k", "mode": "send", "text": "x", "cc": ["a@b.example@c"]}`,
			"move no mailbox": `{"verb": "move_to", "key": "k", "account": "gm2"}`,
			"unknown verb":    `{"verb": "delete", "key": "k"}`,
		} {
			e := heoHighEnv(t)
			if r := act(e, one(ops)); r.exit != 64 {
				t.Errorf("%s: exit %d, want 64\n%s", name, r.exit, r.err)
			}
			if got := logOf(e, "heo.log"); len(got) != 0 {
				t.Errorf("%s: heo was called: %v", name, got)
			}
		}
	})
}

const highAccounts = `heo_accounts=[{"account":"gm1","inbox":"ib-gm1","email":"me@gm1.example"},{"account":"gm2","inbox":"ib-gm2","email":"me@gm2.example"},{"account":"ol","inbox":"ib-ol","email":"me@ol.example"}]`

func TestInboxTriageApplyHighThroughHeo(t *testing.T) {
	start := func(t *testing.T, flags ...string) (*env, *engine, created) {
		t.Helper()
		e := triageEnv(t, "FLO_HEO_POLL=0.01", "FLO_HEO_SETTLE_TIMEOUT=2", "FLO_HEO_ARRIVAL_POLL=0.01", "FLO_HEO_ARRIVAL_TIMEOUT=1")
		e.inboxFlag("heo-items", true)
		e.inboxFlag("heo-high", true)
		for _, f := range flags {
			if strings.HasPrefix(f, "ops-") { // a switch of the fake script
				e.inboxFlag(f, true)
			}
		}
		e.triageActive()
		e.scratchFlag("heo-writes", true)
		setScratch(t, e, "heo-copy-after", "1")
		for _, f := range flags {
			if !strings.HasPrefix(f, "ops-") {
				e.scratchFlag(f, true)
			}
		}
		eng := e.startEngine()
		var c created
		r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var", highAccounts)
		if r.Exit != core.ExitAwaiting {
			t.Fatalf("run: exit %d, %+v\n%s", r.Exit, c, r.Stderr)
		}
		return e, eng, c
	}
	approveAll := func(e *env, c created) {
		t.Helper()
		var d struct{ Resumed bool }
		e.okJSON(&d, "approval", "set", c.WaitingOn[0], "--all=approve", "--yes")
	}
	end := func(e *env, c created) {
		t.Helper()
		eventually(t, "the run to end", func() bool { s := e.show(c.Run).Status; return s == "failed" || s == "succeeded" })
	}
	recordDoc := func(e *env) (doc struct {
		Results map[string]any    `json:"results"`
		Held    map[string]string `json:"held"`
		Notes   map[string]string `json:"notes"`
	}) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "record-doc.json")) //nolint:gosec // the fake's state
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	t.Run("a send waits for an approval; nothing is written before it", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		if len(writes(e)) != 0 {
			t.Errorf("heo was written before any approval: %v", writes(e))
		}
		// the reply send is pending with its guard: no trust can approve it
		_, plan := e.stepOf(c.Run, "plan")
		pend, _ := plan["pending"].([]any)
		found := false
		for _, p := range pend {
			m := p.(map[string]any)
			if fmt.Sprint(m["n"]) == "6" {
				found = true
				if m["guard"] != "send proposed by the unattended decider" {
					t.Errorf("send guard: %v", m["guard"])
				}
				if cl, _ := m["class"].([]any); len(cl) != 0 {
					t.Errorf("a guarded decision has a trust class: %v", cl)
				}
			}
		}
		if !found {
			t.Errorf("the send is not pending: %v", pend)
		}
		var d struct{ Resumed bool }
		e.okJSON(&d, "approval", "set", c.WaitingOn[0], "--all=reject", "--yes")
		end(e, c)
		// the automatic item (trust decided it) ran; nothing the approval was asked about did
		for _, w := range writes(e) {
			if !strings.Contains(w, "msg_h1") {
				t.Errorf("a rejected plan wrote to heo: %s", w)
			}
		}
	})

	t.Run("approved: every kind of op goes through heo on its message; the vendor item and the forward-less account never do", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		approveAll(e, c)
		end(e, c)
		all := strings.Join(logOf(e, "heo.log"), "\n")
		for _, want := range []string{
			"heo conversation read c_msg_h1 --message msg_h1 ", "heo conversation archive c_msg_h1 --message msg_h1 ",
			"heo conversation trash c_msg_h2 --message msg_h2 ",
			"heo conversation move c_msg_h3 --role junk --message msg_h3 ",
			"heo conversation unsubscribe c_msg_h4 --message msg_h4 ", "heo conversation trash c_msg_h4 --message msg_h4 ",
			"heo message reply-draft c_msg_h5 --text Thanks, on it.",
			"heo message reply c_msg_h6 --text Thanks, on it.",
			"heo message forward msg_h7 --to me@gm2.example ",
			"heo conversation trash c_msg_h7 --message msg_h7 ",
		} {
			if !strings.Contains(all, want) {
				t.Errorf("missing heo call %q in\n%s", want, all)
			}
		}
		// every state write is one message; replies and the forward name their own target
		for _, w := range writes(e) {
			if strings.HasPrefix(w, "heo conversation ") && !strings.Contains(w, " --message ") {
				t.Errorf("a conversation write without --message: %s", w)
			}
			if strings.Contains(w, "--references") {
				t.Errorf("--references: %s", w)
			}
		}
		// the vendor item (9) was refused by the script: heo never saw it; the item on the account that cannot forward (8) neither
		for _, never := range []string{"msg_h9", "c_msg_h9", "msg_h8", "c_msg_h8"} {
			if strings.Contains(all, never) {
				t.Errorf("heo was called for %s:\n%s", never, all)
			}
		}
		// the source of the move goes last: after the copy is found and filed
		msgFwd, find := indexOfCall(e, "message forward msg_h7"), indexOfCall(e, "message list --inbox ib-gm2 --message-id")
		if msgFwd >= find || find >= indexOfCall(e, "conversation tag c_copy_") ||
			indexOfCall(e, "conversation archive c_copy_") >= indexOfCall(e, "conversation trash c_msg_h7") {
			t.Errorf("order of the move:\n%s", all)
		}
		doc := recordDoc(e)
		for _, n := range []string{"1", "2", "3", "4", "5", "6", "7"} {
			if v, ok := doc.Results[n]; !ok || v != nil {
				t.Errorf("item %s: %v %v", n, v, ok)
			}
		}
		if v, _ := doc.Results["9"].(string); !strings.HasPrefix(v, "never delete:") {
			t.Errorf("item 9 (vendor): %v", doc.Results["9"])
		}
		if v, _ := doc.Results["8"].(string); !strings.Contains(v, "message forward") {
			t.Errorf("item 8 (forward unsupported on outlook): %v", doc.Results["8"])
		}
		if len(doc.Held) != 0 {
			t.Errorf("held: %v", doc.Held)
		}
		if n := doc.Notes["4"]; !strings.HasPrefix(n, "unsubscribe ") {
			t.Errorf("the unsubscribe outcome is not in the note: %q", n)
		}
		// the plan asked for the headers of the two replies, headers only
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "facts.json")) //nolint:gosec // the fake's state
		if err != nil {
			t.Fatal(err)
		}
		var facts struct {
			Accounts map[string]struct {
				Raw map[string]string `json:"raw"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(b, &facts); err != nil {
			t.Fatal(err)
		}
		raw := facts.Accounts["gm1"].Raw
		if !strings.Contains(raw["msg_h5"], "Cc: lead@client.example") || !strings.Contains(raw["msg_h6"], "Reply-To: attacker@elsewhere.example") || len(raw) != 2 {
			t.Errorf("raw headers for the replies: %v", raw)
		}
		if strings.Contains(string(b), "never kept") {
			t.Error("a body is in the plan facts")
		}
	})

	t.Run("a deleting item the script did not clear never reaches heo, even approved (the flow filter)", func(t *testing.T) {
		e, eng, c := start(t, "ops-unguarded")
		defer eng.stop()
		approveAll(e, c)
		end(e, c)
		for _, bad := range []string{"conversation trash", "conversation move", "message forward", "message list --inbox ib-gm2 --message-id"} {
			if indexOfCall(e, bad) >= 0 {
				t.Errorf("%s ran for an item without the guard mark:\n%s", bad, strings.Join(logOf(e, "heo.log"), "\n"))
			}
		}
		// what does not delete still runs
		if indexOfCall(e, "message reply-draft c_msg_h5") < 0 || indexOfCall(e, "conversation read c_msg_h1") < 0 {
			t.Error("non-deleting items did not run")
		}
		doc := recordDoc(e)
		for _, n := range []string{"2", "3", "4", "7"} {
			if h := doc.Held[n]; !strings.Contains(h, "not cleared") && !strings.Contains(h, "cleared") {
				t.Errorf("item %s is not held with its reason: %v", n, doc.Held)
			}
			if _, ok := doc.Results[n]; ok {
				t.Errorf("item %s has a result", n)
			}
		}
		if st := e.stepState(c.Run, "act-high"); st == "failed" {
			t.Errorf("act-high failed: the filter should leave the item out, not run it into the wrapper's refusal (%s)", st)
		}
	})

	t.Run("the copy does not appear: the item is held, the source is untouched, the rest of the run goes on", func(t *testing.T) {
		e, eng, c := start(t, "heo-copy-never")
		defer eng.stop()
		approveAll(e, c)
		end(e, c)
		if indexOfCall(e, "conversation trash c_msg_h7") >= 0 || indexOfCall(e, "conversation tag c_copy_") >= 0 {
			t.Errorf("the source was trashed or a copy filed with no copy:\n%s", strings.Join(logOf(e, "heo.log"), "\n"))
		}
		doc := recordDoc(e)
		if h := doc.Held["7"]; !strings.Contains(h, "did not appear") || !strings.Contains(h, "source kept") {
			t.Errorf("item 7 held: %v", doc.Held)
		}
		if _, ok := doc.Results["7"]; ok {
			t.Error("a held move has a result")
		}
		if v, ok := doc.Results["2"]; !ok || v != nil {
			t.Errorf("the other items did not complete: %v", doc.Results)
		}
		if rec := e.runFile(c, "receipt.json"); !strings.Contains(string(rec), `"held: forwarded to the destination`) {
			t.Errorf("receipt: %s", rec)
		}
	})

	t.Run("the destination label fails: the source is kept, the item is failed with where the copy is", func(t *testing.T) {
		e, eng, c := start(t, "heo-submit-fail-tag")
		defer eng.stop()
		approveAll(e, c)
		end(e, c)
		if indexOfCall(e, "conversation trash c_msg_h7") >= 0 {
			t.Error("the source was trashed though the copy was not labelled")
		}
		doc := recordDoc(e)
		if v, _ := doc.Results["7"].(string); !strings.Contains(v, "could not be labelled") {
			t.Errorf("item 7: %v", doc.Results["7"])
		}
	})

	t.Run("an outcome heo cannot confirm on a trash parks the run: nothing recorded", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		setScratch(t, e, "heo-delivery-trash", "outcome_unknown")
		approveAll(e, c)
		eventually(t, "the run to fail", func() bool { return e.show(c.Run).Status == "failed" })
		if st := e.stepState(c.Run, "record"); st != "skipped" {
			t.Errorf("record is %q, want skipped", st)
		}
		if n := len(heoCalls(e, "conversation trash c_msg_h2 ")); n != 1 {
			t.Errorf("the trash of msg_h2 was submitted %d times", n)
		}
	})
}
