package serve

import (
	"context"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
)

// testedGate is the ActivationGate (D7, after the phase-1 "validated" rule):
// a version may be activated when it validates and has a passing test of that
// exact version. The caller has checked the grants.
//
// "Exact version" is the document hash and the digest of the bundle around it
// (nodes/, prompts/, trusts/, tests/, record/): a node spec edited after the
// test is a version the test did not see.
type testedGate struct {
	flows   *Flows
	results *flowtest.Store
}

var _ core.ActivationGate = (*testedGate)(nil)

// Allow implements core.ActivationGate.
func (g *testedGate) Allow(ctx context.Context, tenant core.TenantID, v core.FlowVersion) error {
	ck, err := g.flows.Check(ctx, tenant, Source{File: v.Name + ".yaml", Bundle: v.Bundle, Data: v.Source})
	if err != nil {
		return err
	}
	ref := v.Name + "@" + strings.TrimSpace(v.Version)
	if err := ck.Report.Err(); err != nil {
		return core.Errorf(core.CodeInvalidFlow, "flow %s is not valid, so it cannot be activated: %w", ref, err)
	}
	test := "flo flow test " + v.Name + " --version " + v.Hash
	res, ok, err := g.results.Load(tenant, v.Name, v.Hash)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if !ok {
		return core.Errorf(core.CodeFlowNotTested, "flow %s (%.12s) has no test, so it cannot be activated", ref, v.Hash).
			WithDetail("hash", v.Hash).WithSuggestion(test)
	}
	if !res.Passed {
		return core.Errorf(core.CodeFlowNotTested, "the latest test of flow %s (%.12s) failed (%d problem(s)), so it cannot be activated", ref, v.Hash, len(res.Problems)).
			WithDetail("hash", v.Hash).WithDetail("problems", res.Problems).WithSuggestion(test)
	}
	digest, err := flowtest.Digest(v.Bundle)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if !res.Binds(v.Hash, digest) {
		return core.Errorf(core.CodeFlowNotTested, "the bundle of flow %s (%.12s) changed since its test passed, so it cannot be activated", ref, v.Hash).
			WithDetail("hash", v.Hash).WithSuggestion(test)
	}
	return nil
}
