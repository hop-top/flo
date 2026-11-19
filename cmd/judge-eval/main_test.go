package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tool end to end, on synthetic mail (testdata/judgeeval) and a fake
// ollama on loopback: no model, no cloud.

func fakeOllama(t *testing.T, value string) string {
	t.Helper()
	const model = "qwen3:4b-instruct-2507-q4_K_M"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, _ *http.Request) {
		answer, _ := json.Marshal(map[string]any{"label": map[string]any{"value": value, "confidence": 0.95, "why": "fake"}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": model, "done": true, "done_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": string(answer)},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return "ollama://" + strings.TrimPrefix(srv.URL, "http://") + "/" + model
}

func exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestImportRunAndGate(t *testing.T) {
	flow := filepath.Join("..", "..", "flows", "inbox-triage", "flow.yaml")
	runDir := filepath.Join("..", "..", "testdata", "judgeeval", "run")
	set := filepath.Join(t.TempDir(), "label.jsonl")

	code, out, errOut := exec(t, "import", "--flow", flow, "--step", "label", "--out", set, runDir)
	if code != 0 || !strings.Contains(out, "7 cases written") {
		t.Fatalf("import: %d\n%s\n%s", code, out, errOut)
	}

	// a model that files everything under Finance agrees on two cases of seven
	model := fakeOllama(t, "Finance")
	code, out, errOut = exec(t, "run", "--cases", set, "--model", model, "--min-agreement", "0.9", "--at-confidence", "0")
	if code != exitGate || !strings.Contains(out, "FAIL") || !strings.Contains(out, "agreement") {
		t.Fatalf("run against a strict gate: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "| label | choice | 7 |") {
		t.Errorf("no report:\n%s", out)
	}
	// no gate asked: the report is printed and the run passes
	code, out, errOut = exec(t, "run", "--cases", set, "--model", model, "--at-confidence", "0")
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("run without a gate: %d\n%s\n%s", code, out, errOut)
	}
	// JSON report
	code, out, _ = exec(t, "run", "--cases", set, "--model", model, "--json", "--at-confidence", "0")
	if code != 0 || !strings.Contains(out, `"questions"`) {
		t.Fatalf("json: %d\n%s", code, out)
	}
}

func TestRefusals(t *testing.T) {
	flow := filepath.Join("..", "..", "flows", "inbox-triage", "flow.yaml")
	runDir := filepath.Join("..", "..", "testdata", "judgeeval", "run")
	// real mail never lands in this repository
	inRepo := filepath.Join("..", "..", "testdata", "judgeeval", "set.jsonl")
	if code, _, errOut := exec(t, "import", "--flow", flow, "--out", inRepo, runDir); code == 0 || !strings.Contains(errOut, "outside any git work tree") {
		t.Errorf("import into the repository: %d %s", code, errOut)
	}
	if _, err := os.Stat(inRepo); err == nil {
		_ = os.Remove(inRepo)
		t.Error("a set was written into the repository")
	}
	set := filepath.Join(t.TempDir(), "label.jsonl")
	if code, _, errOut := exec(t, "import", "--flow", flow, "--out", set, runDir); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	// Jev is a hosted service: no flag, no call
	if code, _, errOut := exec(t, "run", "--cases", set, "--jev"); code == 0 || !strings.Contains(errOut, "--allow-cloud") {
		t.Errorf("--jev without --allow-cloud: %d %s", code, errOut)
	}
	// a cloud model is refused before any request, whatever the flags
	if code, _, errOut := exec(t, "run", "--cases", set, "--model", "anthropic://claude-sonnet-5-5"); code == 0 || errOut == "" {
		t.Errorf("a cloud model was accepted: %d", code)
	}
	if code, _, _ := exec(t, "run", "--cases", set); code == 0 {
		t.Error("no candidate was accepted")
	}
}
