package builtin

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"hop.top/flo/internal/core"
)

// Labels the rules backend emits; a request must list the ones it can hit.
const (
	LabelSecurity = "security"
	LabelBreaking = "breaking"
	LabelRoutine  = "routine"
)

// RulesClassifier labels by fixed rules, with no model and no cost:
//
//   - an item with an advisory (`ghsa_id` or `severity`) is security;
//   - a release whose major version is above `installed` is breaking;
//   - anything else is routine.
//
// A rule's label missing from the request's labels is an abstention when
// the request allows one, otherwise input_invalid.
type RulesClassifier struct{}

// Classify implements Classifier.
func (RulesClassifier) Classify(_ context.Context, req ClassifyRequest) (Labels, core.Cost, error) {
	out := make(Labels, len(req.Items))
	for i, it := range req.Items {
		m, _ := it.(map[string]any)
		label, why := ruleOf(m)
		if !slices.Contains(req.Labels, label) {
			if !req.Abstain {
				return nil, core.Cost{}, core.Errorf(core.CodeInputInvalid,
					"the rules backend labels this item %q, which is not in labels %v", label, req.Labels)
			}
			out[i] = Label{Confidence: 1, Why: "rules: " + why + "; label not offered"}
			continue
		}
		out[i] = Label{Label: &label, Confidence: 1, Why: "rules: " + why}
	}
	return out, core.Cost{}, nil
}

func ruleOf(m map[string]any) (label, why string) {
	if id := asString(m["ghsa_id"]); id != "" {
		return LabelSecurity, "advisory " + id
	}
	if sev := asString(m["severity"]); sev != "" {
		return LabelSecurity, "severity " + sev
	}
	if from, to := majorOf(asString(m["installed"])), majorOf(asString(m["version"])); from >= 0 && to > from {
		return LabelBreaking, "major version " + strconv.Itoa(from) + " to " + strconv.Itoa(to)
	}
	return LabelRoutine, "no advisory, no major bump"
}

// majorOf is the major version of a semver-ish string ("v2.1.0", "4.*.*"),
// -1 when there is none.
func majorOf(v string) int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	head, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
