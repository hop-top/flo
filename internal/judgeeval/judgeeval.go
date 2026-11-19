// Package judgeeval compares judges on a recorded day of mail, over kit's
// go/ai/judge/eval: a case set (what the inbox-triage classify steps ask of
// each message) with reference verdicts, a candidate judge (a local model, or
// Jev when the owner asks), and the Report and Pass of kit's gate.
//
// Mail is private. Case sets built from a real inbox hold headers (sender,
// subject, history): they are written outside any git work tree (WriteSet
// refuses a path inside one) and never committed. The committed fixtures are
// synthetic.
package judgeeval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/eval"
	"hop.top/kit/go/ai/judge/llmjudge"
	"hop.top/kit/go/ai/llm"

	"hop.top/flo/internal/nodes/builtin"
)

// Steps of the inbox-triage flow that ask a model about a message.
const (
	StepLabel = "label"
	StepWeigh = "weigh"
	// QuestionName is the name of the question each case asks, as the
	// classify steps name it.
	QuestionName = "label"
)

// Question reads the question a classify step of a flow document asks: its
// labels (a literal list, or a var's default when it names one), its
// instructions and whether it may abstain. The flow is the one source of the
// wording, so the evaluation asks what the flow asks.
func Question(flowYAML []byte, step string) (judge.Question, error) {
	var doc struct {
		Vars  map[string]struct{ Default []string } `yaml:"vars"`
		Steps []struct {
			ID   string `yaml:"id"`
			With struct {
				Labels       any    `yaml:"labels"`
				Instructions string `yaml:"instructions"`
				Abstain      bool   `yaml:"abstain"`
			} `yaml:"with"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(flowYAML, &doc); err != nil {
		return judge.Question{}, fmt.Errorf("judgeeval: flow: %w", err)
	}
	for _, s := range doc.Steps {
		if s.ID != step {
			continue
		}
		var labels []string
		switch l := s.With.Labels.(type) {
		case []any:
			for _, v := range l {
				labels = append(labels, fmt.Sprint(v))
			}
		case string:
			name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "{{ vars."), " }}")
			labels = doc.Vars[name].Default
		}
		if len(labels) < 2 {
			return judge.Question{}, fmt.Errorf("judgeeval: step %q has no usable labels", step)
		}
		opts := make([]judge.Option, len(labels))
		for i, v := range labels {
			opts[i] = judge.Option{Value: v}
		}
		return judge.Question{
			Name: QuestionName, Kind: judge.KindChoice, Options: opts,
			Instructions: strings.TrimSpace(s.With.Instructions), Abstain: s.With.Abstain,
		}, nil
	}
	return judge.Question{}, fmt.Errorf("judgeeval: the flow has no step %q", step)
}

// Result is a comparison: kit's report and the gate's verdict.
type Result struct {
	Report  eval.Report
	Pass    bool
	Reasons []string
}

// Evaluate replays set through cand and scores it against the references. A
// zero Thresholds checks nothing but that questions were evaluated.
func Evaluate(ctx context.Context, cand judge.Judge, set []eval.Entry, opt eval.Options, th eval.Thresholds) (Result, error) {
	if len(set) == 0 {
		return Result{}, errors.New("judgeeval: the case set is empty")
	}
	rep, err := eval.Compare(ctx, nil, cand, set, opt)
	if err != nil {
		return Result{}, fmt.Errorf("judgeeval: %w", err)
	}
	ok, reasons := rep.Pass(th)
	return Result{Report: rep, Pass: ok, Reasons: reasons}, nil
}

// Write prints the report as Markdown, then the gate's line.
func (r Result) Write(w io.Writer) error {
	if _, err := fmt.Fprintln(w, r.Report.Markdown()); err != nil {
		return err //nolint:wrapcheck // the writer's
	}
	if r.Pass {
		_, err := fmt.Fprintln(w, "PASS")
		return err //nolint:wrapcheck // the writer's
	}
	_, err := fmt.Fprintf(w, "FAIL\n- %s\n", strings.Join(r.Reasons, "\n- "))
	return err //nolint:wrapcheck // the writer's
}

// Record judges the cases with j and returns them as entries whose reference
// is j's verdict: a recorded day, to replay against another judge.
func Record(ctx context.Context, j judge.Judge, set []eval.Entry, parallel int) ([]eval.Entry, error) {
	cases := make([]judge.Case, len(set))
	for i, e := range set {
		cases[i] = e.Case
	}
	out, err := eval.Record(ctx, j, cases, parallel)
	if err != nil {
		return nil, fmt.Errorf("judgeeval: %w", err)
	}
	return out, nil
}

// Local is a judge over a local model, "ollama://[host:port/]<model>". The
// client is hermetic and local-only: a model that is not on this machine or a
// private host is refused before any request. The base URL is the model URI's;
// to reach a daemon on another host (a tunnel, a build host), name it there.
func Local(model string) (judge.Judge, error) {
	uri, err := llm.ParseURI(model)
	if err != nil {
		return nil, fmt.Errorf("judgeeval: model %q is not a model URI: %w", llm.RedactURI(model), err)
	}
	p, err := llm.Resolve(model, llm.Hermetic(), llm.LocalOnly())
	if err != nil {
		return nil, fmt.Errorf("judgeeval: model %q: %w", llm.RedactURI(model), err)
	}
	c, err := llm.LocalOnlyClient(p)
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("judgeeval: model %q: %w", llm.RedactURI(model), err)
	}
	return llmjudge.New(c, llmjudge.Model(uri.Model)), nil
}

// Jev is the Jev judge (TypeSafe through OpenRouter): a cloud service. The
// case set goes to it, so the caller must have decided that it may; the key is
// the caller's. An empty model is kit's default one.
func Jev(key, model string) (judge.Judge, error) {
	if key == "" {
		return nil, errors.New("judgeeval: jev needs a key")
	}
	j, err := builtin.KitJev(key, model, http.DefaultTransport)
	if err != nil {
		return nil, fmt.Errorf("judgeeval: %w", err)
	}
	return j, nil
}

// WriteSet writes a case set as JSONL, unless the path is inside a git work
// tree: real mail never lands in a repository.
func WriteSet(path string, set []eval.Entry) error {
	if in, err := InsideGitTree(path); err != nil {
		return err
	} else if in {
		return fmt.Errorf("judgeeval: %s is inside a git work tree; case sets of real mail are written elsewhere", path)
	}
	return eval.SaveFile(path, set) //nolint:wrapcheck // kit's
}

// InsideGitTree reports whether path, which need not exist, is under a
// directory holding a .git entry (a work tree, a worktree or a submodule).
func InsideGitTree(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("judgeeval: %w", err)
	}
	// resolve symlinks on the part that exists, so a link into a repository counts
	dir := filepath.Dir(abs)
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			rest, _ := filepath.Rel(dir, filepath.Dir(abs))
			dir = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}
