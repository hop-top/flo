// Package flowcheck applies the validation rules of flow.md §8 that need
// more than the flow document: the node specs a step uses, the sub-flows it
// runs, the grants of the profiles involved. It layers on spec/run (the
// flow-only run-level rules) and returns one Report.
//
// Rules implemented here: V11 (with against inputs or vars), V12 (uses
// resolves, sub-flows pinned, loops and depth refused), V19 (privacy), V20
// (irreversible steps are guarded), V22 (retry on a non-idempotent node,
// warning), V24 (sub-flow within the parent), V25 for run_as, V27 (trigger
// nodes). Phase 1 additionally refuses `kind: agent` steps and sub-flows
// with engine_feature (decision D5), unless Options allow them.
//
// Like the rest of the spec module it imports no engine, no store and no
// kit package.
package flowcheck

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
	"hop.top/flo/spec/node"
	"hop.top/flo/spec/run"
)

// Issue, Report and Severity are the types of spec/run, so one report
// carries the findings of both packages.
type (
	Issue    = run.Issue
	Report   = run.Report
	Severity = run.Severity
)

// Severities.
const (
	SeverityError   = run.SeverityError
	SeverityWarning = run.SeverityWarning
)

// Codes, shared with the engine's vocabulary.
const (
	CodeInvalidFlow           = run.CodeInvalidFlow
	CodeEngineFeature         = "engine_feature"
	CodeNodeNotFound          = node.CodeNodeNotFound
	CodeFlowNotFound          = "flow_not_found"
	CodePrivacyViolation      = "privacy_violation"
	CodeUnguardedIrreversible = "unguarded_irreversible"
	CodeGrantDenied           = "grant_denied"
	CodeCycle                 = "cycle"
)

// MaxSubFlowDepth caps sub-flow nesting (flow.md §5.9).
const MaxSubFlowDepth = 8

// NodeResolver finds node specs and trust providers: *node.Locator.
type NodeResolver interface {
	Locate(ref string) (*node.Entry, error)
	Trust(name string) (*node.TrustEntry, error)
}

// Grants says what a profile may do. It is the narrow view flowcheck needs
// of the grant port; nil Grants skips the checks that need it.
type Grants interface {
	// Capabilities lists what profile holds; ok is false for an unknown
	// profile.
	Capabilities(profile string) (caps []string, ok bool)
}

// Options are what Check resolves against. A nil field skips the rules
// that need it; the flow-only rules always run.
type Options struct {
	// Nodes resolves `uses: ns/name@N` and trigger sources.
	Nodes NodeResolver
	// Flows resolves includes and `uses: name@semver` sub-flows.
	Flows spec.Locator
	// Grants backs V24 capabilities and V25 run_as.
	Grants Grants
	// AllowAgent and AllowSubFlow lift the phase-1 refusals (D5): by
	// default a `kind: agent` step (explicit, or implied by neither kind
	// nor uses) and a sub-flow step are engine_feature.
	AllowAgent   bool
	AllowSubFlow bool
}

// Check validates f: run.Check, then every rule that reads node specs,
// sub-flows or grants. It reports everything it finds; the flow is not
// mutated.
func Check(f *spec.Flow, opts Options) Report {
	c := &checker{f: f, opts: opts, rep: run.Check(f)}
	s, err := run.Decode(f)
	if err != nil {
		s = &run.Settings{Privacy: run.PrivacyLocalOnly, Triggers: nil}
	}
	c.settings = s
	if env, err := cel.EnvForFlow(f); err == nil {
		c.env = env
	}
	c.steps = c.flatten()
	c.byID = map[string]*spec.Step{}
	for i := range c.steps {
		c.byID[c.steps[i].ID] = &c.steps[i]
	}
	c.triggers()
	for i := range c.steps {
		c.step(&c.steps[i])
	}
	sortReport(c.rep)
	return c.rep
}

// sortReport orders issues by rule, then step, then field: stable output.
func sortReport(r Report) {
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].Rule != r[j].Rule {
			return r[i].Rule < r[j].Rule
		}
		if r[i].Step != r[j].Step {
			return r[i].Step < r[j].Step
		}
		return r[i].Field < r[j].Field
	})
}

type checker struct {
	f        *spec.Flow
	opts     Options
	rep      Report
	settings *run.Settings
	env      *cel.Env
	steps    []spec.Step
	byID     map[string]*spec.Step
}

func (c *checker) add(sev Severity, rule, code, step, field, format string, args ...any) {
	c.rep = append(c.rep, Issue{Rule: rule, Code: code, Severity: sev, Step: step, Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) err(rule, code, step, field, format string, args ...any) {
	c.add(SeverityError, rule, code, step, field, format, args...)
}

// flatten lists the task steps to check. With a flow locator, includes and
// repeats are expanded; without, repeat blocks are walked in place and
// includes are skipped.
func (c *checker) flatten() []spec.Step {
	if c.opts.Flows != nil && hasInclude(c.f.Steps) {
		if steps, err := spec.Expand(c.f, c.opts.Flows); err == nil {
			return steps
		} else if spec.IsFlowNotFound(err) {
			c.err("V12", CodeFlowNotFound, "", "include", "%v", err)
		}
	}
	return walk(c.f.Steps, nil)
}

func hasInclude(steps []spec.Step) bool {
	for i := range steps {
		if steps[i].IsInclude() || hasInclude(steps[i].Steps) {
			return true
		}
	}
	return false
}

func walk(steps []spec.Step, out []spec.Step) []spec.Step {
	for i := range steps {
		switch {
		case steps[i].IsRepeat():
			out = walk(steps[i].Steps, out)
		case steps[i].IsInclude():
		default:
			out = append(out, steps[i])
		}
	}
	return out
}

// ceiling is the privacy ceiling in force for a step: its own value, else
// the flow's.
func (c *checker) ceiling(st *spec.Step) run.Privacy {
	if st.Privacy != "" {
		return run.Privacy(st.Privacy)
	}
	return c.settings.Privacy
}

// deps are the step's ancestors' direct edges: depends_on plus the
// results it reads, restricted to steps of this check.
func (c *checker) deps(st *spec.Step) []string {
	ids := slices.Clone(st.DependsOn)
	if c.env != nil {
		ids = append(ids, c.env.ImplicitDeps(st)...)
	} else {
		ids = append(ids, spec.ImplicitDeps(*st)...)
	}
	var out []string
	for _, id := range ids {
		if _, ok := c.byID[id]; ok && id != st.ID && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// ancestors is the transitive closure of deps.
func (c *checker) ancestors(st *spec.Step) []*spec.Step {
	seen := map[string]bool{st.ID: true}
	var out []*spec.Step
	var visit func(*spec.Step)
	visit = func(s *spec.Step) {
		for _, id := range c.deps(s) {
			if seen[id] {
				continue
			}
			seen[id] = true
			a := c.byID[id]
			out = append(out, a)
			visit(a)
		}
	}
	visit(st)
	return out
}

var semverRe = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+){1,2}(-[0-9A-Za-z.-]+)?$`)

// parseSubRef splits a sub-flow reference; ok is false unless it is
// name@semver.
func parseSubRef(ref string) (name, version string, ok bool) {
	name, version, has := strings.Cut(ref, "@")
	return name, version, has && name != "" && semverRe.MatchString(version)
}
