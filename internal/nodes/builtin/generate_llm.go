package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
	"hop.top/kit/go/ai/llm"
	llmerrors "hop.top/kit/go/ai/llm/errors"
	// The adapters of the llm backend. Only stdlib-only adapters are linked;
	// see the generate docs for the schemes.
	_ "hop.top/kit/go/ai/llm/ollama"
)

// PriceFunc returns a model's rate per million tokens; llm.PriceOf is the
// standard one. ok is false when the model's price is unknown.
type PriceFunc func(ctx context.Context, scheme, model string) (llm.Price, bool)

// LLMDeps is what the llm backend runs on.
type LLMDeps struct {
	// HTTP carries every request of the model client: the engine's HTTP
	// seam, so recording and fakes see model calls like any other. Nil
	// leaves the client on the process default transport.
	HTTP llm.Doer
	// Secrets holds provider keys. Nil is a store with no secret.
	Secrets core.Secrets
	// LocalHosts is privacy.local_hosts: hosts a local-only ceiling accepts
	// beyond localhost and private addresses.
	LocalHosts []string
	// Price prices a cloud model's usage. Nil is llm.PriceOf.
	Price PriceFunc
}

// LLMGenerator is the llm backend of flo/ai.generate@1: it calls the step's
// model through kit's model client. The client is built per invocation, from
// the model URI alone (hermetic: no environment variable, no llm.yaml), over
// the engine's HTTP seam. Under a local-only ceiling it is built local-only
// as well, so kit refuses any endpoint that is not local, by the same
// rules flo's ModelPrivacy applies.
type LLMGenerator struct{ d LLMDeps }

// NewLLMGenerator checks d and returns the backend. A privacy.local_hosts
// entry kit cannot read is a usage error here, not at the first call.
func NewLLMGenerator(d LLMDeps) (*LLMGenerator, error) {
	if d.Price == nil {
		d.Price = llm.PriceOf
	}
	if _, err := llm.Resolve("ollama://probe", llm.Hermetic(), llm.LocalOnly(llm.AllowHosts(d.LocalHosts...))); err != nil {
		return nil, core.Errorf(core.CodeUsage, "privacy.local_hosts: %v", err)
	}
	return &LLMGenerator{d}, nil
}

// SecretNameOf is the secret that holds the API key of a model scheme, read
// from the secret store as the profile the step acts as: anthropic_api_key
// for anthropic://.
func SecretNameOf(scheme string) string { return scheme + "_api_key" }

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
	client, err := g.client(ctx, req, uri)
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
	cost := g.costOf(ctx, req, uri, usage)
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

// client resolves the model client of one invocation.
func (g *LLMGenerator) client(ctx context.Context, req GenerateRequest, uri llm.URI) (*llm.Client, error) {
	model := req.Model
	if _, named := uri.Params["api_key"]; !named && keyed(req, uri) {
		key, err := g.key(ctx, req, uri.Scheme)
		if err != nil {
			return nil, err
		}
		sep := "?"
		if strings.Contains(model, "?") {
			sep = "&"
		}
		model += sep + "api_key=" + key
	}
	opts := []llm.ResolveOption{llm.Hermetic(), llm.WithTransport(llm.DoerTransport(g.d.HTTP))}
	if req.LocalOnly {
		opts = append(opts, llm.LocalOnly(llm.AllowHosts(g.d.LocalHosts...)))
	}
	p, err := llm.Resolve(model, opts...)
	if err != nil {
		return nil, resolveErr(err, req.Model)
	}
	if !req.LocalOnly {
		return llm.NewClient(p), nil
	}
	c, err := llm.LocalOnlyClient(p)
	if err != nil {
		_ = p.Close()
		return nil, resolveErr(err, req.Model)
	}
	return c, nil
}

// keyed reports whether the store's key goes with this model: a cloud model
// on its scheme's own default endpoint. A model that names an endpoint (host
// or base_url) gets none: the key is for the provider, not for a host a flow
// file points at. A local model needs none.
func keyed(req GenerateRequest, uri llm.URI) bool {
	if req.Level != node.PrivacyCloud {
		return false
	}
	_, base := uri.Params["base_url"]
	return uri.Host == "" && !base
}

func (g *LLMGenerator) key(ctx context.Context, req GenerateRequest, scheme string) (string, error) {
	name := SecretNameOf(scheme)
	missing := func() error {
		return core.Errorf(core.CodeSecretNotFound, "secret %q not found for profile %q", name, req.Profile).
			WithSuggestion("store the API key of " + scheme + " as the secret " + name + " of the flow's profile")
	}
	if g.d.Secrets == nil {
		return "", missing()
	}
	v, err := g.d.Secrets.Get(ctx, req.Tenant, req.Profile, name)
	if err != nil {
		return "", err //nolint:wrapcheck // the store's classified error
	}
	key := strings.TrimSpace(string(v))
	if key == "" {
		return "", missing()
	}
	if strings.ContainsAny(key, "&?#") {
		return "", core.Errorf(core.CodeInputInvalid, "secret %q holds a character a model URI cannot carry (& ? #)", name)
	}
	return key, nil
}

// costOf prices usage: a local model costs nothing in dollars; a cloud model
// at llm.PriceOf's rate. A model without a known price costs its tokens only.
func (g *LLMGenerator) costOf(ctx context.Context, req GenerateRequest, uri llm.URI, u llm.Usage) core.Cost {
	c := core.Cost{Tokens: int64(u.TotalTokens)}
	if c.Tokens == 0 {
		c.Tokens = int64(u.PromptTokens + u.CompletionTokens + u.CacheReadTokens + u.CacheCreationTokens)
	}
	if req.Level == node.PrivacyLocal {
		return c
	}
	if p, ok := g.d.Price(ctx, uri.Scheme, uri.Model); ok {
		c.USD = u.Cost(p)
	}
	return c
}

// resolveErr classifies a failure to build the client.
func resolveErr(err error, model string) error {
	if errors.Is(err, llm.ErrNotLocal) {
		return core.Errorf(core.CodePrivacyViolation, "model %q is not local under a local-only ceiling: %v", llm.RedactURI(model), err).
			WithSuggestion("name a local model (ollama://<model>, a local scheme://host:port/<model>), list the host in privacy.local_hosts, or set privacy: cloud-allowed")
	}
	var pnf *llmerrors.ErrProviderNotFound
	if errors.As(err, &pnf) {
		return core.Errorf(core.CodeInputInvalid, "model %q: %v", llm.RedactURI(model), err).
			WithSuggestion("this build's llm backend serves ollama://; other schemes are not linked in")
	}
	return core.Errorf(core.CodeInputInvalid, "model %q: %v", llm.RedactURI(model), err)
}

// upstreamErr classifies a failure of a model call. A cancelled or expired
// context is the engine's to classify.
func upstreamErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var se *llm.SchemaError
	if errors.As(err, &se) {
		return core.Errorf(core.CodeOutputInvalid, "model output does not match the schema after %d attempts: %s",
			se.Attempts, strings.Join(se.Violations, "; "))
	}
	var cap *llmerrors.ErrCapabilityNotSupported
	if errors.As(err, &cap) {
		return core.Wrap(core.CodeEngineFeature, err).
			WithSuggestion("use a model whose provider supports the request, or drop schema")
	}
	e := core.Wrap(core.CodeUpstreamFailed, err)
	var rl *llmerrors.ErrRateLimit
	if errors.As(err, &rl) {
		e = e.WithTransience(core.TransienceTransient)
	}
	return e
}
