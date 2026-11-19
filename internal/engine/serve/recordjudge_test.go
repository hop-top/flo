//go:build !windows

package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"hop.top/kit/go/ai/judge"
)

// judgeNet is the live HTTP seam of a recording whose callers are the judge
// engines: an ollama daemon (listing models, answering chat) and Jev.
type judgeNet struct {
	mu     sync.Mutex
	models string
	calls  []string
}

func (n *judgeNet) Do(req *http.Request) (*http.Response, error) {
	n.mu.Lock()
	n.calls = append(n.calls, req.Method+" "+req.URL.Host+req.URL.Path)
	n.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	switch {
	case req.URL.Path == "/api/tags":
		_, _ = io.WriteString(rec, `{"models":[`+n.models+`]}`)
	case req.URL.Path == "/api/chat":
		_, _ = io.WriteString(rec, `{"message":{"role":"assistant","content":"{\"label\":{\"value\":\"routine\",\"confidence\":0.8,\"why\":\"patch\"}}"},"done":true,"prompt_eval_count":3,"eval_count":1}`)
	case req.URL.Host == "openrouter.ai":
		_, _ = io.WriteString(rec, `{"model":"typesafe/jev-1.14","answers":{"label":{"type":"choice","choice":"security","confidence":0.9,"probabilities":{"security":0.9,"breaking":0.04,"routine":0.03,"noise":0.03}}},"usage":{"input_tokens":50,"output_tokens":5,"cost":0.0002}}`)
	default:
		rec.WriteHeader(http.StatusNotFound)
	}
	r := rec.Result()
	r.Request = req
	return r, nil
}

func judgeRun(t *testing.T, net *judgeNet, engine string) (*rig, core.Run) {
	t.Helper()
	root := t.TempDir()
	e, err := serve.Open(bg, serve.Config{
		Root: root, Owner: "jad", NoSync: true, HTTP: net, Classifier: "judge", JudgeEngine: engine, Jev: fakeJev, Secrets: echoStore{}, ModelsAllow: []string{"*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	r := &rig{root: root, e: e, live: &liveExec{t: t}}
	v := r.apply(t, "ai-judge")
	return r, r.startRecorded(t, "ai-judge", v.Hash)
}

func judgeOutput(t *testing.T, r *rig, run core.Run) map[string]any {
	t.Helper()
	steps, _ := r.e.Services.Runs.ListSteps(bg, core.DefaultTenant, run.ID)
	for _, s := range steps {
		if s.StepID == "judge" {
			out, _ := s.Output.(map[string]any)
			return out
		}
	}
	t.Fatalf("no judge step in %+v", steps)
	return nil
}

func tapeOf(t *testing.T, r *rig, run core.Run) (kept []string, all string) {
	t.Helper()
	cass, _ := r.e.Recordings.Cassettes(core.DefaultTenant, run.ID)
	kept, _ = filepath.Glob(filepath.Join(cass, "http-*.req.yaml"))
	var sb strings.Builder
	ents, _ := os.ReadDir(cass)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(cass, e.Name())) //nolint:gosec // the test's own tape
		sb.Write(b)
	}
	return kept, sb.String()
}

func TestARecordedRunKeepsTheLocalEngineAndItsDetection(t *testing.T) {
	net := &judgeNet{models: `{"name":"gemma4:e4b","model":"gemma4:e4b"}`}
	r, run := judgeRun(t, net, "")
	if run.Status != core.RunSucceeded {
		t.Fatalf("the recorded run: %s", run.Status)
	}
	if out := judgeOutput(t, r, run); out["engine"] != "local" {
		t.Errorf("the run does not say what judged: %v", out)
	}
	if len(net.calls) != 2 || !strings.HasSuffix(net.calls[0], "/api/tags") || !strings.HasSuffix(net.calls[1], "/api/chat") {
		t.Fatalf("live calls %v", net.calls)
	}
	if kept, _ := tapeOf(t, r, run); len(kept) != 2 {
		t.Errorf("the detection and the model call are not both on the tape: %d cassettes", len(kept))
	}
}

func TestARecordedRunKeepsTheJevEngineAndItsDetection(t *testing.T) {
	net := &judgeNet{models: `{"name":"other:1b","model":"other:1b"}`}
	r, run := judgeRun(t, net, "")
	if run.Status != core.RunSucceeded {
		t.Fatalf("the recorded run: %s", run.Status)
	}
	if out := judgeOutput(t, r, run); out["engine"] != "jev" {
		t.Errorf("the run does not say what judged: %v", out)
	}
	if len(net.calls) != 2 || !strings.HasSuffix(net.calls[0], "/api/tags") || net.calls[1] != "POST openrouter.ai/api/v1/systemone" {
		t.Fatalf("live calls %v", net.calls)
	}
	kept, tape := tapeOf(t, r, run)
	if len(kept) != 2 {
		t.Errorf("the detection and the Jev call are not both on the tape: %d cassettes", len(kept))
	}
	if strings.Contains(tape, storeSecret) {
		t.Error("the Jev key is on the tape")
	}
}

// The recorded run replays with the tape alone: no live call.
func TestARecordedJudgeRunReplaysFromItsTape(t *testing.T) {
	net := &judgeNet{models: `{"name":"other:1b","model":"other:1b"}`}
	r, run := judgeRun(t, net, "")
	live := len(net.calls)
	res, err := r.replay(t, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Matches || len(res.Diffs) != 0 || len(res.Problems) != 0 || res.Calls != 2 || res.Replayed != 2 || len(net.calls) != live {
		t.Errorf("replay: %+v, live calls %d -> %d", res, live, len(net.calls))
	}
}

// fakeJev stands in for kit's Jev judge: it posts the case to Jev's endpoint
// with the key and reads the System One reply for the question "label".
func fakeJev(key, _ string, rt http.RoundTripper) (judge.Judge, error) {
	return fakeJevJudge{key, rt}, nil
}

type fakeJevJudge struct {
	key string
	rt  http.RoundTripper
}

func (j fakeJevJudge) Judge(ctx context.Context, c judge.Case) (judge.Verdict, error) {
	body, _ := json.Marshal(map[string]any{"state": c.State, "questions": c.Questions})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/systemone", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+j.key)
	rt := j.rt
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return judge.Verdict{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusOK {
		return judge.Verdict{}, fmt.Errorf("jev: status %d", resp.StatusCode)
	}
	var r struct {
		Answers map[string]struct {
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
		Usage struct {
			In   int     `json:"input_tokens"`
			Out  int     `json:"output_tokens"`
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return judge.Verdict{}, err
	}
	a := r.Answers["label"]
	return judge.Verdict{
		Answers: map[string]judge.Answer{"label": {Value: a.Choice, Confidence: a.Confidence}},
		Cost:    judge.Cost{USD: r.Usage.Cost, Tokens: int64(r.Usage.In + r.Usage.Out)},
	}, nil
}
