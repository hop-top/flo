package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/nodes/dispatch"
	"hop.top/flo/internal/runrec"
	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
)

// Scrubbing a test recording (`flo flow test --record`).
//
// What a test records into the bundle (cassettes, trigger payload, plans,
// state seed) may be committed, so it is scrubbed as a run recording is: the
// redact paths of the flow's nodes are dropped, and the secrets the sandbox
// resolved, the FLO_SECRET_* values and the redacted inputs of every node
// dispatched so far are replaced wherever they occur.

// envSecretValues are the values of the FLO_SECRET_* variables.
func envSecretValues() []string {
	var out []string
	for _, kv := range os.Environ() {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, SecretPrefix) {
			out = append(out, val)
		}
	}
	return out
}

// testScrub is the redactor of one recording test.
type testScrub struct {
	base *runrec.Redactor
	sb   *sandbox // its secret log has the secrets resolved so far

	mu     sync.Mutex
	inputs []string // values at the redact paths of dispatched inputs
}

func newTestScrub(paths []string) *testScrub {
	return &testScrub{base: runrec.NewRedactor(paths, envSecretValues()...)}
}

// learn remembers the values a node was given at its redact paths.
func (s *testScrub) learn(in map[string]any) {
	if in == nil {
		return
	}
	vals := s.base.ValuesAt(in)
	s.mu.Lock()
	s.inputs = append(s.inputs, vals...)
	s.mu.Unlock()
}

// redactor is the redactor for what is kept now.
func (s *testScrub) redactor() *runrec.Redactor {
	r := s.base
	if s.sb != nil {
		r = r.With(s.sb.secretValues()...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.With(s.inputs...)
}

// payload is the tape's Scrub.
func (s *testScrub) payload(v any) any { return s.redactor().Payload(v) }

// LiveNode is a node a recording would run for real.
type LiveNode struct {
	Step       string `json:"step"`
	Node       string `json:"node"`
	Kind       string `json:"kind"`
	SideEffect string `json:"side_effect"`
}

// liveNodes are the declared nodes (command, http, plugin: not built-ins) that
// run in a recording and reach real systems: side_effect read or write-local.
// A write-shared or irreversible node is planned, not run; a node with no
// effect (none) computes.
func (e *Engine) liveNodes(v core.FlowVersion, f *spec.Flow) ([]LiveNode, error) {
	uses, err := e.flowSteps(v, f)
	if err != nil {
		return nil, err
	}
	out := []LiveNode{}
	for _, u := range uses {
		sp := u.Spec
		if sp == nil || !dispatch.ExecutesInDryRun(sp.SideEffect) {
			continue
		}
		switch sp.Kind {
		case node.KindCommand, node.KindHTTP, node.KindPlugin:
		default:
			continue
		}
		if sp.SideEffect != node.SideEffectRead && sp.SideEffect != node.SideEffectWriteLocal {
			continue
		}
		out = append(out, LiveNode{Step: u.ID, Node: sp.Node, Kind: string(sp.Kind), SideEffect: string(sp.SideEffect)})
	}
	return out, nil
}

// LiveRefusal is the error of a recording that would run nodes against real
// systems without --allow-live; nil when there are none.
func LiveRefusal(nodes []LiveNode) error {
	if len(nodes) == 0 {
		return nil
	}
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		parts[i] = fmt.Sprintf("%s (%s, %s %s)", n.Step, n.Node, n.Kind, n.SideEffect)
	}
	return core.Errorf(core.CodeUsage, "--record refused without --allow-live: these nodes run for real, against real systems: %s", strings.Join(parts, "; ")).
		WithDetail("live_nodes", nodes).
		WithSuggestion("point the node's profile env at temp dirs (e.g. INBOX_HOME=$(mktemp -d)) so a recording touches nothing real, then pass --allow-live")
}

// scrubResult returns res with its steps, cases and problems (outputs, plans,
// failures, messages) redacted: redact paths dropped, secret values replaced.
// The gate fields (Passed, Hash, Digest, ...) are not touched.
func scrubResult(r *runrec.Redactor, res flowtest.Result) (flowtest.Result, error) {
	var err error
	if res.Steps, err = scrubCopy(r, res.Steps); err != nil {
		return res, err
	}
	if res.Cases, err = scrubCopy(r, res.Cases); err != nil {
		return res, err
	}
	res.Problems, err = scrubCopy(r, res.Problems)
	return res, err
}

// scrubCopy redacts v through its JSON form into a fresh value, so a dropped
// key cannot survive in a reused one.
func scrubCopy[T any](r *runrec.Redactor, v T) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return v, core.Wrap(core.CodeInternal, err)
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return v, core.Wrap(core.CodeInternal, err)
	}
	if b, err = json.Marshal(r.Doc(generic)); err != nil {
		return v, core.Wrap(core.CodeInternal, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return v, core.Wrap(core.CodeInternal, err)
	}
	return out, nil
}
