package builtin

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/llm"
	llmerrors "hop.top/kit/go/ai/llm/errors"
	"hop.top/kit/go/ai/prompt"
	// The adapters of the llm backends. Only stdlib-only adapters are linked;
	// see the generate docs for the schemes.
	_ "hop.top/kit/go/ai/llm/ollama"
)

// PriceFunc returns a model's rate per million tokens; llm.PriceOf is the
// standard one. ok is false when the model's price is unknown.
type PriceFunc func(ctx context.Context, scheme, model string) (llm.Price, bool)

// LLMDeps is what the model-backed backends (the llm generator, the judge
// classifier) run on.
type LLMDeps struct {
	// HTTP carries every request of the model client when the invocation's
	// context has no seam of its own (the dispatcher puts its own there,
	// core.WithHTTP, so a recorded run's tape sees model calls like any
	// other). Nil leaves the client on the process default transport.
	HTTP llm.Doer
	// Secrets holds provider keys. Nil is a store with no secret.
	Secrets core.Secrets
	// LocalHosts is privacy.local_hosts: hosts a local-only ceiling accepts
	// beyond localhost and private addresses.
	LocalHosts []string
	// Price prices a cloud model's usage. Nil is llm.PriceOf.
	Price PriceFunc
	// Patterns resolves the judge's `pattern` input. Nil means no pattern
	// is available: a step that names one fails rather than loses it.
	Patterns prompt.PatternSource
	// JudgeEngine is the engine default of the judge classifier: local, jev
	// or auto (the default when empty). A step's `engine` input overrides it.
	JudgeEngine string
	// Jev builds the Jev judge (kit go/ai/judge/jev) from the OpenRouter key
	// and the transport of the invocation; the key is for Jev's default
	// endpoint only. Nil is a build without Jev: a Jev model is unusable
	// (engine_feature) and auto never falls back to it.
	Jev JevFactory
	// Allow is the model allow-list (`ai.models.allow`): the model URIs a
	// flow may run, each exact or with a trailing `*`. Every model of the
	// model-backed backends must be on it; empty allows none.
	Allow []string
}

// JevFactory builds a Jev judge of the named model (empty is the default one)
// that sends key to Jev's own endpoint over rt (nil is the process default
// transport) and reads no environment or file.
type JevFactory func(key, model string, rt http.RoundTripper) (judge.Judge, error)

// SecretNameOf is the secret that holds the API key of a model scheme, read
// from the secret store as the profile the step acts as: anthropic_api_key
// for anthropic://.
func SecretNameOf(scheme string) string { return scheme + "_api_key" }

// modelClients builds the model client of one invocation. The client is
// hermetic (no environment variable, no llm.yaml), runs over the engine's
// HTTP seam and, under a local-only ceiling, is local-only as well.
type modelClients struct {
	d     LLMDeps
	local *node.LocalHosts
	// probes remembers what the local daemon listed, per run.
	probes probeCache
}

// newModelClients checks d. A privacy.local_hosts entry kit cannot read is a
// usage error here, not at the first call.
func newModelClients(d LLMDeps) (*modelClients, error) {
	if d.Price == nil {
		d.Price = llm.PriceOf
	}
	if _, err := llm.Resolve("ollama://probe", llm.Hermetic(), llm.LocalOnly(llm.AllowHosts(d.LocalHosts...))); err != nil {
		return nil, core.Errorf(core.CodeUsage, "privacy.local_hosts: %v", err)
	}
	local, err := node.NewLocalHosts(d.LocalHosts)
	if err != nil {
		return nil, core.Wrap(core.CodeUsage, err)
	}
	return &modelClients{d: d, local: local}, nil
}

// open resolves the client of model. level is the model's privacy level as
// flo computes it; tenant and profile scope the key read for a cloud model.
func (m *modelClients) open(ctx context.Context, model string, uri llm.URI, level node.Privacy, localOnly bool, tenant core.TenantID, profile string) (*llm.Client, error) {
	full := model
	if _, named := uri.Params["api_key"]; !named && keyed(level, uri) {
		key, err := m.key(ctx, tenant, profile, uri.Scheme)
		if err != nil {
			return nil, err
		}
		sep := "?"
		if strings.Contains(full, "?") {
			sep = "&"
		}
		full += sep + "api_key=" + key
	}
	opts := []llm.ResolveOption{llm.Hermetic(), llm.WithTransport(llm.DoerTransport(m.doer(ctx)))}
	if localOnly {
		opts = append(opts, llm.LocalOnly(llm.AllowHosts(m.d.LocalHosts...)))
	}
	p, err := llm.Resolve(full, opts...)
	if err != nil {
		return nil, resolveErr(err, model)
	}
	if !localOnly {
		return llm.NewClient(p), nil
	}
	c, err := llm.LocalOnlyClient(p)
	if err != nil {
		_ = p.Close()
		return nil, resolveErr(err, model)
	}
	return c, nil
}

// doer is the HTTP seam of one invocation: the dispatcher's on ctx (a
// recorded run's goes through a tape), else the engine's. Nil is the process
// default.
func (m *modelClients) doer(ctx context.Context) llm.Doer {
	if h, ok := core.HTTPFrom(ctx); ok {
		return h
	}
	return m.d.HTTP
}

// keyed reports whether the store's key goes with this model: a cloud model
// on its scheme's own default endpoint. A model that names an endpoint (host
// or base_url) gets none: the key is for the provider, not for a host a flow
// file points at. A local model needs none.
func keyed(level node.Privacy, uri llm.URI) bool {
	if level != node.PrivacyCloud {
		return false
	}
	_, base := uri.Params["base_url"]
	return uri.Host == "" && !base
}

func (m *modelClients) key(ctx context.Context, tenant core.TenantID, profile, scheme string) (string, error) {
	key, err := m.secret(ctx, tenant, profile, scheme)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(key, "&?#") {
		return "", core.Errorf(core.CodeInputInvalid, "secret %q holds a character a model URI cannot carry (& ? #)", SecretNameOf(scheme))
	}
	return key, nil
}

// secret reads the API key of scheme as the profile the step acts as.
func (m *modelClients) secret(ctx context.Context, tenant core.TenantID, profile, scheme string) (string, error) {
	name := SecretNameOf(scheme)
	missing := func() error {
		return core.Errorf(core.CodeSecretNotFound, "secret %q not found for profile %q", name, profile).
			WithSuggestion("store the API key of " + scheme + " as the secret " + name + " of the flow's profile")
	}
	if m.d.Secrets == nil {
		return "", missing()
	}
	v, err := m.d.Secrets.Get(ctx, tenant, profile, name)
	if err != nil {
		return "", err //nolint:wrapcheck // the store's classified error
	}
	key := strings.TrimSpace(string(v))
	if key == "" {
		return "", missing()
	}
	return key, nil
}

// price is the rate to bill a model's usage at: a local model costs nothing
// in dollars, and a cloud model without a known price costs its tokens only.
func (m *modelClients) price(ctx context.Context, level node.Privacy, uri llm.URI) (llm.Price, bool) {
	if level == node.PrivacyLocal {
		return llm.Price{}, false
	}
	return m.d.Price(ctx, uri.Scheme, uri.Model)
}

// costOf prices usage: a local model costs nothing in dollars; a cloud model
// at llm.PriceOf's rate. A model without a known price costs its tokens only.
func (m *modelClients) costOf(ctx context.Context, level node.Privacy, uri llm.URI, u llm.Usage) core.Cost {
	c := core.Cost{Tokens: tokensOf(u)}
	if p, ok := m.price(ctx, level, uri); ok {
		c.USD = u.Cost(p)
	}
	return c
}

func tokensOf(u llm.Usage) int64 {
	if u.TotalTokens != 0 {
		return int64(u.TotalTokens)
	}
	return int64(u.PromptTokens + u.CompletionTokens + u.CacheReadTokens + u.CacheCreationTokens)
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
