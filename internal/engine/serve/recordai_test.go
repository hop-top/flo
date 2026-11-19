//go:build !windows

package serve_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// model is the live HTTP seam of a recording whose only caller is a model.
type model struct {
	mu    sync.Mutex
	paths []string
}

func (m *model) Do(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	m.paths = append(m.paths, req.URL.Path)
	m.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(rec, `{"message":{"role":"assistant","content":"hello"},"done":true,"prompt_eval_count":3,"eval_count":1}`)
	r := rec.Result()
	r.Request = req
	return r, nil
}

func TestARecordedRunKeepsTheModelCallsOfItsBuiltins(t *testing.T) {
	root := t.TempDir()
	live := &model{}
	e, err := serve.Open(bg, serve.Config{Root: root, Owner: "jad", NoSync: true, HTTP: live, Generator: "llm", ModelsAllow: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	r := &rig{root: root, e: e, live: &liveExec{t: t}}
	v := r.apply(t, "ai-say")
	run := r.startRecorded(t, "ai-say", v.Hash)
	if run.Status != core.RunSucceeded {
		t.Fatalf("the recorded run: %s", run.Status)
	}
	if len(live.paths) != 1 || !strings.HasSuffix(live.paths[0], "/api/chat") {
		t.Fatalf("live model calls = %v", live.paths)
	}
	cass, _ := e.Recordings.Cassettes(core.DefaultTenant, run.ID)
	kept, _ := filepath.Glob(filepath.Join(cass, "http-*.req.yaml"))
	if len(kept) != 1 {
		ents, _ := os.ReadDir(cass)
		t.Fatalf("the model call is not on the tape: %d http cassettes, dir %v", len(kept), ents)
	}
	meta, _, _ := e.Recordings.Load(core.DefaultTenant, run.ID)
	if meta.Calls != 1 {
		t.Errorf("meta.Calls = %d, want 1", meta.Calls)
	}
}
