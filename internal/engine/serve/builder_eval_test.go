//go:build !windows

package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/kit/go/runtime/sideeffect"
	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// The builder model comparison: the same drafting tasks (builder_eval_tasks_test.go)
// asked of each local model through the engine's own builder path, scored on
// whether the proposal validates, how many attempts it took, whether it did
// what was asked, whether the dry run matches, how long it took and what it
// cost in tokens. Off by default; see docs/builder-model-eval.md.
//
//	FLO_BUILDER_EVAL=model1,model2 go test ./internal/engine/serve -run TestBuilderModelEval -v -timeout 0

// evalMeter is the engine's HTTP seam for a comparison: it pins the sampling
// (temperature 0, a fixed seed, a context that holds the prompt and its
// retries) on each chat call, and counts the calls and the tokens ollama
// reports. Nothing else about the call changes.
type evalMeter struct {
	inner sideeffect.HTTP
	mu    sync.Mutex
	calls int
	in    int64
	out   int64
}

func (m *evalMeter) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/api/chat" || req.Body == nil {
		return m.inner.Do(req)
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read chat request: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("chat request: %w", err)
	}
	opts, _ := body["options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	opts["temperature"], opts["seed"], opts["num_ctx"], opts["num_predict"] = 0, 7, 12288, 6144
	body["options"], body["keep_alive"] = opts, "20m"
	raw, _ = json.Marshal(body)
	r2 := req.Clone(req.Context())
	r2.Body, r2.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	resp, err := m.inner.Do(r2)
	if err != nil {
		return resp, err //nolint:wrapcheck // the seam's own error
	}
	rb, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(rb))
	m.mu.Lock()
	m.calls++
	if rerr == nil {
		for _, line := range bytes.Split(rb, []byte("\n")) { // one object, or one per streamed chunk
			var c struct {
				In  int64 `json:"prompt_eval_count"`
				Out int64 `json:"eval_count"`
			}
			if json.Unmarshal(line, &c) == nil {
				m.in, m.out = m.in+c.In, m.out+c.Out
			}
		}
	}
	m.mu.Unlock()
	return resp, nil
}

func (m *evalMeter) take() (calls int, in, out int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls, in, out = m.calls, m.in, m.out
	m.calls, m.in, m.out = 0, 0, 0
	return calls, in, out
}

// evalResult is one task asked of one model once. It holds numbers and the
// verdict of the check, never a prompt or an answer.
type evalResult struct {
	Model     string  `json:"model"`
	Task      string  `json:"task"`
	Run       int     `json:"run"`
	Valid     bool    `json:"valid"`     // a proposal validated within the retries
	Attempts  int     `json:"attempts"`  // model calls
	CheckOK   bool    `json:"check_ok"`  // the scripted check passed
	CheckErr  string  `json:"check_err"` // why not
	Dry       bool    `json:"dry"`       // the task has a recording to replay on
	DryMatch  bool    `json:"dry_match"`
	Err       string  `json:"err,omitempty"` // a failure other than invalid_flow
	Seconds   float64 `json:"seconds"`
	TokensIn  int64   `json:"tokens_in"`
	TokensGen int64   `json:"tokens_gen"`
}

// setup stores the task's base flow in the rig, and records a run of it when
// the task is replayed.
func (b *builderRig) setup(t *testing.T, task evalTask) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), task.Fixture)
	copyTree(t, testdata(fixtureDir(task.Fixture)...), bundle)
	var caps []string
	if task.Fixture == "dependency-watch" {
		caps = []string{"vcs.read"}
	}
	v := b.applyBundle(t, bundle, caps...)
	if task.Dry {
		if run := b.startRecorded(t, task.Fixture, v.Hash); run.Status != core.RunSucceeded {
			t.Fatalf("recorded run: %s", run.Status)
		}
	}
	return string(v.Source)
}

// ask drafts the task on the rig and scores it. model is the URI; empty is the
// engine's default.
func (b *builderRig) ask(ctx context.Context, task evalTask, model string) evalResult {
	res := evalResult{Task: task.ID, Dry: task.Dry}
	start := time.Now()
	d, err := b.e.CreateDraft(ctx, core.DefaultTenant, serve.DraftRequest{
		Flow: task.Fixture, Instruction: task.Instruction, Model: model, Caller: "jad",
	})
	res.Seconds = time.Since(start).Seconds()
	switch {
	case err == nil:
		res.Valid = true
		res.Attempts = len(d.Attempts)
		if doc, perr := parseDoc(d.Source); perr != nil {
			res.CheckErr = perr.Error()
		} else if cerr := task.Check(doc); cerr != nil {
			res.CheckErr = cerr.Error()
		} else {
			res.CheckOK = true
		}
		res.DryMatch = d.DryRun != nil && d.DryRun.Ran && d.DryRun.Matches
	case core.IsCode(err, core.CodeInvalidFlow):
		res.Attempts = 1 + 3
		res.CheckErr = "no valid proposal in 4 attempts: " + err.Error()
	default:
		res.Err = err.Error() // a timeout or a model fault: counted as a failed draft
		res.Attempts = 1 + 3
		res.CheckErr = "draft failed"
	}
	return res
}

func TestBuilderModelEval(t *testing.T) {
	models := os.Getenv("FLO_BUILDER_EVAL")
	if models == "" {
		t.Skip("set FLO_BUILDER_EVAL=<ollama model>[,<model>...] to compare local models (docs/builder-model-eval.md)")
	}
	runs := 1
	if v, err := strconv.Atoi(os.Getenv("FLO_BUILDER_EVAL_RUNS")); err == nil && v > 0 {
		runs = v
	}
	limit := 10 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("FLO_BUILDER_EVAL_TIMEOUT")); err == nil && d > 0 {
		limit = d
	}
	only := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("FLO_BUILDER_EVAL_TASKS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			only[id] = true
		}
	}

	meter := &evalMeter{inner: real.NewHTTP(nil)}
	var results []evalResult
	for _, model := range strings.Split(models, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		warm(t, model) // a cold load is not the model's latency
		for run := 1; run <= runs; run++ {
			for _, task := range evalTasks {
				if len(only) > 0 && !only[task.ID] {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d/%s", model, run, task.ID), func(t *testing.T) {
					r := newBuilderRig(t, func(c *serve.Config) { c.HTTP = meter })
					r.setup(t, task)
					meter.take()
					ctx, cancel := context.WithTimeout(bg, limit)
					defer cancel()
					res := r.ask(ctx, task, "ollama://"+model)
					res.Model, res.Run = model, run
					_, res.TokensIn, res.TokensGen = meter.take()
					results = append(results, res)
					t.Logf("%s: valid=%v attempts=%d check=%v %s %.1fs", task.ID, res.Valid, res.Attempts, res.CheckOK, res.CheckErr, res.Seconds)
				})
			}
		}
		unload(t, model)
	}
	report(t, results)
}

// warm loads a model and unload frees it, as ollama's API takes them. They are
// best effort: a failure shows in the first task.
func warm(t *testing.T, model string) {
	t.Helper()
	ollamaChat(t, map[string]any{
		"model": model, "stream": false, "keep_alive": "20m",
		"messages": []any{map[string]any{"role": "user", "content": "ok"}},
		"options":  map[string]any{"num_predict": 1, "num_ctx": 12288},
	})
}

func unload(t *testing.T, model string) {
	t.Helper()
	ollamaChat(t, map[string]any{"model": model, "keep_alive": 0, "messages": []any{}})
}

func ollamaChat(t *testing.T, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(bg, http.MethodPost, "http://localhost:11434/api/chat", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		t.Logf("ollama: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// report prints the per-model summary and the per-task matrix as markdown, and
// writes every result as JSON to FLO_BUILDER_EVAL_OUT when set.
func report(t *testing.T, rs []evalResult) {
	t.Helper()
	var order []string
	by := map[string][]evalResult{}
	for _, r := range rs {
		if _, ok := by[r.Model]; !ok {
			order = append(order, r.Model)
		}
		by[r.Model] = append(by[r.Model], r)
	}
	var out strings.Builder
	out.WriteString("\n| model | drafts | valid 1st try | mean attempts | failed after 3 retries | check pass | dry-run match | mean s/draft | max s | mean tokens in/gen |\n")
	out.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range order {
		g := by[m]
		var first, fail, ok, dry, dryOK, att int
		var secs, maxS float64
		var tin, tgen int64
		for _, r := range g {
			if r.Valid && r.Attempts == 1 {
				first++
			}
			if !r.Valid {
				fail++
			}
			if r.CheckOK {
				ok++
			}
			if r.Dry {
				dry++
				if r.DryMatch {
					dryOK++
				}
			}
			att += r.Attempts
			secs += r.Seconds
			maxS = max(maxS, r.Seconds)
			tin += r.TokensIn
			tgen += r.TokensGen
		}
		n := len(g)
		fmt.Fprintf(&out, "| %s | %d | %d/%d | %.2f | %d | %d/%d | %d/%d | %.1f | %.1f | %d/%d |\n",
			m, n, first, n, float64(att)/float64(n), fail, ok, n, dryOK, dry, secs/float64(n), maxS, tin/int64(n), tgen/int64(n))
	}
	out.WriteString("\n| task |")
	for _, m := range order {
		out.WriteString(" " + m + " |")
	}
	out.WriteString("\n|---|" + strings.Repeat("---|", len(order)) + "\n")
	var ids []string
	seen := map[string]bool{}
	for _, r := range rs {
		if !seen[r.Task] {
			seen[r.Task], ids = true, append(ids, r.Task)
		}
	}
	for _, id := range ids {
		out.WriteString("| " + id + " |")
		for _, m := range order {
			var cell []string
			for _, r := range by[m] {
				if r.Task != id {
					continue
				}
				switch {
				case r.CheckOK:
					cell = append(cell, fmt.Sprintf("ok(%d)", r.Attempts))
				case r.Valid:
					cell = append(cell, fmt.Sprintf("wrong(%d)", r.Attempts))
				default:
					cell = append(cell, "FAIL")
				}
			}
			out.WriteString(" " + strings.Join(cell, " ") + " |")
		}
		out.WriteString("\n")
	}
	out.WriteString("\ncell: ok = check passed, wrong = valid but the check failed, FAIL = no valid proposal; (n) = attempts\n")
	fmt.Print(out.String())
	if p := filepath.Clean(os.Getenv("FLO_BUILDER_EVAL_OUT")); os.Getenv("FLO_BUILDER_EVAL_OUT") != "" {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].Model < rs[j].Model })
		b, _ := json.MarshalIndent(rs, "", " ")
		if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil { //nolint:gosec // the path is the operator's own setting
			t.Error(err)
		}
		if err := os.WriteFile(p+".md", []byte(out.String()), 0o600); err != nil { //nolint:gosec // the path is the operator's own setting
			t.Error(err)
		}
	}
}

// TestBuilderEvalTasksAreSound runs on every `go test`, with no model: each
// task's reference solution passes its check and replays as the task says, and
// the unchanged base does not pass. A check that accepts the base, or rejects
// the solution, would score a model on nothing.
func TestBuilderEvalTasksAreSound(t *testing.T) {
	for _, task := range evalTasks {
		t.Run(task.ID, func(t *testing.T) {
			r := newBuilderRig(t, nil)
			base := r.setup(t, task)
			if doc, err := parseDoc(base); err != nil || task.Check(doc) == nil {
				t.Fatalf("the base document passes the check (parse: %v)", err)
			}
			r.model.replies = []string{yamlReply(task.Solution(base))}
			res := r.ask(bg, task, "")
			if !res.Valid || res.Attempts != 1 || !res.CheckOK {
				t.Fatalf("the reference solution: %+v", res)
			}
			if task.Dry && !res.DryMatch {
				t.Fatalf("the reference solution does not replay as the recording: %+v", res)
			}
		})
	}
}
