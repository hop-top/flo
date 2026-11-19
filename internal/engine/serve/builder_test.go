//go:build !windows

package serve_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hop.top/flo/internal/builder"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// chatModel is the engine's HTTP seam with a scripted ollama behind it: the
// daemon lists gemma4:e4b and other, and /api/chat answers the next reply.
// Nothing reaches a network.
type chatModel struct {
	mu      sync.Mutex
	replies []string
	paths   []string
	bodies  []string // the /api/chat request bodies
}

func (m *chatModel) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	m.mu.Lock()
	m.paths = append(m.paths, req.URL.Path)
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	switch req.URL.Path {
	case "/api/tags":
		_, _ = io.WriteString(rec, `{"models":[{"name":"gemma4:e4b","model":"gemma4:e4b"},{"name":"other","model":"other"}]}`)
	default:
		m.bodies = append(m.bodies, string(body))
		text := "no reply scripted"
		if len(m.replies) > 0 {
			text, m.replies = m.replies[0], m.replies[1:]
		}
		b, _ := json.Marshal(map[string]any{
			"message": map[string]any{"role": "assistant", "content": text},
			"done":    true, "prompt_eval_count": 30, "eval_count": 10,
		})
		_, _ = rec.Write(b)
	}
	m.mu.Unlock()
	r := rec.Result()
	r.Request = req
	return r, nil
}

func (m *chatModel) chats() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.bodies) }
func (m *chatModel) calls() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.paths) }

func yamlReply(doc string) string { return "Here is the new version.\n```yaml\n" + doc + "```\n" }

type builderRig struct {
	*rig
	model *chatModel
	base  string // the applied document
}

func newBuilderRig(t *testing.T, cfg func(*serve.Config)) *builderRig {
	t.Helper()
	root := t.TempDir()
	m := &chatModel{}
	live := &liveExec{t: t}
	c := serve.Config{Root: root, Owner: "jad", NoSync: true, Exec: live, HTTP: m}
	if cfg != nil {
		cfg(&c)
	}
	e, err := serve.Open(bg, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	return &builderRig{rig: &rig{root: root, e: e, live: live}, model: m}
}

func (b *builderRig) applyBase(t *testing.T) core.FlowVersion {
	t.Helper()
	v := b.apply(t, "dependency-watch", "vcs.read")
	b.base = string(v.Source)
	return v
}

// next is the base document at the next version, with edits.
func (b *builderRig) next(version string, edits ...string) string {
	doc := strings.Replace(b.base, "version: 0.1.0\n", "version: "+version+"\n", 1)
	for i := 0; i+1 < len(edits); i += 2 {
		doc = strings.Replace(doc, edits[i], edits[i+1], 1)
	}
	return doc
}

func (b *builderRig) create(t *testing.T, instruction string) (builder.Draft, error) {
	t.Helper()
	return b.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "dependency-watch", Instruction: instruction, Caller: "jad"})
}

func TestDraftValidOnFirstTry(t *testing.T) {
	r := newBuilderRig(t, nil)
	base := r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch"))}

	d, err := r.create(t, "rename the findings title to dep-watch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.ID, "drf_") || d.Flow != "dependency-watch" || d.Version != "0.1.1" || d.Base.Hash != base.Hash || d.Head != base.Hash {
		t.Fatalf("draft = %+v", d)
	}
	if !strings.Contains(d.Source, "title: dep-watch") || d.Hash == "" || d.Hash == base.Hash || len(d.Attempts) != 1 || d.Cost.Tokens == 0 {
		t.Fatalf("draft = %+v", d)
	}
	if d.Model != "ollama://gemma4:e4b" {
		t.Errorf("the default model is local ollama: %q", d.Model)
	}
	// the model saw the rules, the instruction and the base document
	if !strings.Contains(r.model.bodies[0], "You write flo flow documents") {
		t.Errorf("the system prompt did not reach the model: %s", r.model.bodies[0])
	}
	if body := r.model.bodies[0]; !strings.Contains(body, "rename the findings title") || !strings.Contains(body, "gh/advisories@1") {
		t.Errorf("request body lacks the instruction or the document: %s", body)
	}
	// a draft is not a version
	if vs, _ := r.e.Services.Flows.ListVersions(bg, core.DefaultTenant, "dependency-watch"); len(vs) != 1 {
		t.Fatalf("a draft stored a version: %d", len(vs))
	}
	got, err := r.e.GetDraft(bg, core.DefaultTenant, d.ID)
	if err != nil || got.Source != d.Source {
		t.Fatalf("show: %v", err)
	}
	if all, _ := r.e.ListDrafts(bg, core.DefaultTenant, "dependency-watch"); len(all) != 1 {
		t.Fatalf("list = %d", len(all))
	}
}

func TestDraftInvalidThenFixed(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	broken := r.next("0.1.1", `results.advisories }}`, `results.nosuchstep }}`)
	fixed := r.next("0.1.1")
	r.model.replies = []string{yamlReply(broken), yamlReply(fixed)}

	d, err := r.create(t, "change nothing much")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Attempts) != 2 || len(d.Attempts[0].Errors) == 0 || len(d.Attempts[1].Errors) != 0 {
		t.Fatalf("attempts = %+v", d.Attempts)
	}
	if r.model.chats() != 2 {
		t.Fatalf("model calls = %d", r.model.chats())
	}
	if retry := r.model.bodies[1]; !strings.Contains(retry, "nosuchstep") || !strings.Contains(retry, "refused") {
		t.Errorf("the errors did not go back to the model: %s", retry)
	}
	if d.Cost.Tokens != 80 {
		t.Errorf("cost = %+v: both calls count", d.Cost)
	}
}

func TestDraftInvalidThreeTimesFailsAndKeepsNothing(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	broken := yamlReply(r.next("0.1.1", `results.advisories }}`, `results.nosuchstep }}`))
	r.model.replies = []string{broken, broken, broken, broken, broken}

	_, err := r.create(t, "x")
	if !core.IsCode(err, core.CodeInvalidFlow) {
		t.Fatalf("err = %v, want invalid_flow", err)
	}
	if !strings.Contains(err.Error(), "nosuchstep") && !strings.Contains(err.Error(), "advisories") {
		// the message names the last errors
		t.Logf("err = %v", err)
	}
	if r.model.chats() != 4 {
		t.Fatalf("model calls = %d, want 1 proposal + 3 fixes", r.model.chats())
	}
	if all, _ := r.e.ListDrafts(bg, core.DefaultTenant, ""); len(all) != 0 {
		t.Fatalf("a failed build kept a draft: %v", all)
	}
}

func TestDraftDryRunOnTheLastRecording(t *testing.T) {
	r := newBuilderRig(t, nil)
	base := r.applyBase(t)
	run := r.startRecorded(t, "dependency-watch", base.Hash)
	if run.Status != core.RunSucceeded {
		t.Fatalf("recorded run: %s", run.Status)
	}
	liveCalls := r.live.count()

	// no behaviour change: the outcome matches the recording
	r.model.replies = []string{yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch"))}
	same, err := r.create(t, "rename")
	if err != nil {
		t.Fatal(err)
	}
	dr := same.DryRun
	if dr == nil || !dr.Ran || dr.Run != string(run.ID) || dr.Recorded.Hash != base.Hash || dr.Status != "succeeded" || !dr.Matches || dr.Replayed == 0 {
		t.Fatalf("dry run = %+v", dr)
	}

	// a step that no longer runs: the outcome differs
	r.model.replies = []string{yamlReply(r.next("0.1.2", "  - id: newrel\n    uses: flo/dedupe@1\n", "  - id: newrel\n    uses: flo/dedupe@1\n    when: \"{{ 1 == 2 }}\"\n"))}
	diff, err := r.create(t, "skip release dedupe")
	if err != nil {
		t.Fatal(err)
	}
	if dr := diff.DryRun; dr == nil || !dr.Ran || dr.Matches || !hasDiff(dr.Diffs, "newrel") {
		t.Fatalf("dry run = %+v", dr)
	}
	if r.live.count() != liveCalls {
		t.Errorf("the dry run reached the real world: %d -> %d live calls", liveCalls, r.live.count())
	}
	if runs, _ := r.e.Services.Runs.ListRuns(bg, core.DefaultTenant, core.RunFilter{}); len(runs) != 1 {
		t.Errorf("the dry run left runs behind: %d", len(runs))
	}
}

func hasDiff(diffs []string, step string) bool {
	for _, d := range diffs {
		if strings.Contains(d, step) {
			return true
		}
	}
	return false
}

func TestDraftWithoutARecordingSaysSo(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1"))}
	d, err := r.create(t, "x")
	if err != nil {
		t.Fatal(err)
	}
	if d.DryRun == nil || d.DryRun.Ran || !strings.Contains(d.DryRun.Reason, "recording") {
		t.Fatalf("dry run = %+v", d.DryRun)
	}
}

func TestApplyStoresTheDraftAsANewVersion(t *testing.T) {
	r := newBuilderRig(t, nil)
	base := r.applyBase(t)
	run := r.startRecorded(t, "dependency-watch", base.Hash)
	_ = run
	r.model.replies = []string{yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch"))}
	d, err := r.create(t, "rename")
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied.Created || res.Applied.Version.Hash != d.Hash || res.Applied.Version.Version != "0.1.1" || res.Applied.Version.State != core.FlowDraft {
		t.Fatalf("applied = %+v", res.Applied.Version)
	}
	if res.DryRun == nil || !res.DryRun.Ran {
		t.Fatalf("apply reports no dry run: %+v", res.DryRun)
	}
	vs, _ := r.e.Services.Flows.ListVersions(bg, core.DefaultTenant, "dependency-watch")
	if len(vs) != 2 || string(vs[0].Source) != d.Source {
		t.Fatalf("versions = %d", len(vs))
	}
	if _, err := r.e.GetDraft(bg, core.DefaultTenant, d.ID); !core.IsCode(err, core.CodeDraftNotFound) {
		t.Fatalf("an applied draft is spent: %v", err)
	}
}

func TestApplyOnAMovedBaseIsDraftStale(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch"))}
	d, err := r.create(t, "rename")
	if err != nil {
		t.Fatal(err)
	}
	// someone applies another version meanwhile
	moved := filepath.Join(t.TempDir(), "dependency-watch")
	copyTree(t, testdata("flows", "dependency-watch"), moved)
	p := filepath.Join(moved, "flow.yaml")
	b, _ := os.ReadFile(p) //nolint:gosec // a temp copy
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "version: 0.1.0\n", "version: 0.2.0\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	r.applyBundle(t, moved)

	for i := 0; i < 2; i++ { // check and apply agree
		var err error
		if i == 0 {
			_, err = r.e.CheckDraft(bg, core.DefaultTenant, d.ID)
		} else {
			_, err = r.e.ApplyDraft(bg, core.DefaultTenant, "jad", d.ID)
		}
		if !core.IsCode(err, core.CodeDraftStale) || core.Code(core.CodeDraftStale).Exit() != 4 {
			t.Fatalf("err = %v, want draft_stale", err)
		}
	}
	if vs, _ := r.e.Services.Flows.ListVersions(bg, core.DefaultTenant, "dependency-watch"); len(vs) != 2 {
		t.Fatalf("a stale apply stored a version: %d", len(vs))
	}
	if _, err := r.e.GetDraft(bg, core.DefaultTenant, d.ID); err != nil {
		t.Fatalf("a stale draft is kept for the person to read: %v", err)
	}
}

func TestTheBaseOfAnActiveFlowIsTheActiveVersionAndAnApplyMovesIt(t *testing.T) {
	r := newBuilderRig(t, nil)
	base := r.applyBase(t)
	if _, err := r.e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{Flow: "dependency-watch", Ref: base.Hash, Mode: "record", Caller: "jad", AllowLive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", base.Hash, core.FlowActive); err != nil {
		t.Fatal(err)
	}
	r.model.replies = []string{
		yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch")),
		yamlReply(r.next("0.1.2", "title: dependency-watch", "title: dw")),
	}
	first, err := r.create(t, "rename")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.create(t, "rename again")
	if err != nil {
		t.Fatal(err)
	}
	if first.Base.Hash != base.Hash || second.Base.Hash != base.Hash {
		t.Fatalf("the base of a flow with an active version is the active one: %+v", first.Base)
	}
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", first.ID); err != nil {
		t.Fatal(err)
	}
	// the first apply moved the newest version under the second draft
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", second.ID); !core.IsCode(err, core.CodeDraftStale) {
		t.Fatalf("err = %v, want draft_stale", err)
	}
}

func TestDraftOfAnUnknownFlow(t *testing.T) {
	r := newBuilderRig(t, nil)
	_, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "nope", Instruction: "x", Caller: "jad"})
	if !core.IsCode(err, core.CodeFlowNotFound) {
		t.Fatalf("err = %v", err)
	}
	if r.model.calls() != 0 {
		t.Error("the model was called")
	}
}

func TestDraftDiscard(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1"))}
	d, err := r.create(t, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.DiscardDraft(bg, core.DefaultTenant, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.e.DiscardDraft(bg, core.DefaultTenant, d.ID); !core.IsCode(err, core.CodeDraftNotFound) {
		t.Fatalf("discard twice: %v", err)
	}
}

// The model call goes through flo's model path: the allow and deny lists, the
// privacy ceiling and the secret store apply, and a refused call sends nothing.

func TestDraftModelAllowAndDenyLists(t *testing.T) {
	for name, cfg := range map[string]func(*serve.Config){
		"deny":  func(c *serve.Config) { c.ModelsDeny = []string{"gemma4:*"} },
		"allow": func(c *serve.Config) { c.ModelsAllow = []string{"ollama://other"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newBuilderRig(t, cfg)
			r.applyBase(t)
			_, err := r.create(t, "x")
			if err == nil || !strings.Contains(err.Error(), "gemma4:e4b") {
				t.Fatalf("err = %v, want the model refused", err)
			}
			if r.model.calls() != 0 {
				t.Errorf("a refused model was called: %v", r.model.paths)
			}
		})
	}
	// the allowed one works, named per request
	r := newBuilderRig(t, func(c *serve.Config) { c.ModelsAllow = []string{"ollama://other"}; c.ModelsDeny = []string{"gemma4:*"} })
	r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "dependency-watch", Instruction: "x", Model: "ollama://other", Caller: "jad"})
	if err != nil || d.Model != "ollama://other" {
		t.Fatalf("draft %+v err %v", d, err)
	}
}

func TestDraftHonoursTheConfiguredModel(t *testing.T) {
	r := newBuilderRig(t, func(c *serve.Config) { c.BuilderModel = "ollama://other" })
	r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1"))}
	d, err := r.create(t, "x")
	if err != nil || d.Model != "ollama://other" {
		t.Fatalf("draft %+v err %v", d, err)
	}
}

func TestDraftPrivacyCeiling(t *testing.T) {
	// ai-say is local-only: a cloud model is refused before anything is sent
	r := newBuilderRig(t, nil)
	r.apply(t, "ai-say")
	_, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "ai-say", Instruction: "x", Model: "anthropic://claude-sonnet", Caller: "jad"})
	if !core.IsCode(err, core.CodePrivacyViolation) {
		t.Fatalf("err = %v, want privacy_violation", err)
	}
	if r.model.calls() != 0 {
		t.Errorf("a refused call was sent: %v", r.model.paths)
	}
	// a local-only flow with a local model goes through
	r.model.replies = []string{yamlReply(strings.Replace(readFlow(t, "ai-say"), "version: 0.1.0\n", "version: 0.1.1\n", 1))}
	if _, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "ai-say", Instruction: "x", Caller: "jad"}); err != nil {
		t.Fatal(err)
	}
}

func readFlow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(testdata("flows", name, "flow.yaml")) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDraftCloudModelNeedsItsKeyInTheSecretStore(t *testing.T) {
	// dependency-watch is cloud-allowed: the ceiling passes, the key is read
	// as the caller's profile, and its absence is named
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	_, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "dependency-watch", Instruction: "x", Model: "anthropic://claude-sonnet", Caller: "jad"})
	if err == nil || !strings.Contains(err.Error(), "anthropic_api_key") {
		t.Fatalf("err = %v, want the missing secret named", err)
	}
	if r.model.calls() != 0 {
		t.Errorf("a call was sent without a key: %v", r.model.paths)
	}
}
