package builder_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hop.top/flo/internal/builder"
	"hop.top/flo/internal/core"
)

const baseDoc = "flow: demo\nversion: 0.1.0\nsteps:\n  - id: a\n    uses: flo/output@1\n"

func fenced(doc string) string { return "here you go\n```yaml\n" + doc + "```\n" }

// script answers each call from the next reply and keeps the requests.
type script struct {
	replies []string
	reqs    []builder.Request
}

func (s *script) Propose(_ context.Context, r builder.Request) (builder.Reply, error) {
	s.reqs = append(s.reqs, r)
	if len(s.reqs) > len(s.replies) {
		return builder.Reply{}, errors.New("no more replies")
	}
	return builder.Reply{Text: s.replies[len(s.reqs)-1], Model: "ollama://m", Cost: core.Cost{Tokens: 10}}, nil
}

// validator accepts a document unless it contains the word "bad".
func validator(name, version string) builder.Validator {
	return func(_ context.Context, doc []byte) (builder.Check, error) {
		c := builder.Check{Name: name, Version: version}
		if strings.Contains(string(doc), "bad") {
			c.Errors = []builder.Finding{{Code: "unknown_step", Step: "a", Message: "step a reads a missing step"}}
		}
		return c, nil
	}
}

func input(m builder.Proposer, v builder.Validator) builder.LoopInput {
	return builder.LoopInput{
		Flow: "demo", Version: "0.1.0", Base: []byte(baseDoc), Instruction: "add a step",
		Catalog: "flo/output@1: publish", Model: m, Validate: v,
	}
}

func TestValidOnFirstTry(t *testing.T) {
	doc := "flow: demo\nversion: 0.2.0\nsteps: []\n"
	m := &script{replies: []string{fenced(doc)}}
	res, err := builder.Run(context.Background(), input(m, validator("demo", "0.2.0")))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Doc) != doc || len(res.Attempts) != 1 || len(res.Attempts[0].Errors) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if res.Cost.Tokens != 10 || res.Model != "ollama://m" {
		t.Fatalf("cost/model = %+v %q", res.Cost, res.Model)
	}
	u := m.reqs[0].User
	for _, want := range []string{"add a step", baseDoc, "flo/output@1: publish"} {
		if !strings.Contains(u, want) {
			t.Errorf("first request lacks %q:\n%s", want, u)
		}
	}
	if m.reqs[0].System == "" {
		t.Error("no system prompt")
	}
}

func TestInvalidThenFixed(t *testing.T) {
	bad := "flow: demo\nversion: 0.2.0\n# bad\n"
	good := "flow: demo\nversion: 0.2.0\nsteps: []\n"
	m := &script{replies: []string{fenced(bad), fenced(good)}}
	res, err := builder.Run(context.Background(), input(m, validator("demo", "0.2.0")))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Doc) != good || len(res.Attempts) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Attempts[0].Errors) != 1 || res.Cost.Tokens != 20 {
		t.Fatalf("attempts = %+v cost %+v", res.Attempts, res.Cost)
	}
	retry := m.reqs[1].User
	for _, want := range []string{"step a reads a missing step", "# bad", "unknown_step"} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry lacks %q:\n%s", want, retry)
		}
	}
}

func TestInvalidThreeTimesFails(t *testing.T) {
	bad := "flow: demo\nversion: 0.2.0\n# bad\n"
	// one proposal and three fixes: four calls, then the last errors
	m := &script{replies: []string{fenced(bad), fenced(bad), fenced(bad), fenced(bad), fenced("flow: demo\nversion: 0.3.0\n")}}
	res, err := builder.Run(context.Background(), input(m, validator("demo", "0.2.0")))
	if !core.IsCode(err, core.CodeInvalidFlow) {
		t.Fatalf("err = %v, want invalid_flow", err)
	}
	if len(m.reqs) != 4 || len(res.Attempts) != 4 {
		t.Fatalf("calls = %d attempts = %d, want 4", len(m.reqs), len(res.Attempts))
	}
	var ce *core.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "step a reads a missing step") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := ce.Detail["issues"]; !ok {
		t.Fatalf("details = %v", ce.Detail)
	}
	if res.Cost.Tokens != 40 {
		t.Fatalf("spend of failed attempts lost: %+v", res.Cost)
	}
}

func TestNoYAMLInReplyIsFedBack(t *testing.T) {
	good := "flow: demo\nversion: 0.2.0\nsteps: []\n"
	m := &script{replies: []string{"I cannot do that.", fenced(good)}}
	res, err := builder.Run(context.Background(), input(m, validator("demo", "0.2.0")))
	if err != nil || len(res.Attempts) != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if !strings.Contains(m.reqs[1].User, "yaml") {
		t.Errorf("retry does not ask for a fenced yaml block:\n%s", m.reqs[1].User)
	}
}

func TestBareYAMLReplyAccepted(t *testing.T) {
	good := "flow: demo\nversion: 0.2.0\nsteps: []\n"
	m := &script{replies: []string{good}}
	if _, err := builder.Run(context.Background(), input(m, validator("demo", "0.2.0"))); err != nil {
		t.Fatal(err)
	}
}

func TestNameAndVersionRules(t *testing.T) {
	good := "flow: demo\nversion: 0.2.0\nsteps: []\n"
	// renamed, then unchanged version, then fine
	m := &script{replies: []string{fenced("flow: other\n"), fenced("flow: demo\n"), fenced(good)}}
	v := func(_ context.Context, doc []byte) (builder.Check, error) {
		switch {
		case strings.Contains(string(doc), "flow: other"):
			return builder.Check{Name: "other", Version: "0.2.0"}, nil
		case strings.Contains(string(doc), "version: 0.2.0"):
			return builder.Check{Name: "demo", Version: "0.2.0"}, nil
		}
		return builder.Check{Name: "demo", Version: "0.1.0"}, nil
	}
	res, err := builder.Run(context.Background(), input(m, v))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 3 {
		t.Fatalf("attempts = %d: %+v", len(res.Attempts), res.Attempts)
	}
	if !strings.Contains(res.Attempts[0].Errors[0].Message, `"demo"`) {
		t.Errorf("rename not named: %+v", res.Attempts[0].Errors)
	}
	if !strings.Contains(res.Attempts[1].Errors[0].Message, "version") {
		t.Errorf("unchanged version not named: %+v", res.Attempts[1].Errors)
	}
}

func TestModelErrorStopsTheLoop(t *testing.T) {
	m := &script{}
	res, err := builder.Run(context.Background(), input(m, validator("demo", "0.2.0")))
	if err == nil || core.IsCode(err, core.CodeInvalidFlow) || len(res.Attempts) != 0 {
		t.Fatalf("err = %v attempts %d", err, len(res.Attempts))
	}
}
