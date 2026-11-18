package run_test

import (
	"testing"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

func TestInvocationCarriesTheCeiling(t *testing.T) {
	cases := map[string]struct {
		flow string
		want specrun.Privacy
	}{
		"default is local-only": {"", specrun.PrivacyLocalOnly},
		"flow cloud-allowed":    {"privacy: cloud-allowed\n", specrun.PrivacyCloudAllowed},
		"step narrows the flow": {"privacy: cloud-allowed\n", specrun.PrivacyLocalOnly},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newH(t)
			h.reads("t/read@1")
			step := "  - id: a\n    uses: t/read@1\n"
			if name == "step narrows the flow" {
				step += "    privacy: local-only\n"
			}
			id := h.do("flow: priv\nversion: 1.0.0\n"+c.flow+"steps:\n"+step, nil)
			h.wantRun(id, core.RunSucceeded, "")
			calls := h.fx.Runner.CallsFor("a")
			if len(calls) != 1 || calls[0].Privacy != c.want {
				t.Fatalf("privacy = %+v, want %s", calls, c.want)
			}
		})
	}
}
