//go:build !windows

package serve_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// recordedDraft makes a draft on a flow with a recording; edits change the
// base document.
func (b *builderRig) recordedDraft(t *testing.T, version string, edits ...string) (id string, bundle string) {
	t.Helper()
	base := b.applyBase(t)
	if run := b.startRecorded(t, "dependency-watch", base.Hash); run.Status != core.RunSucceeded {
		t.Fatalf("recorded run: %s", run.Status)
	}
	b.model.replies = []string{yamlReply(b.next(version, edits...))}
	d, err := b.create(t, "change it")
	if err != nil {
		t.Fatal(err)
	}
	return d.ID, base.Bundle
}

func (b *builderRig) storedVersions(t *testing.T) int {
	t.Helper()
	vs, _ := b.e.Services.Flows.ListVersions(bg, core.DefaultTenant, "dependency-watch")
	return len(vs)
}

const (
	failingKey = "key: \"{{ item.ghsa_id }}\"\n      seen: \"{{ state.seen_adv }}\""
	brokenKey  = "key: \"{{ item.no_such_field }}\"\n      seen: \"{{ state.seen_adv }}\""
	otherKey   = "key: \"{{ item.html_url }}\"\n      seen: \"{{ state.seen_adv }}\""
	skipRel    = "  - id: newrel\n    uses: flo/dedupe@1\n"
)

func TestApplyBlockedByAFailedOrDifferentStep(t *testing.T) {
	for name, edit := range map[string][]string{
		"failed step":    {failingKey, brokenKey},
		"different step": {skipRel, skipRel + "    when: \"{{ 1 == 2 }}\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newBuilderRig(t, nil)
			id, _ := r.recordedDraft(t, "0.1.1", edit...)
			_, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", id)
			if !core.IsCode(err, core.CodeDraftDryRunFailed) || core.Code(core.CodeDraftDryRunFailed).Exit() != 4 {
				t.Fatalf("err = %v, want draft_dry_run_failed", err)
			}
			if !strings.Contains(err.Error(), "step") {
				t.Errorf("the message lists no blocking difference: %v", err)
			}
			if r.storedVersions(t) != 1 {
				t.Error("a blocked apply stored a version")
			}
			if _, err := r.e.GetDraft(bg, core.DefaultTenant, id); err != nil {
				t.Errorf("a blocked draft is kept: %v", err)
			}
			// --force passes and still reports what it overrode
			res, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", id, serve.ApplyDraftOptions{Force: true})
			if err != nil {
				t.Fatal(err)
			}
			if r.storedVersions(t) != 2 || len(res.Overridden) == 0 {
				t.Fatalf("stored %d, overridden %v", r.storedVersions(t), res.Overridden)
			}
		})
	}
}

func TestApplyBlockedByACallWithoutARecording(t *testing.T) {
	r := newBuilderRig(t, nil)
	id, _ := r.recordedDraft(t, "0.1.1", `repo: "{{ vars.repo }}"`, `repo: someone/else`)
	_, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", id)
	if !core.IsCode(err, core.CodeDraftDryRunFailed) || !strings.Contains(err.Error(), "recording") {
		t.Fatalf("err = %v, want draft_dry_run_failed naming the missing recording", err)
	}
	if r.storedVersions(t) != 1 {
		t.Error("a blocked apply stored a version")
	}
	if _, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", id, serve.ApplyDraftOptions{Force: true}); err != nil {
		t.Fatalf("force: %v", err)
	}
}

func TestApplyOutputOnlyDifferenceJustReports(t *testing.T) {
	r := newBuilderRig(t, nil)
	id, _ := r.recordedDraft(t, "0.1.1", failingKey, otherKey)
	res, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", id)
	if err != nil {
		t.Fatalf("an output-only difference blocks: %v", err)
	}
	if res.DryRun == nil || res.DryRun.Matches || len(res.DryRun.Diffs) == 0 || len(res.DryRun.Blocking) != 0 {
		t.Fatalf("dry run = %+v", res.DryRun)
	}
	if r.storedVersions(t) != 2 {
		t.Error("not stored")
	}
}

func TestApplyWithoutARecordingIsAllowed(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	r.model.replies = []string{yamlReply(r.next("0.1.1"))}
	d, err := r.create(t, "x")
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", d.ID)
	if err != nil || res.DryRun == nil || res.DryRun.Ran || res.DryRun.Reason == "" {
		t.Fatalf("res %+v err %v: nothing to compare is not a block, and says why", res.DryRun, err)
	}
}

func TestApplyWriteRewritesTheBundleFile(t *testing.T) {
	r := newBuilderRig(t, nil)
	id, bundle := r.recordedDraft(t, "0.1.1", "title: dependency-watch", "title: dep-watch")
	file := filepath.Join(bundle, "flow.yaml")
	if err := os.Chmod(file, 0o640); err != nil { //nolint:gosec // a temp copy, mode kept by the write
		t.Fatal(err)
	}
	res, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", id, serve.ApplyDraftOptions{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file) //nolint:gosec // a temp copy
	if string(got) != res.Draft.Source || res.Written != file {
		t.Fatalf("file not rewritten (written %q)", res.Written)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(bundle); len(ents) != 2 { // flow.yaml and nodes/: no temp file left
		t.Errorf("bundle holds %d entries", len(ents))
	}
}

func TestApplyWithoutWriteLeavesTheFile(t *testing.T) {
	r := newBuilderRig(t, nil)
	id, bundle := r.recordedDraft(t, "0.1.1", "title: dependency-watch", "title: dep-watch")
	before, _ := os.ReadFile(filepath.Join(bundle, "flow.yaml")) //nolint:gosec // a temp copy
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", id); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(bundle, "flow.yaml")) //nolint:gosec // a temp copy
	if string(before) != string(after) {
		t.Fatal("apply without --write changed the file")
	}
}

func TestApplyWriteRefusesAFileChangedSinceTheDraft(t *testing.T) {
	r := newBuilderRig(t, nil)
	id, bundle := r.recordedDraft(t, "0.1.1", "title: dependency-watch", "title: dep-watch")
	file := filepath.Join(bundle, "flow.yaml")
	edited := []byte(r.base + "# edited by hand\n")
	if err := os.WriteFile(file, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", id, serve.ApplyDraftOptions{Write: true})
	if !core.IsCode(err, core.CodeDraftStale) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v, want draft_stale naming the changed file", err)
	}
	if got, _ := os.ReadFile(file); string(got) != string(edited) { //nolint:gosec // a temp copy
		t.Error("the changed file was overwritten")
	}
	if r.storedVersions(t) != 1 {
		t.Error("a refused write still stored a version")
	}
	if _, err := r.e.GetDraft(bg, core.DefaultTenant, id); err != nil {
		t.Errorf("draft not kept: %v", err)
	}
	// without --write the same draft applies: the file is not touched
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", id); err != nil {
		t.Fatalf("apply without write: %v", err)
	}
}

func TestApplyWriteRefusesAMissingFile(t *testing.T) {
	r := newBuilderRig(t, nil)
	id, bundle := r.recordedDraft(t, "0.1.1", "title: dependency-watch", "title: dep-watch")
	if err := os.Remove(filepath.Join(bundle, "flow.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", id, serve.ApplyDraftOptions{Write: true})
	if !core.IsCode(err, core.CodeDraftStale) {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(bundle, "flow.yaml")); serr == nil {
		t.Error("a missing file was recreated")
	}
}

// a step that failed in the recorded run and fails the same way in the replay
// is a failed step all the same: the draft does not fix it.
func TestApplyBlockedByAStepThatFailsInBothRuns(t *testing.T) {
	r := newBuilderRig(t, nil)
	bundle := filepath.Join(t.TempDir(), "dependency-watch")
	copyTree(t, testdata("flows", "dependency-watch"), bundle)
	p := filepath.Join(bundle, "flow.yaml")
	b, _ := os.ReadFile(p) //nolint:gosec // a temp copy
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), failingKey, brokenKey, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	base := r.applyBundle(t, bundle, "vcs.read")
	r.base = string(base.Source)
	if run := r.startRecorded(t, "dependency-watch", base.Hash); run.Status != core.RunFailed {
		t.Fatalf("recorded run: %s, want failed", run.Status)
	}
	r.model.replies = []string{yamlReply(r.next("0.1.1", "title: dependency-watch", "title: dep-watch"))}
	d, err := r.create(t, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", d.ID); !core.IsCode(err, core.CodeDraftDryRunFailed) || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want draft_dry_run_failed", err)
	}
}
