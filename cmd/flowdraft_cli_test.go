//go:build !windows

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// scriptedOllama is the engine's HTTP seam: it lists gemma4:e4b and answers
// /api/chat with the next scripted reply.
type scriptedOllama struct {
	mu      sync.Mutex
	replies []string
	chats   int
}

func (m *scriptedOllama) Do(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	if req.URL.Path == "/api/tags" {
		_, _ = io.WriteString(rec, `{"models":[{"name":"gemma4:e4b","model":"gemma4:e4b"}]}`)
	} else {
		m.chats++
		text := "nothing scripted"
		if len(m.replies) > 0 {
			text, m.replies = m.replies[0], m.replies[1:]
		}
		b, _ := json.Marshal(map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "done": true})
		_, _ = rec.Write(b)
	}
	r := rec.Result()
	r.Request = req
	return r, nil
}

func fence(doc string) string { return "```yaml\n" + doc + "```\n" }

// draftRig is a flowRig whose engine has a scripted model behind it.
type draftRig struct {
	*flowRig
	model *scriptedOllama
	base  string
}

func newDraftRig(t *testing.T) *draftRig {
	t.Helper()
	r := newFlowRig(t)
	_ = r.eng.Close(context.Background())
	m := &scriptedOllama{}
	eng, err := serve.Open(context.Background(), serve.Config{Root: t.TempDir(), Owner: "jad", NoSync: true, Exec: r.live, HTTP: m})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	r.eng = eng
	if _, err := eng.Grants.Set(context.Background(), core.DefaultTenant, "automation", "vcs.read"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.bundle, "flow.yaml")) //nolint:gosec // a temp copy
	if err != nil {
		t.Fatal(err)
	}
	d := &draftRig{flowRig: r, model: m, base: string(b)}
	if _, _, err := r.run("flow", "apply", "-f", r.bundle, "--yes"); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *draftRig) doc(version string) string {
	return strings.Replace(d.base, "version: 0.1.0\n", "version: "+version+"\n", 1)
}

// create makes a draft through the CLI and returns its id.
func (d *draftRig) create(t *testing.T, version string) string {
	t.Helper()
	d.model.replies = append(d.model.replies, fence(d.doc(version)))
	out, _, err := d.run("flow", "draft", "create", "dependency-watch", "bump", "the", "version", "--format", "json")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var got struct {
		ID, Version string
	}
	if err := json.Unmarshal([]byte(strings.ToLower(strings.NewReplacer(`"ID"`, `"id"`).Replace(out))), &got); err != nil || got.ID == "" {
		t.Fatalf("create output: %v\n%s", err, out)
	}
	return got.ID
}

func TestDraftCreateShowListApply(t *testing.T) {
	d := newDraftRig(t)
	id := d.create(t, "0.1.1")
	if !strings.HasPrefix(id, "drf_") {
		t.Fatalf("id = %q", id)
	}
	if d.stored() != 1 {
		t.Fatal("create stored a flow version")
	}

	out, _, err := d.run("flow", "draft", "list", "--format", "json")
	if err != nil || !strings.Contains(out, id) {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, _, err = d.run("flow", "draft", "show", id, "--format", "json")
	if err != nil || !strings.Contains(out, "version: 0.1.1") {
		t.Fatalf("show: %v\n%s", err, out)
	}

	// apply is a write: it asks, and --dry-run plans
	_, _, err = d.run("flow", "draft", "apply", id)
	wantCode(t, err, core.CodeConfirmationRequired)
	if d.stored() != 1 {
		t.Fatal("an unconfirmed apply stored a version")
	}
	out, _, err = d.run("flow", "draft", "apply", id, "--dry-run")
	if err != nil || !strings.Contains(out, "dependency-watch") || d.stored() != 1 {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	out, _, err = d.run("flow", "draft", "apply", id, "--yes", "--format", "json")
	if err != nil || d.stored() != 2 || !strings.Contains(out, `"0.1.1"`) {
		t.Fatalf("apply: %v (stored %d)\n%s", err, d.stored(), out)
	}
	_, _, err = d.run("flow", "draft", "show", id)
	wantCode(t, err, core.CodeDraftNotFound)
}

func TestDraftApplyOnAMovedBaseIsDraftStale(t *testing.T) {
	d := newDraftRig(t)
	id := d.create(t, "0.1.1")
	other := d.doc("0.2.0")
	if err := os.WriteFile(filepath.Join(d.bundle, "flow.yaml"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.run("flow", "apply", "-f", d.bundle, "--yes"); err != nil {
		t.Fatal(err)
	}
	_, _, err := d.run("flow", "draft", "apply", id, "--yes")
	wantCode(t, err, core.CodeDraftStale)
	if ExitCode(err) != 4 || d.stored() != 2 {
		t.Fatalf("exit %d, stored %d", ExitCode(err), d.stored())
	}
	_, _, err = d.run("flow", "draft", "apply", id, "--dry-run")
	wantCode(t, err, core.CodeDraftStale)
}

func TestDraftCreateFailsAfterThreeFixes(t *testing.T) {
	d := newDraftRig(t)
	bad := fence(strings.Replace(d.doc("0.1.1"), "results.advisories }}", "results.nope }}", 1))
	d.model.replies = []string{bad, bad, bad, bad}
	_, _, err := d.run("flow", "draft", "create", "dependency-watch", "break it")
	wantCode(t, err, core.CodeInvalidFlow)
	if d.model.chats != 4 {
		t.Fatalf("model calls = %d", d.model.chats)
	}
	out, _, _ := d.run("flow", "draft", "list", "--format", "json")
	if strings.Contains(out, "drf_") {
		t.Fatalf("a failed build left a draft: %s", out)
	}
}

func TestDraftCreateReadsTheInstructionFromStdin(t *testing.T) {
	d := newDraftRig(t)
	d.model.replies = []string{fence(d.doc("0.1.1"))}
	root := NewRootOver(func(context.Context) (*serve.Engine, error) { return d.eng, nil })
	root.Cmd.SetIn(strings.NewReader("make it better\n"))
	var out strings.Builder
	root.Cmd.SetOut(&out)
	root.SetArgs([]string{"flow", "draft", "create", "dependency-watch", "-"})
	if err := root.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.model.chats != 1 {
		t.Fatal("no model call")
	}
}

func TestDraftDiscardAsksAndDeletes(t *testing.T) {
	d := newDraftRig(t)
	id := d.create(t, "0.1.1")
	_, _, err := d.run("flow", "draft", "discard", id)
	wantCode(t, err, core.CodeConfirmationRequired)
	promptSource = kitcli.PromptSourceFromReadWriter(strings.NewReader("y\n"), io.Discard)
	if _, _, err := d.run("flow", "draft", "discard", id); err != nil {
		t.Fatal(err)
	}
	_, _, err = d.run("flow", "draft", "discard", id, "--yes")
	wantCode(t, err, core.CodeDraftNotFound)
	_, _, err = d.run("flow", "draft", "show", "nonsense")
	wantCode(t, err, core.CodeUsage)
}
