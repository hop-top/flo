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

// newDoc is a small flow of the name, as a model would write it.
func newDoc(name, version, privacy string) string {
	return "flow: " + name + "\nversion: " + version + "\nprivacy: " + privacy + "\nsteps:\n  - id: say\n    uses: flo/output@1\n    with:\n      kind: findings\n      title: hello\n      items: []\n"
}

func TestNewFlowDraftApplyStoresTheFirstVersion(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.model.replies = []string{yamlReply(newDoc("greeter", "0.1.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "greeter", New: true, Instruction: "say hello", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.New || d.Flow != "greeter" || d.Version != "0.1.0" || d.Ceiling != "local-only" || d.Base.Hash != "" {
		t.Fatalf("draft = %+v", d)
	}
	if d.DryRun == nil || d.DryRun.Ran || !strings.Contains(d.DryRun.Reason, "no recording") {
		t.Fatalf("dry run = %+v: a new flow has nothing to replay on, and says so", d.DryRun)
	}
	if body := r.model.bodies[0]; !strings.Contains(body, "New flow to write, named") || !strings.Contains(body, "say hello") || strings.Contains(body, "Current flow") {
		t.Errorf("request body: %s", body)
	}
	res, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied.Created || res.Applied.Version.Name != "greeter" || res.Applied.Version.State != core.FlowDraft {
		t.Fatalf("applied = %+v", res.Applied)
	}
}

func TestNewFlowNameClash(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.applyBase(t)
	_, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "dependency-watch", New: true, Instruction: "x", Caller: "jad"})
	if !core.IsCode(err, core.CodeDuplicate) {
		t.Fatalf("err = %v, want duplicate", err)
	}
	if r.model.calls() != 0 {
		t.Error("the model was called")
	}
	// taken between the draft and the apply: draft_stale
	r.model.replies = []string{yamlReply(newDoc("late", "0.1.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "late", New: true, Instruction: "x", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "late")
	if err := os.MkdirAll(bundle, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "flow.yaml"), []byte(newDoc("late", "0.0.1", "local-only")), 0o600); err != nil {
		t.Fatal(err)
	}
	r.applyBundle(t, bundle)
	if _, err := r.e.ApplyDraft(bg, core.DefaultTenant, "jad", d.ID); !core.IsCode(err, core.CodeDraftStale) {
		t.Fatalf("err = %v, want draft_stale", err)
	}
	// a bad name
	if _, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "Bad Name", New: true, Instruction: "x", Caller: "jad"}); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("err = %v, want usage", err)
	}
}

func TestNewFlowMustCarryTheNameItWasGiven(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.model.replies = []string{yamlReply(newDoc("other", "0.1.0", "local-only")), yamlReply(newDoc("greeter", "0.1.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "greeter", New: true, Instruction: "x", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Attempts) != 2 || !strings.Contains(r.model.bodies[1], "keep the name") {
		t.Fatalf("attempts %+v", d.Attempts)
	}
}

func TestNewFlowPrivacyCeiling(t *testing.T) {
	r := newBuilderRig(t, nil)
	// the default ceiling is local-only: a cloud model is refused before a byte is sent
	_, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "g", New: true, Instruction: "x", Model: "anthropic://claude-sonnet", Caller: "jad"})
	if !core.IsCode(err, core.CodePrivacyViolation) {
		t.Fatalf("err = %v, want privacy_violation", err)
	}
	// cloud-allowed lifts it (the key is then the next thing asked for)
	_, err = r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "g", New: true, Instruction: "x", Model: "anthropic://claude-sonnet", Privacy: "cloud-allowed", Caller: "jad"})
	if err == nil || !strings.Contains(err.Error(), "anthropic_api_key") {
		t.Fatalf("err = %v, want the missing key named", err)
	}
	if r.model.calls() != 0 {
		t.Errorf("a refused call was sent: %v", r.model.paths)
	}
	// the document's own privacy is validated: a cloud model's flow cannot say local-only and use a cloud node
	if _, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "g", New: true, Instruction: "x", Privacy: "bogus", Caller: "jad"}); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("err = %v, want usage", err)
	}
	if _, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "g", Instruction: "x", Privacy: "cloud-allowed", Caller: "jad"}); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("--privacy on an existing flow: %v", err)
	}
}

func TestNewFlowWriteMakesABundle(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.model.replies = []string{yamlReply(newDoc("greeter", "0.1.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "greeter", New: true, Instruction: "x", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", d.ID, serve.ApplyDraftOptions{Write: true, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "greeter", "flow.yaml")
	got, _ := os.ReadFile(file) //nolint:gosec // a temp dir
	if res.Written != file || string(got) != d.Source {
		t.Fatalf("written %q: %s", res.Written, got)
	}
	if res.Applied.Version.Bundle != filepath.Join(dir, "greeter") {
		t.Errorf("bundle = %q", res.Applied.Version.Bundle)
	}
}

func TestNewFlowWriteRefusesAnExistingBundle(t *testing.T) {
	r := newBuilderRig(t, nil)
	r.model.replies = []string{yamlReply(newDoc("greeter", "0.1.0", "local-only"))}
	d, err := r.e.CreateDraft(bg, core.DefaultTenant, serve.DraftRequest{Flow: "greeter", New: true, Instruction: "x", Caller: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "greeter"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.ApplyDraftWith(bg, core.DefaultTenant, "jad", d.ID, serve.ApplyDraftOptions{Write: true, Dir: dir}); !core.IsCode(err, core.CodeDuplicate) {
		t.Fatalf("err = %v, want duplicate", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "greeter", "flow.yaml")); err == nil {
		t.Error("an existing directory was written into")
	}
	if _, err := r.e.GetDraft(bg, core.DefaultTenant, d.ID); err != nil {
		t.Errorf("draft not kept: %v", err)
	}
	vs, _ := r.e.Services.Flows.ListVersions(bg, core.DefaultTenant, "greeter")
	if len(vs) != 0 {
		t.Error("a refused write stored a version")
	}
}
