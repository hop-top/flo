// Package builder is the chat builder's core: a model proposes the full next
// version of a flow from an instruction and the current document, validation
// errors go back to the model (at most MaxFixes times), and the result is kept
// as a draft until it is applied. The package knows no model, engine or store
// of flo's: the proposer and the validator are seams the engine fills.
package builder

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"hop.top/flo/internal/core"
)

// MaxFixes is how many times validation errors go back to the model: one
// proposal and up to this many corrections, then the build fails.
const MaxFixes = 3

//go:embed reference.md
var system string

// Request is one call to the model.
type Request struct{ System, User string }

// Reply is the model's answer.
type Reply struct {
	Text string
	// Model is the model that answered, as a URI without secrets.
	Model string
	Cost  core.Cost
}

// Proposer calls the model.
type Proposer interface {
	Propose(ctx context.Context, r Request) (Reply, error)
}

// Finding is one thing wrong with a proposal.
type Finding struct {
	Rule    string `json:"rule,omitempty" yaml:"rule,omitempty"`
	Code    string `json:"code,omitempty" yaml:"code,omitempty"`
	Step    string `json:"step,omitempty" yaml:"step,omitempty"`
	Field   string `json:"field,omitempty" yaml:"field,omitempty"`
	Message string `json:"message" yaml:"message"`
}

func (f Finding) String() string {
	var b strings.Builder
	if f.Code != "" {
		b.WriteString(f.Code + ": ")
	}
	if f.Step != "" {
		b.WriteString("step " + f.Step + ": ")
	}
	b.WriteString(f.Message)
	return b.String()
}

// Check is what validating a proposal found. Name and Version are the
// document's own, empty when it does not parse.
type Check struct {
	Name, Version string
	Errors        []Finding
	Warnings      []Finding
}

// Validator validates a proposed document. The error is for faults; findings
// about the document go in the Check.
type Validator func(ctx context.Context, doc []byte) (Check, error)

// Attempt is one proposal and what was wrong with it.
type Attempt struct {
	N      int       `json:"n" yaml:"n"`
	Errors []Finding `json:"errors" yaml:"errors"`
}

// LoopInput is what Run needs.
type LoopInput struct {
	// Flow and Version are the base flow's name and document version.
	Flow, Version string
	// Base is the base document; Instruction what the person asked for.
	Base        []byte
	Instruction string
	// Catalog lists the node types the flow may use, one per line.
	Catalog  string
	Model    Proposer
	Validate Validator
}

// LoopResult is what Run built. On failure it still carries the attempts and
// the spend, beside the error.
type LoopResult struct {
	Doc      []byte
	Warnings []Finding
	Attempts []Attempt
	Cost     core.Cost
	Model    string
}

// Run asks the model for the next version and validates it, feeding the errors
// back up to MaxFixes times. It fails with invalid_flow carrying the last
// errors when the model never gets it right.
func Run(ctx context.Context, in LoopInput) (LoopResult, error) {
	var res LoopResult
	var prev string
	var errs []Finding
	for n := 1; n <= 1+MaxFixes; n++ {
		reply, err := in.Model.Propose(ctx, Request{System: system, User: userPrompt(in, prev, errs)})
		if err != nil {
			return res, err //nolint:wrapcheck // the model call's classified error
		}
		res.Cost = res.Cost.Add(reply.Cost)
		res.Model = reply.Model
		doc, ok := Extract(reply.Text)
		att := Attempt{N: n}
		var check Check
		switch {
		case !ok:
			att.Errors = []Finding{{Code: string(core.CodeInvalidFlow), Message: "the reply holds no flow document: answer with the complete flow in one fenced ```yaml block"}}
			prev = reply.Text
		default:
			prev = string(doc)
			if check, err = in.Validate(ctx, doc); err != nil {
				return res, err
			}
			att.Errors = append(att.Errors, check.Errors...)
			if len(att.Errors) == 0 {
				att.Errors = identity(in, check)
			}
		}
		if att.Errors == nil {
			att.Errors = []Finding{}
		}
		res.Attempts = append(res.Attempts, att)
		if len(att.Errors) == 0 {
			res.Doc, res.Warnings = doc, check.Warnings
			return res, nil
		}
		errs = att.Errors
	}
	return res, failure(res.Attempts)
}

// identity holds a proposal to being a next version of the same flow.
func identity(in LoopInput, c Check) []Finding {
	var out []Finding
	if c.Name != in.Flow {
		out = append(out, Finding{Code: string(core.CodeInvalidFlow), Field: "flow",
			Message: fmt.Sprintf("flow is %q: keep the name %q", c.Name, in.Flow)})
	}
	if c.Version == in.Version {
		out = append(out, Finding{Code: string(core.CodeInvalidFlow), Field: "version",
			Message: fmt.Sprintf("version is still %q: change version so this is a new version", in.Version)})
	}
	return out
}

func failure(attempts []Attempt) error {
	last := attempts[len(attempts)-1].Errors
	msg := fmt.Sprintf("the model's proposal is invalid after %d attempts: %s", len(attempts), last[0])
	if n := len(last) - 1; n > 0 {
		msg += fmt.Sprintf(" (and %d more)", n)
	}
	return core.New(core.CodeInvalidFlow, msg).
		WithDetail("issues", last).WithDetail("attempts", len(attempts)).
		WithSuggestion("say the change in other words, name a stronger model with --model, or edit the flow by hand")
}

func userPrompt(in LoopInput, prev string, errs []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Current flow %q, version %s:\n```yaml\n%s```\n\n", in.Flow, in.Version, ensureNL(string(in.Base)))
	fmt.Fprintf(&b, "Available nodes:\n%s\n\n", ensureNL(in.Catalog))
	fmt.Fprintf(&b, "Change to make: %s\n", strings.TrimSpace(in.Instruction))
	if len(errs) > 0 {
		fmt.Fprintf(&b, "\nYour previous proposal:\n```yaml\n%s```\n\nIt was refused:\n", ensureNL(prev))
		for _, e := range errs {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		b.WriteString("\nReply with the corrected, complete flow in one fenced ```yaml block.\n")
	}
	return b.String()
}

func ensureNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// Extract returns the flow document a reply holds: the last fenced block
// tagged yaml (or the last fenced block), else the whole reply when it reads as
// a document. ok is false when there is none.
func Extract(reply string) ([]byte, bool) {
	var blocks, yamls []string
	lines := strings.Split(reply, "\n")
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "```") {
			continue
		}
		tag := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(t, "```")))
		var body []string
		j := i + 1
		for ; j < len(lines) && strings.TrimSpace(lines[j]) != "```"; j++ {
			body = append(body, lines[j])
		}
		if j >= len(lines) { // never closed
			break
		}
		doc := strings.Join(body, "\n") + "\n"
		blocks = append(blocks, doc)
		if tag == "yaml" || tag == "yml" {
			yamls = append(yamls, doc)
		}
		i = j
	}
	switch {
	case len(yamls) > 0:
		return []byte(yamls[len(yamls)-1]), true
	case len(blocks) > 0:
		return []byte(blocks[len(blocks)-1]), true
	}
	if t := strings.TrimSpace(reply); strings.HasPrefix(t, "flow:") {
		return []byte(ensureNL(t)), true
	}
	return nil, false
}
