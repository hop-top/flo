//go:build !windows

package serve_test

import (
	"strings"
	"testing"

	"hop.top/flo/internal/builder"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

func (b *builderRig) refine(from, instruction string) (builder.Draft, error) {
	return b.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{From: from, Instruction: instruction, Caller: "jad"})
}

func TestRefineSendsHistoryAndKeepsTheBase(t *testing.T) {
	r := newBuilderRig(t, nil)
	base := r.applyBase(t)
	r.model.replies = []string{
		yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch")),
		yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dw")),
		yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dw2")),
	}
	first, err := r.create(t, "rename the title to dep-watch")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.refine(first.ID, "shorter please")
	if err != nil {
		t.Fatal(err)
	}
	// the model got the original base, the previous draft, the history and the new instruction
	body := r.model.bodies[1]
	for _, want := range []string{"gh/advisories@1", "title: dep-watch", "rename the title to dep-watch", "shorter please", "previous draft"} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %q: %s", want, body)
		}
	}
	if !strings.Contains(body, "version 0.1.0") {
		t.Errorf("the base is not the original version: %s", body)
	}
	if second.Parent != first.ID || second.Base != first.Base || second.Base.Hash != base.Hash || second.Head != first.Head {
		t.Fatalf("second = %+v", second)
	}
	if len(second.History) != 1 || second.History[0] != "rename the title to dep-watch" || second.Instruction != "shorter please" {
		t.Fatalf("history = %v", second.History)
	}
	if !strings.Contains(second.Source, "title: dw") {
		t.Errorf("source = %s", second.Source)
	}
	// the earlier draft is kept; the version is still the same next one
	if _, err := r.e.GetDraft(bg, core.DefaultTenant, first.ID); err != nil {
		t.Errorf("the parent is kept: %v", err)
	}
	// a third refinement carries both earlier instructions, oldest first
	third, err := r.refine(second.ID, "and again")
	if err != nil {
		t.Fatal(err)
	}
	if len(third.History) != 2 || third.History[0] != "rename the title to dep-watch" || third.History[1] != "shorter please" || third.Parent != second.ID {
		t.Fatalf("history = %v", third.History)
	}
	// the refined draft applies like any other
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", third.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRefineOnAMovedBaseIsDraftStale(t *testing.T) {
	r2 := newBuilderRig(t, nil)
	r2.applyBase(t)
	r2.model.replies = []string{yamlReply(r2.next("0.1.1", "title: dependency-watch", "title: a")), yamlReply(r2.next("0.1.2", "title: dependency-watch", "title: b"))}
	a, err := r2.create(t, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r2.refine(a.ID, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.e.ApplyDraft(bg, core.DefaultTenant, "jad", a.ID); err != nil { // the parent applies: the base moved under b
		t.Fatal(err)
	}
	if _, err := r2.refine(b.ID, "c"); !core.IsCode(err, core.CodeDraftStale) {
		t.Fatalf("refine on a moved base: %v, want draft_stale", err)
	}
	if _, err := r2.e.ApplyDraft(bg, core.DefaultTenant, "jad", b.ID); !core.IsCode(err, core.CodeDraftStale) {
		t.Fatalf("apply on a moved base: %v, want draft_stale", err)
	}
	if r2.model.chats() != 2 {
		t.Errorf("a stale refine called the model: %d", r2.model.chats())
	}
}

func TestRefineOfAMissingDraftOrWithNew(t *testing.T) {
	r := newBuilderRig(t, nil)
	if _, err := r.refine("drf_nope", "x"); !core.IsCode(err, core.CodeDraftNotFound) {
		t.Fatalf("err = %v", err)
	}
	r.model.replies = []string{yamlReply(newDoc("greeter", "0.1.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "greeter", New: true, Instruction: "x", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{From: d.ID, New: true, Instruction: "x", Caller: "jad"}); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("--new with --from: %v", err)
	}
}

// refining a new flow's draft stays a new flow, under the ceiling it was made with.
func TestRefineANewFlowDraft(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.model.replies = []string{yamlReply(newDoc("greeter", "0.1.0", "local-only")), yamlReply(newDoc("greeter", "0.2.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "greeter", New: true, Instruction: "say hello", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := r.refine(d.ID, "twice")
	if err != nil {
		t.Fatal(err)
	}
	if !d2.New || d2.Parent != d.ID || d2.Ceiling != "local-only" || d2.Flow != "greeter" || d2.Version != "0.2.0" {
		t.Fatalf("d2 = %+v", d2)
	}
	if body := r.model.bodies[1]; !strings.Contains(body, "say hello") || !strings.Contains(body, "twice") || strings.Contains(body, "Current flow") {
		t.Errorf("body: %s", body)
	}
}
