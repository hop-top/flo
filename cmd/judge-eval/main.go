// Command judge-eval compares judges on a recorded day of mail, for the
// inbox-triage flow: which one should answer its classify steps.
//
//	judge-eval import --flow FLOW.yaml --step label|weigh --out SET.jsonl RUN_DIR...
//	judge-eval run    --cases SET.jsonl --model ollama://[host:port/]M [gate flags]
//	judge-eval run    --cases SET.jsonl --jev --allow-cloud [gate flags]
//	judge-eval record --cases SET.jsonl --model ollama://... --out SET2.jsonl
//
// import reads run folders of the inbox script (snapshot.json, decisions.json)
// and writes a case set whose references are the verdicts the script's hosted
// decider (Jev) recorded: no model is called. run replays the set through a
// candidate and prints kit's report and the gate's PASS or FAIL (exit 3 on
// FAIL). record judges a set with a candidate and writes the answers as the
// references, to replay another judge against them.
//
// Real mail never goes into a repository: a set written by import or record
// must be outside any git work tree, and the tool refuses otherwise. The
// models are local (ollama) unless --jev is given with --allow-cloud, which
// sends every case of the set to Jev with the key in OPENROUTER_API_KEY.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/eval"

	"hop.top/flo/internal/judgeeval"
)

const exitGate = 3

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: judge-eval import|run|record [flags]")
		return 64
	}
	var err error
	code := 0
	switch args[0] {
	case "import":
		err = importCmd(args[1:], stdout)
	case "run":
		code, err = runCmd(ctx, args[1:], stdout)
	case "record":
		err = recordCmd(ctx, args[1:], stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "judge-eval: unknown command %q\n", args[0])
		return 64
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "judge-eval:", err)
		if errors.Is(err, flag.ErrHelp) {
			return 64
		}
		return 1
	}
	return code
}

func importCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	flow := fs.String("flow", "flows/inbox-triage/flow.yaml", "the flow document the questions are read from")
	step := fs.String("step", judgeeval.StepLabel, "the classify step to build cases for: label or weigh")
	dest := fs.String("out", "", "the case set to write (outside any git work tree)")
	if err := fs.Parse(args); err != nil {
		return err //nolint:wrapcheck // flag's
	}
	if *dest == "" || fs.NArg() == 0 {
		return errors.New("usage: import --step label|weigh --out SET.jsonl RUN_DIR")
	}
	if *step != judgeeval.StepLabel && *step != judgeeval.StepWeigh {
		return fmt.Errorf("--step %q is not label or weigh", *step)
	}
	if in, err := judgeeval.InsideGitTree(*dest); err != nil || in {
		return fmt.Errorf("--out %s must be outside any git work tree (real mail)%v", *dest, errOrNil(err))
	}
	b, err := os.ReadFile(*flow) //nolint:gosec // the flow the caller names
	if err != nil {
		return err //nolint:wrapcheck // the OS's
	}
	q, err := judgeeval.Question(b, *step)
	if err != nil {
		return err //nolint:wrapcheck // ours
	}
	var set []eval.Entry
	var total judgeeval.ImportStats
	for _, dir := range fs.Args() {
		es, st, err := judgeeval.ImportRun(dir, q, *step)
		if err != nil {
			return err //nolint:wrapcheck // ours
		}
		set = append(set, es...)
		total.Rows += st.Rows
		total.Decided += st.Decided
		total.NoLabel += st.NoLabel
		total.Abstained += st.Abstained
	}
	if err := judgeeval.WriteSet(*dest, set); err != nil {
		return err //nolint:wrapcheck // ours
	}
	_, _ = fmt.Fprintf(out, "%d runs, %d rows, %d decided by Jev, %d cases written to %s\n", fs.NArg(), total.Rows, total.Decided, len(set), *dest)
	if *step == judgeeval.StepLabel {
		_, _ = fmt.Fprintf(out, "  %d are abstentions (Jev left them to a person); %d rows left out: Jev's pick was under the floor and not kept\n", total.Abstained, total.NoLabel)
	}
	return nil
}

func errOrNil(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// candidate builds the judge named by the flags.
func candidate(model string, jev, allowCloud bool, jevModel string) (judge.Judge, error) {
	switch {
	case jev && model != "":
		return nil, errors.New("--jev and --model are exclusive")
	case jev:
		if !allowCloud {
			return nil, errors.New("--jev sends the case set to a hosted service: pass --allow-cloud to say the mail may leave the machine")
		}
		return judgeeval.Jev(os.Getenv("OPENROUTER_API_KEY"), jevModel) //nolint:wrapcheck // ours
	case model == "":
		return nil, errors.New("name a candidate: --model ollama://<model>, or --jev --allow-cloud")
	}
	return judgeeval.Local(model) //nolint:wrapcheck // ours
}

func runCmd(ctx context.Context, args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cases := fs.String("cases", "", "the case set (JSONL) with references")
	model := fs.String("model", "", "local candidate: ollama://[host:port/]<model>")
	jev := fs.Bool("jev", false, "candidate is Jev (a hosted service)")
	allowCloud := fs.Bool("allow-cloud", false, "the case set may be sent to the hosted candidate")
	jevModel := fs.String("jev-model", "", "Jev model (default: kit's)")
	parallel := fs.Int("parallel", 2, "concurrent model calls")
	atConf := fs.Float64("at-confidence", 0.9, "confidence cut-off the gate reads agreement and coverage at (one of 0.5 0.7 0.8 0.9 0.95; 0: overall)")
	minAgree := fs.Float64("min-agreement", 0, "gate: least agreement (0: not checked)")
	minCover := fs.Float64("min-coverage", 0, "gate: least coverage (0: not checked)")
	minN := fs.Int("min-n", 0, "gate: least cases per question")
	maxECE := fs.Float64("max-ece", 0, "gate: greatest ECE (0: not checked)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 0, err //nolint:wrapcheck // flag's
	}
	if *cases == "" {
		return 0, errors.New("usage: run --cases SET.jsonl (--model ollama://M | --jev --allow-cloud)")
	}
	cand, err := candidate(*model, *jev, *allowCloud, *jevModel)
	if err != nil {
		return 0, err
	}
	set, err := eval.LoadFile(*cases)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", *cases, err)
	}
	res, err := judgeeval.Evaluate(ctx, cand, set, eval.Options{Parallel: *parallel}, eval.Thresholds{
		MinN: *minN, AtConfidence: *atConf, MinAgreement: *minAgree, MinCoverage: *minCover, MaxECE: *maxECE,
	})
	if err != nil {
		return 0, err //nolint:wrapcheck // ours
	}
	if *asJSON {
		b, err := res.Report.JSON()
		if err != nil {
			return 0, fmt.Errorf("report: %w", err)
		}
		_, _ = fmt.Fprintln(out, string(b))
	}
	if !*asJSON {
		if err := res.Write(out); err != nil {
			return 0, err //nolint:wrapcheck // the writer's
		}
	} else if !res.Pass {
		_, _ = fmt.Fprintf(out, "FAIL: %s\n", strings.Join(res.Reasons, "; "))
	}
	if !res.Pass {
		return exitGate, nil
	}
	return 0, nil
}

func recordCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	cases := fs.String("cases", "", "the case set (JSONL); its references are replaced")
	dest := fs.String("out", "", "the case set to write (outside any git work tree)")
	model := fs.String("model", "", "local judge: ollama://[host:port/]<model>")
	jev := fs.Bool("jev", false, "judge with Jev (a hosted service)")
	allowCloud := fs.Bool("allow-cloud", false, "the case set may be sent to the hosted judge")
	jevModel := fs.String("jev-model", "", "Jev model (default: kit's)")
	parallel := fs.Int("parallel", 2, "concurrent model calls")
	if err := fs.Parse(args); err != nil {
		return err //nolint:wrapcheck // flag's
	}
	if *cases == "" || *dest == "" {
		return errors.New("usage: record --cases SET.jsonl --out SET2.jsonl (--model ollama://M | --jev --allow-cloud)")
	}
	if in, err := judgeeval.InsideGitTree(*dest); err != nil || in {
		return fmt.Errorf("--out %s must be outside any git work tree (real mail)%v", *dest, errOrNil(err))
	}
	j, err := candidate(*model, *jev, *allowCloud, *jevModel)
	if err != nil {
		return err
	}
	set, err := eval.LoadFile(*cases)
	if err != nil {
		return fmt.Errorf("%s: %w", *cases, err)
	}
	rec, err := judgeeval.Record(ctx, j, set, *parallel)
	if err != nil {
		return err //nolint:wrapcheck // ours
	}
	if err := judgeeval.WriteSet(*dest, rec); err != nil {
		return err //nolint:wrapcheck // ours
	}
	_, _ = fmt.Fprintf(out, "%d cases judged, written to %s\n", len(rec), *dest)
	return nil
}
