//go:build !windows

package serve_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

// tapeModel is the model of the recorded build: small and local.
const tapeModel = "ollama://qwen3:4b-instruct-2507-q4_K_M"

// TestDraftOnALocalModelTape replays a build recorded against a local ollama:
// the model's own answers, through the same client path as a flow's. It is
// hermetic; the tape is testdata/builder/tape.
//
// Re-record it on a host that runs ollama with the model pulled, after a change
// to what the builder sends (the prompt, the node catalog, the flow):
//
//	FLO_BUILDER_RECORD=1 go test ./internal/engine/serve -run TestDraftOnALocalModelTape
func TestDraftOnALocalModelTape(t *testing.T) {
	dir := testdata("builder", "tape")
	mode := flowtest.ModeReplay
	if os.Getenv("FLO_BUILDER_RECORD") == "1" {
		mode = flowtest.ModeRecord
	}
	tape, err := flowtest.NewTape(mode, dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := serve.Open(bg, serve.Config{
		Root: t.TempDir(), Owner: "jad", NoSync: true,
		HTTP: tape.HTTP(real.NewHTTP(nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	bundle := filepath.Join(t.TempDir(), "greeter")
	copyTree(t, testdata("builder", "greeter"), bundle)
	src, err := serve.ReadSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(bg, 10*time.Minute)
	defer cancel()
	d, err := e.CreateDraft(ctx, core.DefaultTenant, serve.DraftRequest{
		Flow: "greeter", Instruction: "Change the title of the hello step to greetings.", Model: tapeModel, Caller: "jad",
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if d.Version == "0.1.0" || d.Flow != "greeter" || d.Model != tapeModel || !strings.Contains(d.Source, "greetings") {
		t.Fatalf("draft = %+v", d)
	}
	if len(d.Attempts) == 0 || len(d.Attempts[len(d.Attempts)-1].Errors) != 0 {
		t.Fatalf("attempts = %+v", d.Attempts)
	}
	if calls, replayed := tape.Stats(); mode == flowtest.ModeReplay && (calls == 0 || calls != replayed) {
		t.Fatalf("tape: %d calls, %d replayed", calls, replayed)
	}
	t.Logf("attempts: %d, model tokens: %d", len(d.Attempts), d.Cost.Tokens)
}
