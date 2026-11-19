package builtin

import (
	"net/http"

	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/jev"
)

// JevModel is the default Jev model as a model URI; the `jev` shorthand of a
// step's model list stands for it.
const JevModel = jevPrefix + jev.DefaultModel

// KitJev is the JevFactory over kit's go/ai/judge/jev: hermetic (no llm.yaml,
// no environment, no LLM_BASE_URL), the key from the caller, sent to Jev's
// default endpoint only, over rt. An empty model is kit's default one.
func KitJev(key, model string, rt http.RoundTripper) (judge.Judge, error) {
	opts := []jev.Option{jev.Hermetic(), jev.WithAPIKey(key), jev.WithTransport(rt)}
	if model != "" {
		opts = append(opts, jev.WithModel(model))
	}
	return jev.New(opts...)
}
