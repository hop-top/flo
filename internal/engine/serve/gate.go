package serve

import (
	"context"
	"strings"

	"hop.top/flo/internal/core"
)

// validatedGate is the ActivationGate of phase 1 (D7): a version may be
// activated when it validates. The caller has checked the grants; the testing
// work replaces this with "has a passing test of this exact version".
type validatedGate struct{ flows *Flows }

var _ core.ActivationGate = (*validatedGate)(nil)

// Allow implements core.ActivationGate.
func (g *validatedGate) Allow(ctx context.Context, tenant core.TenantID, v core.FlowVersion) error {
	ck, err := g.flows.Check(ctx, tenant, Source{File: v.Name + ".yaml", Bundle: v.Bundle, Data: v.Source})
	if err != nil {
		return err
	}
	if err := ck.Report.Err(); err != nil {
		return core.Errorf(core.CodeInvalidFlow, "flow %s@%s is not valid, so it cannot be activated: %w", v.Name, strings.TrimSpace(v.Version), err)
	}
	return nil
}
