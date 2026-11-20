//go:build !windows

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// Apply through heo, the low-blast ops only: the script says what to do (inbox/ops@1), heo/conversation.act@1 and
// heo/provider-label.ensure@1 do it, inbox/record@1 commits it. The wrappers run for real over a fake heo
// (testdata/inbox/heoshims, writes on) and a fake script; no daemon, no account, no token. What these tests pin:
// every write names the one message, a rerun repeats the same keys, only `security` is ever created, an outcome
// heo cannot confirm parks the run, and nothing destructive reaches heo.

func heoActEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	needJQ(t)
	e := newEnv(t, append([]string{"FLO_HEO_POLL=0.01", "FLO_HEO_SETTLE_TIMEOUT=2"}, extra...)...)
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
	e.scratchFlag("heo-writes", true)
	return e
}

func TestHeoApplyNodesDeclareHonestEffects(t *testing.T) {
	heo := loadPackNodes(t, "heo")
	for _, id := range []string{"heo/conversation.act@1", "heo/provider-label.ensure@1"} {
		n := heo[id]
		if n == nil {
			t.Fatalf("%s is not in the heo pack", id)
		}
		if n.SideEffect != node.SideEffectIrreversible || n.Approval != "required" || n.Privacy != node.PrivacyLocal ||
			n.Idempotent == nil || !*n.Idempotent || !slices.Equal(n.Capabilities, []string{"mail.write"}) || n.Timeout == "" {
			t.Errorf("%s: irreversible, approval required, local, idempotent, mail.write: %+v", id, n)
		}
		// 70 (no daemon) and 6 are retried; 3 is data; 8 (outcome unknown) is neither: the step fails, nothing is retried
		if n.ExitCodes == nil || !slices.Equal(n.ExitCodes.Retry, []int{6, 70}) || !slices.Equal(n.ExitCodes.OK, []int{0, 3}) {
			t.Errorf("%s exit codes: %+v", id, n.ExitCodes)
		}
	}
	// the verbs the node accepts: six, none destructive; the one label it may create is security
	verbs := heo["heo/conversation.act@1"].Inputs["ops"].Items.Properties["verb"].Enum
	if len(verbs) != 6 {
		t.Errorf("act verbs: %v", verbs)
	}
	for _, v := range verbs {
		if slices.Contains([]string{"trash", "spam", "unsubscribe", "reply", "move_to", "send", "move"}, fmt.Sprint(v)) {
			t.Errorf("act accepts the verb %v", v)
		}
	}
	if l := heo["heo/provider-label.ensure@1"].Inputs["label"].Enum; len(l) != 1 || l[0] != "security" {
		t.Errorf("ensure label enum: %v", l)
	}
	if n := heo["heo/conversation.act@1"].Inputs["provider_id"]; !n.Required.Flag {
		t.Error("act requires provider_id")
	}
	in := loadPackNodes(t, "inbox")
	if n := in["inbox/ops@1"]; n == nil || n.SideEffect != node.SideEffectWriteLocal {
		t.Errorf("inbox/ops@1: %+v", n)
	}
	if n := in["inbox/record@1"]; n == nil || n.SideEffect != node.SideEffectWriteLocal || n.Idempotent == nil || !*n.Idempotent {
		t.Errorf("inbox/record@1: %+v", n)
	}
}

// writes are the heo calls that change something.
func writes(e *env) []string {
	var out []string
	for _, l := range logOf(e, "heo.log") {
		if (strings.HasPrefix(l, "heo conversation ") && !strings.HasPrefix(l, "heo conversation list ") && !strings.HasPrefix(l, "heo conversation get ")) ||
			strings.HasPrefix(l, "heo provider-label create ") {
			out = append(out, l)
		}
	}
	return out
}

var keyRe = regexp.MustCompile(`--idempotency-key (\S+)`)

func keysOf(ls []string) []string {
	var out []string
	for _, l := range ls {
		if m := keyRe.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

const actStdin = `{"n": 1, "account": "acme", "channel": "acme", "provider_id": "msg_1", "ops": [
  {"verb": "read", "key": "k-read"}, {"verb": "label", "label": "Finance", "key": "k-label"}, {"verb": "archive", "key": "k-arch"}]}`

func TestHeoConversationActWrapper(t *testing.T) {
	act := func(e *env, stdin string) wrapRun { return wrap(t, e, stdin, "heo-conversation-act") }

	t.Run("every write names the one message, a rerun replays", func(t *testing.T) {
		e := heoActEnv(t)
		r := act(e, actStdin)
		if r.exit != 0 || r.out["status"] != "ok" {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		ws := writes(e)
		if len(ws) != 3 {
			t.Fatalf("writes: %v", ws)
		}
		for _, w := range ws {
			if !strings.Contains(w, "--message msg_1 ") || !strings.Contains(w, "c_msg_1") {
				t.Errorf("a write that does not name the message: %s", w)
			}
		}
		if !strings.Contains(ws[0], "conversation read c_msg_1") || !strings.Contains(ws[1], "conversation tag c_msg_1 --add Finance") ||
			!strings.Contains(ws[2], "conversation archive c_msg_1") {
			t.Errorf("order: read, label, archive last: %v", ws)
		}
		if got := keysOf(ws); !slices.Equal(got, []string{"k-read", "k-label", "k-arch"}) {
			t.Errorf("keys: %v", got)
		}
		res, _ := r.out["results"].([]any)
		if len(res) != 3 || res[0].(map[string]any)["replayed"] != false {
			t.Errorf("results: %v", res)
		}
		// the same input again: the same keys, heo says replayed
		r = act(e, actStdin)
		if r.exit != 0 {
			t.Fatalf("rerun: exit %d\n%s", r.exit, r.err)
		}
		if got := keysOf(writes(e)); !slices.Equal(got, []string{"k-read", "k-label", "k-arch", "k-read", "k-label", "k-arch"}) {
			t.Errorf("keys across the rerun: %v", got)
		}
		for _, x := range r.out["results"].([]any) {
			if x.(map[string]any)["replayed"] != true {
				t.Errorf("a rerun is a replay: %v", x)
			}
		}
	})

	t.Run("nothing destructive, malformed or untargeted reaches heo", func(t *testing.T) {
		for name, stdin := range map[string]string{
			"trash":       `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "trash", "key": "k"}]}`,
			"spam":        `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "read", "key": "k1"}, {"verb": "spam", "key": "k"}]}`,
			"unsubscribe": `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "unsubscribe", "key": "k"}]}`,
			"reply":       `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "reply", "key": "k"}]}`,
			"move_to":     `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "move_to", "key": "k"}]}`,
			"no key":      `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "read"}]}`,
			"no message":  `{"account": "acme", "ops": [{"verb": "read", "key": "k"}]}`,
			"empty id":    `{"account": "acme", "provider_id": "", "ops": [{"verb": "archive", "key": "k"}]}`,
			"label list":  `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "label", "label": "A,B", "key": "k"}]}`,
			"label flag":  `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "label", "label": "--role", "key": "k"}]}`,
			"no label":    `{"account": "acme", "provider_id": "msg_1", "ops": [{"verb": "label", "key": "k"}]}`,
		} {
			e := heoActEnv(t)
			if r := act(e, stdin); r.exit != 64 {
				t.Errorf("%s: exit %d, want 64\n%s", name, r.exit, r.err)
			}
			if got := logOf(e, "heo.log"); len(got) != 0 {
				t.Errorf("%s: heo was called: %v", name, got)
			}
		}
	})

	t.Run("a failed op is data and nothing runs after it", func(t *testing.T) {
		e := heoActEnv(t)
		e.scratchFlag("heo-delivery-tag", true)
		if err := os.WriteFile(filepath.Join(e.scratch, "heo-delivery-tag"), []byte("failed"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := act(e, actStdin)
		if r.exit != 3 || r.out["status"] != "failed" {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		res := r.out["results"].([]any)
		var st []string
		for _, x := range res {
			st = append(st, x.(map[string]any)["status"].(string))
		}
		if !slices.Equal(st, []string{"ok", "failed", "skipped"}) {
			t.Errorf("statuses: %v", st)
		}
		for _, w := range writes(e) {
			if strings.Contains(w, "conversation archive") {
				t.Errorf("the archive ran after a failed label: %s", w)
			}
		}
		if e := res[1].(map[string]any)["error"].(string); !strings.Contains(e, "refused") {
			t.Errorf("the failure carries heo's reason: %q", e)
		}
	})

	t.Run("submit refused by heo (CONFLICT) fails the op, the message is gone (NOT_FOUND) fails them all", func(t *testing.T) {
		e := heoActEnv(t)
		e.scratchFlag("heo-submit-fail-read", true)
		if r := act(e, actStdin); r.exit != 3 || len(writes(e)) != 0 && !strings.Contains(strings.Join(writes(e), ""), "conversation read") {
			t.Errorf("conflict: exit %d %v", r.exit, writes(e))
		}
		e = heoActEnv(t)
		r := act(e, strings.Replace(actStdin, "msg_1", "msg_404", 1))
		if r.exit != 3 || len(writes(e)) != 0 {
			t.Errorf("not found: exit %d, writes %v", r.exit, writes(e))
		}
	})

	t.Run("an outcome heo cannot confirm parks it: exit 8, the operation recorded", func(t *testing.T) {
		for name, setup := range map[string]func(*env){
			"delivery outcome_unknown": func(e *env) {
				_ = os.WriteFile(filepath.Join(e.scratch, "heo-delivery-archive"), []byte("outcome_unknown"), 0o600)
			},
			"dead lettered": func(e *env) {
				_ = os.WriteFile(filepath.Join(e.scratch, "heo-delivery-archive"), []byte("dead_lettered"), 0o600)
			},
			"never settles": func(e *env) {
				_ = os.WriteFile(filepath.Join(e.scratch, "heo-delivery-archive"), []byte("queued"), 0o600)
			},
			"submit exit 8": func(e *env) { e.scratchFlag("heo-submit-unknown-archive", true) },
		} {
			e := heoActEnv(t, "FLO_HEO_SETTLE_TIMEOUT=1")
			setup(e)
			r := act(e, actStdin)
			if r.exit != 8 || r.out["status"] != "outcome_unknown" {
				t.Errorf("%s: exit %d %v\n%s", name, r.exit, r.out["status"], r.err)
				continue
			}
			res := r.out["results"].([]any)
			last := res[len(res)-1].(map[string]any)
			if last["status"] != "outcome_unknown" || last["verb"] != "archive" {
				t.Errorf("%s: %v", name, last)
			}
			if name != "submit exit 8" && (last["operation"] == nil || !strings.Contains(r.err, last["operation"].(string))) {
				t.Errorf("%s: the operation id is recorded and on stderr: %v / %s", name, last, r.err)
			}
		}
	})

	t.Run("an outcome unknown is never retried by the node: exit 8 is not an ok or retry code", func(t *testing.T) {
		n := loadPackNodes(t, "heo")["heo/conversation.act@1"]
		if slices.Contains(n.ExitCodes.Retry, 8) || slices.Contains(n.ExitCodes.OK, 8) {
			t.Errorf("exit 8: %+v", n.ExitCodes)
		}
	})

	t.Run("daemon down: 70, retried, nothing written", func(t *testing.T) {
		e := heoActEnv(t)
		e.scratchFlag("heo-down", true)
		if r := act(e, actStdin); r.exit != 70 {
			t.Errorf("exit %d", r.exit)
		}
		if len(writes(e)) != 0 {
			t.Errorf("writes: %v", writes(e))
		}
	})
}

func TestHeoLabelEnsureWrapper(t *testing.T) {
	ens := func(e *env, label string) wrapRun {
		return wrap(t, e, "", "heo-label-ensure", "--run", "20261118-090001", "--channel", "acme", "--account", "acme", "--label", label)
	}
	t.Run("security: created once, settled, listed; a second run finds it", func(t *testing.T) {
		e := heoActEnv(t)
		r := ens(e, "security")
		if r.exit != 0 || r.out["status"] != "ok" || r.out["created"] != true || r.out["id"] != "Label_new" || r.out["account"] != "acme" {
			t.Fatalf("exit %d %v\n%s", r.exit, r.out, r.err)
		}
		ws := writes(e)
		if len(ws) != 1 || !strings.HasPrefix(ws[0], "heo provider-label create security --channel acme --idempotency-key inbox-") {
			t.Errorf("writes: %v", ws)
		}
		r = ens(e, "security")
		if r.exit != 0 || r.out["created"] != false || r.out["id"] != "Label_new" || len(writes(e)) != 1 {
			t.Errorf("second run: exit %d %v, writes %v", r.exit, r.out, writes(e))
		}
	})
	t.Run("the key is sha256(run|provider-label-create|channel|name|), as the script derives it", func(t *testing.T) {
		e := heoActEnv(t)
		ens(e, "security")
		// printf 'run|provider-label-create|acme|security|' | shasum -a 256 | cut -c1-24
		if got := keysOf(writes(e)); len(got) != 1 || got[0] != "inbox-"+sha24("20261118-090001|provider-label-create|acme|security|") {
			t.Errorf("key: %v", got)
		}
	})
	t.Run("no other label is ever created", func(t *testing.T) {
		for _, name := range []string{"Finance", "Waiting", "Totally New", "Security", "security ", "", "a,b", "--kind"} {
			e := heoActEnv(t)
			if r := ens(e, name); r.exit != 64 {
				t.Errorf("%q: exit %d", name, r.exit)
			}
			if got := logOf(e, "heo.log"); len(got) != 0 {
				t.Errorf("%q: heo was called: %v", name, got)
			}
		}
	})
	t.Run("a create that fails is data; one that does not settle parks", func(t *testing.T) {
		e := heoActEnv(t)
		_ = os.WriteFile(filepath.Join(e.scratch, "heo-delivery-label-create"), []byte("rejected"), 0o600)
		if r := ens(e, "security"); r.exit != 3 || r.out["status"] != "failed" {
			t.Errorf("rejected: exit %d %v", r.exit, r.out)
		}
		e = heoActEnv(t, "FLO_HEO_SETTLE_TIMEOUT=1")
		_ = os.WriteFile(filepath.Join(e.scratch, "heo-delivery-label-create"), []byte("queued"), 0o600)
		r := ens(e, "security")
		if r.exit != 8 || r.out["status"] != "outcome_unknown" || r.out["operation"] == nil {
			t.Errorf("never settles: exit %d %v", r.exit, r.out)
		}
		e = heoActEnv(t)
		e.scratchFlag("heo-submit-unknown-label-create", true)
		if r := ens(e, "security"); r.exit != 8 {
			t.Errorf("submit exit 8: %d", r.exit)
		}
	})
}

// TestInboxTriageApplyThroughHeo: inbox-triage with a heo account, the approved items applied through heo.
func TestInboxTriageApplyThroughHeo(t *testing.T) {
	const heoAccounts = `heo_accounts=[{"account":"acme","inbox":"ib-acme","email":"me@acme.example"}]`

	start := func(t *testing.T, flags ...string) (*env, *engine, created) {
		t.Helper()
		e := triageEnv(t, "FLO_HEO_POLL=0.01", "FLO_HEO_SETTLE_TIMEOUT=2")
		e.inboxFlag("heo-items", true)
		e.triageActive() // records a test of the bundle on the default scenario: scratch flags are set after it
		e.scratchFlag("heo-writes", true)
		for _, f := range flags {
			e.scratchFlag(f, true)
		}
		eng := e.startEngine()
		var c created
		r := e.jsonOf(&c, "run", "create", "--flow", "inbox-triage", "--wait", "--var", heoAccounts)
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
	scriptCalls := func(e *env) (ops, legacy, records []string) {
		for _, l := range e.inboxLog() {
			switch {
			case strings.HasPrefix(l, "apply ") && strings.Contains(l, " --ops "):
				ops = append(ops, l)
			case strings.HasPrefix(l, "apply "):
				legacy = append(legacy, l)
			case strings.HasPrefix(l, "record "):
				records = append(records, l)
			}
		}
		return
	}

	t.Run("low-blast ops go through heo, a trash the channel cannot do fails its item, other accounts keep the script's path", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		approveAll(e, c)
		eventually(t, "the run to end", func() bool { s := e.show(c.Run).Status; return s == "failed" || s == "succeeded" })

		// heo: every write targets one message; nothing destructive; only security was created
		ws := writes(e)
		if len(ws) == 0 {
			t.Fatal("nothing went through heo")
		}
		for _, w := range ws {
			switch {
			case strings.HasPrefix(w, "heo provider-label create "):
				if !strings.HasPrefix(w, "heo provider-label create security ") {
					t.Errorf("a label other than security: %s", w)
				}
			case !strings.Contains(w, " --message msg_"):
				t.Errorf("a write that does not name a message: %s", w)
			}
			for _, bad := range []string{" trash ", " spam ", " move ", "unsubscribe", " reply", " send", " forward", " delete "} {
				if strings.Contains(w, bad) {
					t.Errorf("a destructive or high-blast call reached heo: %s", w)
				}
			}
		}
		all := strings.Join(logOf(e, "heo.log"), "\n")
		if strings.Contains(all, "c_msg_9") {
			t.Errorf("the trash item was written in heo:\n%s", all)
		}
		if n := strings.Count(all, "heo provider-label create security"); n != 1 {
			t.Errorf("security created %d times", n)
		}
		for _, w := range []string{
			"conversation read c_msg_1 --message msg_1 ", "conversation archive c_msg_1 --message msg_1 ",
			"conversation tag c_msg_3 --add security --message msg_3 ", "conversation archive c_msg_3 --message msg_3 ",
			"conversation read c_msg_2 --message msg_2 ", "conversation tag c_msg_2 --add Finance --message msg_2 ", "conversation archive c_msg_2 --message msg_2 ",
		} {
			if !strings.Contains(all, "heo "+w) {
				t.Errorf("missing heo call %q in\n%s", w, all)
			}
		}
		// the security label is created before the message is tagged with it
		if strings.Index(all, "provider-label create security") > strings.Index(all, "conversation tag c_msg_3") {
			t.Errorf("the label is tagged before it is created:\n%s", all)
		}

		// the script: ops asked once, the heo account's items recorded, the legacy apply leaves acme alone
		ops, legacy, records := scriptCalls(e)
		if len(ops) != 1 || !strings.Contains(ops[0], "--account acme") || len(records) != 1 || len(legacy) != 1 ||
			!strings.Contains(legacy[0], "--exclude-account acme") {
			t.Fatalf("ops %v, record %v, apply %v", ops, records, legacy)
		}
		if got := e.applied(); !slices.Equal(got, []string{"applied " + m5}) {
			t.Errorf("the legacy apply took acme's items too, or missed beta's: %v", got)
		}
		b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "record-doc.json")) //nolint:gosec // the fake's state
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Results map[string]any    `json:"results"`
			Held    map[string]string `json:"held"`
			Created []map[string]any  `json:"labels_created"`
			Backend string            `json:"backend"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"1", "2", "6", "7"} { // auto, label+archive, attention (no op), label+archive
			if v, ok := doc.Results[n]; !ok || v != nil {
				t.Errorf("item %s: %v %v", n, v, ok)
			}
		}
		if v, ok := doc.Results["5"]; !ok || v == nil || !strings.Contains(v.(string), "not found") {
			t.Errorf("item 5 (message gone): %v", v)
		}
		// the trash is the high-blast act's now: acme's channel does not declare it, so the item fails before any write
		if v, _ := doc.Results["3"].(string); !strings.Contains(v, "does not declare conversation trash") {
			t.Errorf("item 3 (trash on a channel that cannot): %v", doc.Results["3"])
		}
		if len(doc.Held) != 0 {
			t.Errorf("held: %v", doc.Held)
		}
		if len(doc.Created) != 1 || doc.Created[0]["label"] != "security" || doc.Created[0]["account"] != "acme" || doc.Backend != "heo" {
			t.Errorf("labels created / backend: %+v %s", doc.Created, doc.Backend)
		}
		// the item whose trash the channel cannot do is marked failed, the receipt says why
		plan := e.runFile(c, "plan.json")
		var p struct {
			Items []struct {
				N        int            `json:"n"`
				Executed map[string]any `json:"executed"`
			} `json:"items"`
		}
		if err := json.Unmarshal(plan, &p); err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			if it.N == 3 && (it.Executed == nil || it.Executed["ok"] != false) {
				t.Errorf("item 3 is marked failed: %v", it.Executed)
			}
			if it.N == 2 && (it.Executed == nil || it.Executed["ok"] != true) {
				t.Errorf("item 2: %v", it.Executed)
			}
		}
		rec := e.runFile(c, "receipt.json")
		if !strings.Contains(string(rec), "does not declare conversation trash") {
			t.Errorf("receipt: %s", rec)
		}
		// the failed item (5) fails the run at the apply step, which reads the whole plan
		if e.show(c.Run).Status != "failed" {
			t.Errorf("a failed item fails the run: %s", e.show(c.Run).Status)
		}
	})

	t.Run("every op of the run has its own key, the script derives them", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		approveAll(e, c)
		eventually(t, "the run to end", func() bool { s := e.show(c.Run).Status; return s == "failed" || s == "succeeded" })
		first := keysOf(writes(e))
		if len(first) == 0 {
			t.Fatal("no writes")
		}
		// the script reproduces the ops of the same run: the keys are a function of run, verb, message, extra
		ops, _, _ := scriptCalls(e)
		if len(ops) != 1 {
			t.Fatalf("ops calls: %v", ops)
		}
		seen := map[string]bool{}
		for _, k := range first {
			if seen[k] {
				t.Errorf("a key used for two ops: %s", k)
			}
			seen[k] = true
		}
	})

	t.Run("an outcome heo cannot confirm parks the run: nothing is recorded, nothing else applied", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		_ = os.WriteFile(filepath.Join(e.scratch, "heo-delivery-archive"), []byte("outcome_unknown"), 0o600)
		approveAll(e, c)
		eventually(t, "the run to fail", func() bool { return e.show(c.Run).Status == "failed" })
		rec, _ := e.stepOf(c.Run, "act")
		_ = rec
		_, _, records := scriptCalls(e)
		_, legacy, _ := scriptCalls(e)
		if len(records) != 0 || len(legacy) != 0 || len(e.applied()) != 0 {
			t.Errorf("work went on after an unknown outcome: record %v, apply %v, applied %v", records, legacy, e.applied())
		}
		if st := e.stepState(c.Run, "record"); st != "skipped" {
			t.Errorf("record is %q, want skipped", st)
		}
		if st := e.stepState(c.Run, "apply"); st != "skipped" {
			t.Errorf("apply is %q, want skipped", st)
		}
		// the step was not retried: one archive write per message that got that far, not three
		n := 0
		for _, w := range writes(e) {
			if strings.Contains(w, "conversation archive c_msg_1 ") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the archive of msg_1 was submitted %d times", n)
		}
	})

	t.Run("heo down at apply: retried, nothing recorded", func(t *testing.T) {
		e, eng, c := start(t)
		defer eng.stop()
		e.scratchFlag("heo-down", true)
		approveAll(e, c)
		eventually(t, "the run to fail", func() bool { return e.show(c.Run).Status == "failed" })
		if len(writes(e)) != 0 {
			t.Errorf("writes with the daemon down: %v", writes(e))
		}
		if _, _, records := scriptCalls(e); len(records) != 0 {
			t.Errorf("recorded with nothing done: %v", records)
		}
	})

	t.Run("no heo account: none of the heo apply steps, the apply is the apply of before", func(t *testing.T) {
		e := triageEnv(t)
		e.triageActive()
		eng := e.startEngine()
		defer eng.stop()
		c, _ := e.startTriage()
		var d struct{ Resumed bool }
		e.okJSON(&d, "approval", "set", c.WaitingOn[0], "--all=approve", "--yes")
		eventually(t, "the run to end", func() bool { s := e.show(c.Run).Status; return s == "succeeded" || s == "failed" })
		for _, s := range []string{"ops", "ensure", "act", "record"} {
			if st := e.stepState(c.Run, s); st != "skipped" {
				t.Errorf("step %s: %q, want skipped", s, st)
			}
		}
		_, legacy, records := scriptCalls(e)
		if len(legacy) != 1 || legacy[0] != "apply "+c.runStamp()+" --no-task --json" || len(records) != 0 {
			t.Errorf("apply calls %v, record %v", legacy, records)
		}
		if n := len(logOf(e, "heo.log")); n != 0 {
			t.Errorf("heo was called with no heo account: %v", logOf(e, "heo.log"))
		}
	})
}

// runFile reads a file of the fake script's run directory (the stamp is the one the fake fetch gave).
func (e *env) runFile(c created, name string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.scratch, "inbox", "runs", c.runStamp(), name)) //nolint:gosec // the fake's state
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// runStamp is the run stamp of the one run the fake script made.
func (c created) runStamp() string { return "20261118-090001" }

// sha24 is the first 24 hex digits of the SHA-256 of s: the tail of an idempotency key.
func sha24(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:24]
}
