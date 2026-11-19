package judgeeval

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/eval"
	"hop.top/kit/go/ai/judge/judgetest"
)

// The case sets here are synthetic (testdata/judgeeval): example.org senders
// and made-up subjects. Nothing in the repository is a real message.

func flowYAML(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "flows", "inbox-triage", "flow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestQuestionFollowsTheFlow(t *testing.T) {
	q, err := Question(flowYAML(t), StepLabel)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Abstain || len(q.Options) != 14 || q.Options[0].Value != "Action Required" || !strings.Contains(q.Instructions, "Abstain when") {
		t.Errorf("label question: %+v", q)
	}
	w, err := Question(flowYAML(t), StepWeigh)
	if err != nil {
		t.Fatal(err)
	}
	if w.Abstain || len(w.Options) != 5 || w.Options[0].Value != "irrelevant" || w.Options[4].Value != "critical" {
		t.Errorf("weigh question: %+v", w)
	}
	if _, err := Question(flowYAML(t), "nope"); err == nil {
		t.Error("an unknown step was accepted")
	}
}

func TestImportRun(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "judgeeval", "run")
	ql, _ := Question(flowYAML(t), StepLabel)
	set, st, err := ImportRun(dir, ql, StepLabel)
	if err != nil {
		t.Fatal(err)
	}
	// 9 rows, one answered by hand; 8 decided by Jev; Needs Review (row 6) has no pick
	if st.Rows != 8 || st.Decided != 8 || st.NoLabel != 1 || st.Abstained != 1 || len(set) != 7 {
		t.Fatalf("stats %+v, %d entries", st, len(set))
	}
	want := map[string]string{"Weekly digest": "Subscriptions", "Invoice 2291": "Finance", "Contract renewal": judge.LabelAbstain, "Re: kickoff": "Projects"}
	for _, e := range set {
		subj, _ := e.Case.State["subject"].(string)
		if w, ok := want[subj]; ok && e.Reference.Labels[QuestionName] != w {
			t.Errorf("%s: reference %q, want %q", subj, e.Reference.Labels[QuestionName], w)
		}
		if _, body := e.Case.State["body"]; body {
			t.Errorf("a body in the case: %v", e.Case.State)
		}
	}

	qw, _ := Question(flowYAML(t), StepWeigh)
	weigh, st, err := ImportRun(dir, qw, StepWeigh)
	if err != nil || len(weigh) != 8 {
		t.Fatalf("weigh: %v, %d entries, %+v", err, len(weigh), st)
	}
	wantW := map[string]string{"Weekly digest": "irrelevant", "Invoice 2291": "high", "Lunch on Friday?": "normal", "Contract renewal": "high"}
	for _, e := range weigh {
		subj, _ := e.Case.State["subject"].(string)
		if w, ok := wantW[subj]; ok && e.Reference.Labels[QuestionName] != w {
			t.Errorf("weigh %s: reference %q, want %q", subj, e.Reference.Labels[QuestionName], w)
		}
	}
}

// answers computes a candidate verdict per case: the label named by subject.
func answers(byLabel map[string]string, conf float64) *judgetest.Fake {
	return &judgetest.Fake{Fn: func(_ context.Context, c judge.Case) (judge.Verdict, error) {
		subj, _ := c.State["subject"].(string)
		v := byLabel[subj]
		a := judge.Answer{Value: v, Confidence: conf, Source: "verbalized"}
		if v == judge.LabelAbstain {
			a.Abstained = true
		}
		return judge.Verdict{Answers: map[string]judge.Answer{QuestionName: a}, Backend: "fake"}, nil
	}}
}

func TestEvaluateReportsAndGates(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "judgeeval", "run")
	ql, _ := Question(flowYAML(t), StepLabel)
	set, _, err := ImportRun(dir, ql, StepLabel)
	if err != nil {
		t.Fatal(err)
	}
	// a candidate that agrees on six of seven cases
	cand := answers(map[string]string{
		"Weekly digest": "Subscriptions", "Invoice 2291": "Finance", "Lunch on Friday?": "People",
		"Contract renewal": judge.LabelAbstain, "Your order has shipped": "Reference",
		"Quarterly numbers": "Finance", "Re: kickoff": "Operations", // disagrees
	}, 0.95)
	res, err := Evaluate(context.Background(), cand, set, eval.Options{}, eval.Thresholds{MinN: 5, MinAgreement: 0.8})
	if err != nil {
		t.Fatal(err)
	}
	q := res.Report.Questions[0]
	if res.Report.N != 7 || q.N != 7 || q.Matches != 6 || !res.Pass {
		t.Fatalf("report %+v, pass %v %v", q, res.Pass, res.Reasons)
	}
	var out bytes.Buffer
	if err := res.Write(&out); err != nil || !strings.Contains(out.String(), "| label | choice | 7 |") || !strings.HasSuffix(strings.TrimSpace(out.String()), "PASS") {
		t.Errorf("output: %v\n%s", err, out.String())
	}
	// a stricter gate fails, and says why
	res, err = Evaluate(context.Background(), cand, set, eval.Options{}, eval.Thresholds{MinAgreement: 0.95})
	if err != nil || res.Pass || len(res.Reasons) == 0 || !strings.Contains(res.Reasons[0], "agreement") {
		t.Fatalf("a failing gate: %v %+v", err, res)
	}
	out.Reset()
	_ = res.Write(&out)
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("output: %s", out.String())
	}
	if _, err := Evaluate(context.Background(), cand, nil, eval.Options{}, eval.Thresholds{}); err == nil {
		t.Error("an empty set was evaluated")
	}
}

func TestRecordThenCompare(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "judgeeval", "run")
	ql, _ := Question(flowYAML(t), StepLabel)
	set, _, _ := ImportRun(dir, ql, StepLabel)
	always := answers(map[string]string{}, 0.9)
	always.Fn = func(_ context.Context, _ judge.Case) (judge.Verdict, error) {
		return judge.Verdict{Answers: map[string]judge.Answer{QuestionName: {Value: "Reference", Confidence: 0.9}}}, nil
	}
	rec, err := Record(context.Background(), always, set, 2)
	if err != nil || len(rec) != len(set) || rec[0].Reference.Verdict == nil {
		t.Fatalf("record: %v", err)
	}
	// the recorded judge against itself agrees everywhere
	res, err := Evaluate(context.Background(), always, rec, eval.Options{}, eval.Thresholds{MinAgreement: 1})
	if err != nil || !res.Pass {
		t.Fatalf("self-agreement: %v %+v", err, res)
	}
}

func TestSetsOfRealMailStayOutOfRepositories(t *testing.T) {
	// a directory holding .git is a work tree, whatever is under it
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repo, "sub", "set.jsonl")
	if in, err := InsideGitTree(inside); err != nil || !in {
		t.Fatalf("InsideGitTree(%s) = %v, %v", inside, in, err)
	}
	if err := WriteSet(inside, []eval.Entry{{}}); err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Errorf("a set was written into a work tree: %v", err)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("the file exists")
	}
	// a link out of the tree into a repository counts as inside
	elsewhere := t.TempDir()
	link := filepath.Join(elsewhere, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if in, _ := InsideGitTree(filepath.Join(link, "set.jsonl")); !in {
		t.Error("a symlink into a work tree is outside")
	}
	outside := filepath.Join(t.TempDir(), "set.jsonl")
	if in, err := InsideGitTree(outside); err != nil || in {
		t.Fatalf("InsideGitTree(%s) = %v, %v", outside, in, err)
	}
	if err := WriteSet(outside, []eval.Entry{{Case: judge.Case{State: map[string]any{"subject": "x"}, Questions: []judge.Question{{Name: "q", Kind: judge.KindYesNo}}}}}); err != nil {
		t.Errorf("a set outside a work tree: %v", err)
	}
}
