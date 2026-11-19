package spec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Flow is one flow document: a graph of steps. At the plan level it is
// expanded, rendered and materialized by the implementer; the spec module
// itself never executes anything.
type Flow struct {
	Name        string            `yaml:"flow" json:"flow"`
	Version     string            `yaml:"version" json:"version"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Requires    Requires          `yaml:"requires,omitempty" json:"requires,omitempty"`
	Vars        map[string]VarDef `yaml:"vars,omitempty" json:"vars,omitempty"`
	Agent       string            `yaml:"agent,omitempty" json:"agent,omitempty"`
	Track       *Track            `yaml:"track,omitempty" json:"track,omitempty"`
	Steps       []Step            `yaml:"steps" json:"steps"`

	// Run-level keys (flow spec §1.3). The spec module parses and keeps
	// them so a document that uses them loads, but gives them no meaning:
	// a plan-level tool ignores or refuses them, the engine interprets
	// them. They are deliberately untyped here.
	On          []map[string]any  `yaml:"on,omitempty" json:"on,omitempty"`
	State       map[string]any    `yaml:"state,omitempty" json:"state,omitempty"`
	RunAs       string            `yaml:"run_as,omitempty" json:"run_as,omitempty"`
	Privacy     string            `yaml:"privacy,omitempty" json:"privacy,omitempty"`
	Budget      map[string]any    `yaml:"budget,omitempty" json:"budget,omitempty"`
	Tasks       string            `yaml:"tasks,omitempty" json:"tasks,omitempty"`
	Concurrency map[string]any    `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	Returns     map[string]string `yaml:"returns,omitempty" json:"returns,omitempty"`
	Retention   map[string]any    `yaml:"retention,omitempty" json:"retention,omitempty"`

	// Path is the file the flow was parsed from, Source the directory or
	// layer it was located in, Hash the sha256 of the raw bytes. None of
	// them is part of the document.
	Path   string `yaml:"-" json:"-"`
	Source string `yaml:"-" json:"-"`
	Hash   string `yaml:"-" json:"-"`
}

// Key is the pinned reference form, name@version.
func (r *Flow) Key() string { return r.Name + "@" + r.Version }

// Requires declares what a flow must be applied to.
type Requires struct {
	Subject string `yaml:"subject,omitempty" json:"subject,omitempty"` // task | track | none
}

// Subject kinds a flow can require.
const (
	SubjectNone  = "none"
	SubjectTask  = "task"
	SubjectTrack = "track"
)

// VarDef declares a flow variable. Required and Default are mutually
// exclusive: a default makes the var optional.
type VarDef struct {
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Type is the declared type (flow spec §3): string (default), integer,
	// number, boolean, duration, list, map.
	Type     string `yaml:"type,omitempty" json:"type,omitempty"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Default  any    `yaml:"default,omitempty" json:"default,omitempty"`
}

// Track is what a plan-level tool builds a track from; the engine ignores it.
type Track struct {
	Title string `yaml:"title,omitempty" json:"title,omitempty"`
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Plan  string `yaml:"plan,omitempty" json:"plan,omitempty"`
}

// Step is one entry of the ordered steps list. A step is exactly one
// of: a task (default), an include of another flow, or a repeat block
// over nested steps. Ordinal is the 1-based position after expansion.
type Step struct {
	ID          string   `yaml:"id" json:"id"`
	Kind        Kind     `yaml:"kind,omitempty" json:"kind,omitempty"`
	Title       string   `yaml:"title,omitempty" json:"title,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	DependsOn   []string `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	When        string   `yaml:"when,omitempty" json:"when,omitempty"`
	Agent       string   `yaml:"agent,omitempty" json:"agent,omitempty"`
	Assignee    string   `yaml:"assignee,omitempty" json:"assignee,omitempty"`
	Due         DueSpec  `yaml:"due,omitempty" json:"due,omitempty"`

	Exec  *ExecSpec  `yaml:"exec,omitempty" json:"exec,omitempty"`
	Human *HumanSpec `yaml:"human,omitempty" json:"human,omitempty"`
	Retry *RetrySpec `yaml:"retry,omitempty" json:"retry,omitempty"`
	Check *Check     `yaml:"check,omitempty" json:"check,omitempty"`

	// Node reference (flow spec §5.2): ns/name@N for a node, name@version
	// for a sub-flow. Exclusive with Kind. With binds its inputs.
	Uses string `yaml:"uses,omitempty" json:"uses,omitempty"`

	// Run-level step keys, kept untyped like the flow-level ones.
	ForEach     string         `yaml:"for_each,omitempty" json:"for_each,omitempty"`
	OnItemError string         `yaml:"on_item_error,omitempty" json:"on_item_error,omitempty"`
	Concurrency int            `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	Outputs     map[string]any `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	Timeout     string         `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	RunAs       string         `yaml:"run_as,omitempty" json:"run_as,omitempty"`
	Privacy     string         `yaml:"privacy,omitempty" json:"privacy,omitempty"`
	Budget      map[string]any `yaml:"budget,omitempty" json:"budget,omitempty"`
	Task        *bool          `yaml:"task,omitempty" json:"task,omitempty"`
	Runner      string         `yaml:"runner,omitempty" json:"runner,omitempty"`

	// Composition: include another flow, binding its vars through With.
	Include string         `yaml:"include,omitempty" json:"include,omitempty"`
	With    map[string]any `yaml:"with,omitempty" json:"with,omitempty"`

	// Repetition: unroll Steps Repeat times; Until stops early at dispatch
	// time through a derived `when` on every iteration after the first.
	Repeat int    `yaml:"repeat,omitempty" json:"repeat,omitempty"`
	Until  string `yaml:"until,omitempty" json:"until,omitempty"`
	Steps  []Step `yaml:"steps,omitempty" json:"steps,omitempty"`

	Ordinal int `yaml:"-" json:"ordinal,omitempty"`
}

// IsInclude reports whether the step composes another flow.
func (s *Step) IsInclude() bool { return s.Include != "" }

// IsRepeat reports whether the step is an unrolled block.
func (s *Step) IsRepeat() bool { return s.Repeat > 0 || len(s.Steps) > 0 }

// IsUses reports whether the step references a node or sub-flow.
func (s *Step) IsUses() bool { return s.Uses != "" }

// IsNode reports whether the step's uses names a node (a "/" before the
// "@") rather than a sub-flow (flow spec §5.2).
func (s *Step) IsNode() bool {
	ref, _, _ := strings.Cut(s.Uses, "@")
	return s.Uses != "" && strings.Contains(ref, "/")
}

// IsBlock reports whether the step expands into other steps rather than
// becoming a task itself.
func (s *Step) IsBlock() bool { return s.IsInclude() || s.IsRepeat() }

// EffectiveKind resolves an unset kind to agent.
func (s *Step) EffectiveKind() Kind {
	if s.Kind == "" {
		return KindAgent
	}
	return s.Kind
}

// DueSpec is a step's due date in either form:
//
//	due: "{{sprint_end}}"                     # Raw: any string the due parser accepts,
//	due: "{{steps.planning.due}} + 5d"        # optionally followed by an offset
//	due: {after: planning, offset: 5d}        # structured: another step's due plus an offset
//
// Raw holds the scalar form and, after rendering, the resolved value of
// either form.
type DueSpec struct {
	Raw    string `yaml:"-" json:"-"`
	After  string `yaml:"after,omitempty" json:"after,omitempty"`
	Offset string `yaml:"offset,omitempty" json:"offset,omitempty"`
}

// IsZero reports whether no due was given; it lets yaml omitempty skip it.
func (d DueSpec) IsZero() bool { return d.Raw == "" && d.After == "" && d.Offset == "" }

// UnmarshalYAML accepts a scalar or an {after, offset} mapping.
func (d *DueSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		d.Raw = value.Value
		return nil
	case yaml.MappingNode:
		type structured struct {
			After  string `yaml:"after"`
			Offset string `yaml:"offset"`
		}
		var s structured
		if err := dueMappingKnownFields(value); err != nil {
			return err
		}
		if err := value.Decode(&s); err != nil {
			return fmt.Errorf("due: %w", err)
		}
		d.After, d.Offset = s.After, s.Offset
		return nil
	default:
		return fmt.Errorf("line %d: due must be a string or {after, offset}", value.Line)
	}
}

// MarshalYAML emits the scalar form when set, else the structured form.
func (d DueSpec) MarshalYAML() (any, error) {
	if d.Raw != "" {
		return d.Raw, nil
	}
	return map[string]string{"after": d.After, "offset": d.Offset}, nil
}

// UnmarshalJSON mirrors UnmarshalYAML for JSON flows.
func (d *DueSpec) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &d.Raw) //nolint:wrapcheck // caller wraps with the field name
	}
	type structured struct {
		After  string `json:"after"`
		Offset string `json:"offset"`
	}
	var s structured
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("due: %w", err)
	}
	d.After, d.Offset = s.After, s.Offset
	return nil
}

// MarshalJSON mirrors MarshalYAML.
func (d DueSpec) MarshalJSON() ([]byte, error) {
	if d.Raw != "" {
		return json.Marshal(d.Raw) //nolint:wrapcheck // plain string, nothing to add
	}
	return json.Marshal(map[string]string{"after": d.After, "offset": d.Offset}) //nolint:wrapcheck // plain map
}

func dueMappingKnownFields(value *yaml.Node) error {
	for i := 0; i+1 < len(value.Content); i += 2 {
		if k := value.Content[i].Value; k != dueKeyAfter && k != dueKeyOffset {
			return fmt.Errorf("line %d: due: unknown key %q (want after, offset)", value.Content[i].Line, k)
		}
	}
	return nil
}

// Grammar shared by the parser, validator and selector.
var (
	// flowNameRe bounds flow names: file-name and reference safe.
	flowNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// StepIDRe bounds authored step ids; "/" is reserved for expansion.
	StepIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// varNameRe bounds var names to identifiers so {{name}} is unambiguous.
	varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// reservedVarNames are template namespaces a flow cannot shadow.
var reservedVarNames = map[string]bool{
	nsSubject: true, nsResults: true, nsRun: true, nsSteps: true, nsVars: true,
	nsTrigger: true, nsState: true, nsItem: true, nsIndex: true, nsConfig: true,
	nsIn: true, nsResult: true, nsDecisions: true, nsAdmission: true,
}

// engineNamespaces are bound by the engine at run start or dispatch. A
// plan-level tool cannot resolve them: validation accepts a reference to
// one, rendering leaves it verbatim, and the plan tool refuses the flow
// (flow spec §9).
var engineNamespaces = map[string]bool{
	nsTrigger: true, nsState: true, nsItem: true, nsIndex: true, nsConfig: true,
	nsIn: true, nsResult: true, nsDecisions: true, nsAdmission: true,
}

// Template namespaces: {{<namespace>.<field>}}. A bare {{name}} is a var.
const (
	nsVars    = "vars"
	nsSubject = "subject"
	nsRun     = "run"
	nsSteps   = "steps"
	nsResults = "results"

	nsTrigger   = "trigger"
	nsState     = "state"
	nsItem      = "item"
	nsIndex     = "index"
	nsConfig    = "config"
	nsIn        = "in"
	nsResult    = "result"
	nsDecisions = "decisions"
	// nsAdmission is bound by the engine from the admission backend's
	// approval (node.md §10.2); flow data cannot set or shadow it.
	nsAdmission = "admission"
)

// Field names shared by the template scope, the validator and the
// rendered-step view.
const (
	fieldID          = "id"
	fieldTitle       = "title"
	fieldDescription = "description"
	fieldDue         = "due"
	fieldAssignee    = "assignee"
	fieldWhen        = "when"
	fieldAgent       = "agent"
	fieldKind        = "kind"
	fieldIteration   = "iteration"
	fieldStartedAt   = "started_at"
	fieldTrack       = "track"
	fieldTags        = "tags"
	fieldProject     = "project"
	fieldFlow        = "flow"
	fieldVersion     = "version"
)

// Keys of the structured due form.
const (
	dueKeyAfter  = "after"
	dueKeyOffset = "offset"
)

// subjectFields are the values auto-bound from a subject task or track.
var subjectFields = map[string]bool{
	fieldID: true, fieldTitle: true, fieldDescription: true, fieldTrack: true, fieldTags: true, fieldProject: true,
}

// runFields are the values bound from the run itself (flow spec §4.2).
// "iteration" is only bound inside a repeat block.
var runFields = map[string]bool{
	fieldID: true, fieldFlow: true, fieldVersion: true, fieldStartedAt: true, fieldIteration: true,
}

// stepFields are the rendered values another step may read through
// steps.<id>.<field>.
var stepFields = map[string]bool{
	fieldID: true, fieldTitle: true, fieldDescription: true, fieldDue: true,
	fieldAssignee: true, fieldWhen: true, fieldAgent: true, fieldKind: true,
}
