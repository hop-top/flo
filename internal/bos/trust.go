package bos

import (
	"context"

	"hop.top/flo/internal/core"
)

// TrustProvider is the name of the trust provider that is bos.
const TrustProvider = "bos"

// Trust is the core.Trust of the business that runs bos. Its provider `bos`
// (kind command: the bos CLI) is served here; every other provider goes to
// the document-driven port next, so packs keep their own.
//
//	read    `bos agent trust --agent A --class C...`: auto only under an
//	        active standing grant, else ask; counters are bos's own history
//	decide  acknowledges every item and counts nothing: only decisions on bos
//	        proposals count toward graduation (a skipped entry would fail the
//	        flo step, so bos acknowledges)
//	set     refused, trust_capped: graduation is a grant signed in bos
//
// The context of a read names the profile (`agent`) and the action classes
// (`classes`); the engine fills both from the step.
type Trust struct {
	gw   *Gateway
	next core.Trust
}

var _ core.Trust = (*Trust)(nil)

// NewTrust returns the trust port that serves the provider bos and hands the
// rest to next (which may be nil).
func NewTrust(gw *Gateway, next core.Trust) *Trust { return &Trust{gw: gw, next: next} }

// Levels implements core.Trust.
func (t *Trust) Levels(ctx context.Context, tenant core.TenantID, provider string, params map[string]any) ([]core.TrustLevel, error) {
	if provider != TrustProvider {
		return t.other().Levels(ctx, tenant, provider, params)
	}
	agent, _ := params["agent"].(string)
	if agent == "" {
		return nil, core.New(core.CodeUsage, "trust provider bos: the context names no agent (the profile the step runs as)")
	}
	id, ok := t.gw.Config().Principal(agent)
	if !ok {
		return nil, core.Errorf(core.CodeUsage, "trust provider bos: profile %q has no bos principal", agent).
			WithSuggestion("map it in the config key bos.principals")
	}
	return t.gw.TrustLevels(ctx, agent, id, stringList(params["classes"]))
}

// Decide implements core.Trust: bos acknowledges every item and counts
// nothing.
func (t *Trust) Decide(ctx context.Context, tenant core.TenantID, provider string, params map[string]any, decisions []core.TrustDecision) (core.TrustApplied, error) {
	if provider != TrustProvider {
		return t.other().Decide(ctx, tenant, provider, params, decisions)
	}
	out := core.TrustApplied{Applied: make([]string, 0, len(decisions)), Skipped: []string{}}
	for _, d := range decisions {
		out.Applied = append(out.Applied, d.ID)
	}
	return out, nil
}

// Set implements core.Trust: refused.
func (t *Trust) Set(ctx context.Context, tenant core.TenantID, provider, class string, level core.TrustLevelName, reason string) error {
	if provider != TrustProvider {
		return t.other().Set(ctx, tenant, provider, class, level, reason)
	}
	return core.Errorf(core.CodeTrustCapped,
		"trust provider bos refuses to set %s to %s: graduation is a grant signed by owner and compliance in bos", class, level).
		WithSuggestion("graduate the class in bos (bos agent eligible, bos agent grant)")
}

func (t *Trust) other() core.Trust {
	if t.next == nil {
		return noTrust{}
	}
	return t.next
}

// noTrust answers for a port with nothing behind it.
type noTrust struct{}

func (noTrust) Levels(_ context.Context, _ core.TenantID, provider string, _ map[string]any) ([]core.TrustLevel, error) {
	return nil, core.Errorf(core.CodeNotFound, "trust provider %q not found", provider)
}

func (noTrust) Decide(_ context.Context, _ core.TenantID, provider string, _ map[string]any, _ []core.TrustDecision) (core.TrustApplied, error) {
	return core.TrustApplied{}, core.Errorf(core.CodeNotFound, "trust provider %q not found", provider)
}

func (noTrust) Set(_ context.Context, _ core.TenantID, provider, _ string, _ core.TrustLevelName, _ string) error {
	return core.Errorf(core.CodeNotFound, "trust provider %q not found", provider)
}

// stringList reads a list of strings from a decoded context value.
func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
