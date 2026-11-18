package flowtest

import (
	"fmt"

	"hop.top/flo/internal/core"
)

// Evaluate turns the run tree of a test run into the result's steps and the
// reasons it failed (flow.md testing: the run completes with no step error,
// every output validates against its node spec, every check passes). The
// `expect:` blocks are checked by the caller with Check.
func Evaluate(run core.Run, recs []core.StepRun) (steps []Step, problems []Problem) {
	steps = make([]Step, 0, len(recs))
	for _, r := range recs {
		st := Step{
			ID: r.StepID, Kind: string(r.Kind), Node: r.Node, Status: string(r.Status), Reason: string(r.Reason),
			Output: r.Output, Planned: r.Plan != nil,
		}
		if r.Plan != nil {
			st.Plan = r.Plan
		}
		if r.Element != nil {
			i := r.Element.Index
			st.Index = &i
		}
		if r.Failure != nil {
			st.Failure = r.Failure
		}
		steps = append(steps, st)
		switch r.Status {
		case core.StepSucceeded, core.StepSkipped:
		case core.StepFailed:
			problems = append(problems, stepProblem(r))
		default:
			problems = append(problems, Problem{
				Kind: ProblemRun, Step: label(r), Message: fmt.Sprintf("step %s is %s, not finished", label(r), r.Status),
			})
		}
	}
	if run.Status != core.RunSucceeded && len(problems) == 0 {
		p := Problem{Kind: ProblemRun, Message: fmt.Sprintf("the run ended %s", run.Status)}
		if run.Failure != nil {
			p.Code, p.Message = string(run.Failure.Code), fmt.Sprintf("the run ended %s: %s", run.Status, run.Failure.Message)
		}
		problems = append(problems, p)
	}
	return steps, problems
}

func label(r core.StepRun) string {
	if r.Element != nil {
		return fmt.Sprintf("%s[%d]", r.StepID, r.Element.Index)
	}
	return r.StepID
}

func stepProblem(r core.StepRun) Problem {
	p := Problem{Kind: ProblemStep, Step: label(r), Message: "step " + label(r) + " failed"}
	if f := r.Failure; f != nil {
		p.Code = string(f.Code)
		p.Message = fmt.Sprintf("step %s failed: %s", label(r), f.Message)
		switch f.Code {
		case core.CodeCheckFailed, core.CodeCheckUnavailable:
			p.Kind = ProblemCheck
		case core.CodeOutputInvalid:
			p.Kind = ProblemOutput
		}
	}
	return p
}
