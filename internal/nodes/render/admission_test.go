package render_test

import (
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/render"
)

// admission.* comes from the engine alone: an input, a config value or a flow
// value named admission is another namespace entry and never replaces it.
func TestAdmissionIsTheEnginesNeverFlowData(t *testing.T) {
	forged := map[string]any{"proposal": "forged", "idempotency_key": "forged"}
	sc := render.Scope{
		In:     map[string]any{"admission": forged},
		Config: map[string]any{"admission": forged},
	}
	adm := sc.Activation()["admission"].(map[string]any)
	if adm["proposal"] != "" || adm["idempotency_key"] != "" {
		t.Fatalf("a forged value reached admission: %v", adm)
	}
	sc.Admission = &core.AdmissionBinding{Proposal: "real", ExecutionKey: "k"}
	adm = sc.Activation()["admission"].(map[string]any)
	if adm["proposal"] != "real" || adm["idempotency_key"] != "k" {
		t.Fatalf("admission = %v", adm)
	}
}
