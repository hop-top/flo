package builtin

import (
	"context"
	"encoding/json"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/kit/go/ai/llm"
)

// LLMGenerator is the llm backend of flo/ai.generate@1: it calls the step's
// model through kit's model client. The client is built per invocation, from
// the model URI alone (hermetic: no environment variable, no llm.yaml), over
// the engine's HTTP seam. Under a local-only ceiling it is built local-only
// as well, so kit refuses any endpoint that is not local, by the same
// rules flo's ModelPrivacy applies.
type LLMGenerator struct{ m *modelClients }

// NewLLMGenerator checks d and returns the backend. A privacy.local_hosts
// entry kit cannot read is a usage error here, not at the first call.
func NewLLMGenerator(d LLMDeps) (*LLMGenerator, error) {
	m, err := newModelClients(d)
	if err != nil {
		return nil, err
	}
	return &LLMGenerator{m}, nil
}

// Generate implements Generator.
func (g *LLMGenerator) Generate(ctx context.Context, req GenerateRequest) (Generation, core.Cost, error) {
	uri, err := llm.ParseURI(req.Model)
	if err != nil {
		return Generation{}, core.Cost{}, core.Errorf(core.CodeInputInvalid, "model %q is not a model URI", llm.RedactURI(req.Model)).
			WithSuggestion("write scheme://<model>, e.g. ollama://gemma4:e4b")
	}
	msgs, err := messagesOf(req)
	if err != nil {
		return Generation{}, core.Cost{}, err
	}
	client, err := g.m.open(ctx, req.Model, uri, req.Level, req.LocalOnly, req.Tenant, req.Profile)
	if err != nil {
		return Generation{}, core.Cost{}, err
	}
	defer client.Provider().Close() //nolint:errcheck // adapters hold nothing open between calls

	llmReq := llm.Request{Messages: msgs}
	var got llm.Response
	var data json.RawMessage
	var usage llm.Usage
	if req.Schema != nil {
		raw, merr := json.Marshal(req.Schema)
		if merr != nil {
			return Generation{}, core.Cost{}, core.Errorf(core.CodeInputInvalid, "schema: %v", merr)
		}
		var res llm.StructuredResult
		res, err = llm.CompleteStructured(ctx, client, llmReq, llm.ResponseFormat{Schema: raw})
		got, data, usage = res.Response, res.Data, res.Usage
	} else {
		got, err = client.Complete(ctx, llmReq)
		usage = got.Usage
	}
	cost := g.m.costOf(ctx, req.Level, uri, usage)
	if err != nil {
		return Generation{}, cost, upstreamErr(err)
	}
	out := Generation{Text: got.Content}
	if data != nil {
		if err := json.Unmarshal(data, &out.Data); err != nil {
			return Generation{}, cost, core.Errorf(core.CodeOutputInvalid, "model data is not an object: %v", err)
		}
	}
	return out, cost, nil
}

// messagesOf is the prompt's system text, when it has one, then the rendered
// template.
func messagesOf(req GenerateRequest) ([]llm.Message, error) {
	text, err := req.Prompt.Render(req.Context)
	if err != nil {
		return nil, err //nolint:wrapcheck // the prompt's own classified error
	}
	var msgs []llm.Message
	if sys := strings.TrimSpace(req.Prompt.System); sys != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: req.Prompt.System})
	}
	return append(msgs, llm.Message{Role: "user", Content: text}), nil
}
