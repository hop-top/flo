package builtin

import (
	"net/http"

	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/jev"
)

// KitJev is the JevFactory over kit's go/ai/judge/jev: hermetic (no llm.yaml,
// no environment, no LLM_BASE_URL), the key from the caller, sent to Jev's
// default endpoint only, over rt.
func KitJev(key string, rt http.RoundTripper) (judge.Judge, error) {
	return jev.New(jev.Hermetic(), jev.WithAPIKey(key), jev.WithTransport(rt))
}
