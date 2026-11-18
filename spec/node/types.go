// Package node holds the node specification (docs/spec/node.md): the typed
// document that describes one unit of work a flow step can use, and its
// strict decoder. It is parse-only: resolution layers, pack documents,
// lowering and the N03..N15 rules are separate files; nothing here runs a
// node.
//
// Like the rest of the spec module it imports no engine, no store and no
// kit package.
package node

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind says how a node runs (node.md §2).
type Kind string

// Node kinds.
const (
	KindCommand Kind = "command" // declared: argv under aps
	KindHTTP    Kind = "http"    // declared: one HTTP call
	KindBuiltin Kind = "builtin" // engine code, spec shipped with the engine
	KindPlugin  Kind = "plugin"  // code behind kit's extension interface
)

// Valid reports whether k is one of the four kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindCommand, KindHTTP, KindBuiltin, KindPlugin:
		return true
	}
	return false
}

// Role says where a node may be used: as a flow step or as a trigger source.
type Role string

// Node roles.
const (
	RoleStep    Role = "step"
	RoleTrigger Role = "trigger"
)

// LowersTo says what a node becomes when a flow is planned (node.md §14).
type LowersTo string

// Lowering targets.
const (
	LowersExec   LowersTo = "exec"
	LowersEngine LowersTo = "engine"
)

// SideEffect is the side-effect ladder of node.md §8, weakest first.
type SideEffect string

// Side-effect classes.
const (
	SideEffectNone         SideEffect = "none"
	SideEffectRead         SideEffect = "read"
	SideEffectWriteLocal   SideEffect = "write-local"
	SideEffectWriteShared  SideEffect = "write-shared"
	SideEffectIrreversible SideEffect = "irreversible"
)

var sideEffectRank = map[SideEffect]int{
	SideEffectNone: 0, SideEffectRead: 1, SideEffectWriteLocal: 2,
	SideEffectWriteShared: 3, SideEffectIrreversible: 4,
}

// Valid reports whether s is on the ladder.
func (s SideEffect) Valid() bool { _, ok := sideEffectRank[s]; return ok }

// Rank orders the ladder: none 0, read 1, write-local 2, write-shared 3,
// irreversible 4. An invalid class ranks as irreversible, the safe side.
func (s SideEffect) Rank() int {
	if r, ok := sideEffectRank[s]; ok {
		return r
	}
	return sideEffectRank[SideEffectIrreversible]
}

// AtLeast reports whether s is at or above other on the ladder.
func (s SideEffect) AtLeast(other SideEffect) bool { return s.Rank() >= other.Rank() }

// Privacy is where a node's data goes (node.md §9).
type Privacy string

// Privacy levels.
const (
	PrivacyLocal Privacy = "local"
	PrivacyCloud Privacy = "cloud"
)

// Valid reports whether p is local or cloud.
func (p Privacy) Valid() bool { return p == PrivacyLocal || p == PrivacyCloud }

// Approval is the approval requirement of a node (node.md §10.1).
type Approval string

// Approval requirements.
const (
	ApprovalNone     Approval = "none"
	ApprovalRequired Approval = "required"
	ApprovalTrust    Approval = "trust"
)

// Valid reports whether a is one of the three values.
func (a Approval) Valid() bool {
	return a == ApprovalNone || a == ApprovalRequired || a == ApprovalTrust
}

// CostClass is the cost class of a node (node.md §13).
type CostClass string

// Cost classes.
const (
	CostFree      CostClass = "free"
	CostLow       CostClass = "low"
	CostMetered   CostClass = "metered"
	CostExpensive CostClass = "expensive"
)

// Valid reports whether c is a defined class.
func (c CostClass) Valid() bool {
	switch c {
	case CostFree, CostLow, CostMetered, CostExpensive:
		return true
	}
	return false
}

// Isolation levels of node.md §15. Phase 1 runs none and process.
const (
	IsolationNone      = "none"
	IsolationProcess   = "process"
	IsolationContainer = "container" // reserved
	IsolationPod       = "pod"       // reserved
)

// RunnerLocal is the only runner class of phase 1.
const RunnerLocal = "local"

// Spec is one node specification document. The kind-specific blocks
// (Argv.., Request, Response) are set only for their kind; N03 checks that.
type Spec struct {
	Node        string   `yaml:"node" json:"node"`
	Kind        Kind     `yaml:"kind" json:"kind"`
	Role        Role     `yaml:"role,omitempty" json:"role,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	LowersTo    LowersTo `yaml:"lowers_to,omitempty" json:"lowers_to,omitempty"`

	// Config is the static settings an operator sets for a pack.
	Config  map[string]InputDef `yaml:"config,omitempty" json:"config,omitempty"`
	Inputs  map[string]InputDef `yaml:"inputs" json:"inputs"`
	Outputs OutputDef           `yaml:"outputs" json:"outputs"`

	Capabilities []string   `yaml:"capabilities" json:"capabilities"`
	SideEffect   SideEffect `yaml:"side_effect" json:"side_effect"`
	Privacy      Privacy    `yaml:"privacy" json:"privacy"`
	Approval     Approval   `yaml:"approval,omitempty" json:"approval,omitempty"`
	Trust        *TrustRef  `yaml:"trust,omitempty" json:"trust,omitempty"`

	// Idempotent is nil when the spec does not say; see EffectiveIdempotent.
	Idempotent     *bool  `yaml:"idempotent,omitempty" json:"idempotent,omitempty"`
	IdempotencyKey string `yaml:"idempotency_key,omitempty" json:"idempotency_key,omitempty"`

	Cost      *Cost      `yaml:"cost,omitempty" json:"cost,omitempty"`
	Timeout   string     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	RateLimit *RateLimit `yaml:"rate_limit,omitempty" json:"rate_limit,omitempty"`
	ExitCodes *ExitCodes `yaml:"exit_codes,omitempty" json:"exit_codes,omitempty"`
	Retry     *Retry     `yaml:"retry,omitempty" json:"retry,omitempty"`
	Placement *Placement `yaml:"placement,omitempty" json:"placement,omitempty"`

	// Record is nil when the spec does not say.
	Record *bool `yaml:"record,omitempty" json:"record,omitempty"`

	// Command nodes (§4).
	Argv      []string          `yaml:"argv,omitempty" json:"argv,omitempty"`
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Cwd       string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Stdin     *Stdin            `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	StdoutMax int               `yaml:"stdout_max,omitempty" json:"stdout_max,omitempty"`

	// HTTP nodes (§5).
	Request  *Request  `yaml:"request,omitempty" json:"request,omitempty"`
	Response *Response `yaml:"response,omitempty" json:"response,omitempty"`

	Examples []Example `yaml:"examples,omitempty" json:"examples,omitempty"`

	// Redact lists input and output paths dropped from cassettes and logs (§9).
	Redact []string `yaml:"redact,omitempty" json:"redact,omitempty"`

	// Ext holds the x-* keys of the document root, preserved on rewrite.
	Ext map[string]any `yaml:"-" json:"-"`

	// Path and Hash are set by Parse: the file name given and the SHA-256
	// of the raw bytes.
	Path string `yaml:"-" json:"-"`
	Hash string `yaml:"-" json:"-"`
}

// EffectiveIdempotent applies the default of node.md §11: true for none and
// read, false for every other class, unless the spec says.
func (s *Spec) EffectiveIdempotent() bool {
	if s.Idempotent != nil {
		return *s.Idempotent
	}
	return s.SideEffect == SideEffectNone || s.SideEffect == SideEffectRead
}

// EffectiveRole returns Role, defaulting to step.
func (s *Spec) EffectiveRole() Role {
	if s.Role == "" {
		return RoleStep
	}
	return s.Role
}

// EffectiveLowersTo applies the default of node.md §1.1: exec for command
// nodes, engine for the rest.
func (s *Spec) EffectiveLowersTo() LowersTo {
	if s.LowersTo != "" {
		return s.LowersTo
	}
	if s.Kind == KindCommand {
		return LowersExec
	}
	return LowersEngine
}

// EffectiveApproval applies the default of node.md §10.1: required for
// irreversible nodes, none otherwise.
func (s *Spec) EffectiveApproval() Approval {
	if s.Approval != "" {
		return s.Approval
	}
	if s.SideEffect == SideEffectIrreversible {
		return ApprovalRequired
	}
	return ApprovalNone
}

// EffectivePlacement applies the default of node.md §15: local, none.
func (s *Spec) EffectivePlacement() Placement {
	p := Placement{Runner: RunnerLocal, Isolation: IsolationNone}
	if s.Placement != nil {
		if s.Placement.Runner != "" {
			p.Runner = s.Placement.Runner
		}
		if s.Placement.Isolation != "" {
			p.Isolation = s.Placement.Isolation
		}
	}
	return p
}

// EffectiveRecord applies the default of node.md §1.1: true unless the spec
// says (built-ins decide per node).
func (s *Spec) EffectiveRecord() bool {
	if s.Record != nil {
		return *s.Record
	}
	return true
}

// Type returns the parsed node type, or an error when Node is malformed.
func (s *Spec) Type() (Type, error) { return ParseType(s.Node) }

// InputDef is the JSON-Schema subset of node.md §3.1, used for inputs and
// for config entries.
type InputDef struct {
	Type        TypeList            `yaml:"type,omitempty" json:"type,omitempty"`
	Required    Required            `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any                 `yaml:"default,omitempty" json:"default,omitempty"`
	Enum        []any               `yaml:"enum,omitempty" json:"enum,omitempty"`
	Minimum     *float64            `yaml:"minimum,omitempty" json:"minimum,omitempty"`
	Maximum     *float64            `yaml:"maximum,omitempty" json:"maximum,omitempty"`
	Items       *InputDef           `yaml:"items,omitempty" json:"items,omitempty"`
	Properties  map[string]InputDef `yaml:"properties,omitempty" json:"properties,omitempty"`
	Format      string              `yaml:"format,omitempty" json:"format,omitempty"`
	Description string              `yaml:"description,omitempty" json:"description,omitempty"`
}

// Required is the `required` of an input: a flag on the input itself
// (`required: true`), or, inside an object schema, the list of property
// names that must be present (`required: [a, b]`).
type Required struct {
	Flag  bool
	Names []string
}

// UnmarshalYAML accepts a bool or a list of names.
func (r *Required) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("line %d: required is true, false or a list of names", n.Line)
		}
		*r = Required{Flag: b}
		return nil
	case yaml.SequenceNode:
		var names []string
		if err := n.Decode(&names); err != nil {
			return fmt.Errorf("line %d: required names are strings", n.Line)
		}
		*r = Required{Names: names}
		return nil
	}
	return fmt.Errorf("line %d: required is true, false or a list of names", n.Line)
}

// MarshalYAML writes the bool or the list.
func (r Required) MarshalYAML() (any, error) {
	if r.Names != nil {
		return r.Names, nil
	}
	return r.Flag, nil
}

// IsZero reports an absent `required`, so omitempty drops it.
func (r Required) IsZero() bool { return !r.Flag && len(r.Names) == 0 }

// MarshalJSON writes the bool or the list.
func (r Required) MarshalJSON() ([]byte, error) {
	if r.Names != nil {
		return json.Marshal(r.Names)
	}
	return json.Marshal(r.Flag)
}

// UnmarshalJSON reads the bool or the list.
func (r *Required) UnmarshalJSON(b []byte) error {
	var names []string
	if err := json.Unmarshal(b, &names); err == nil {
		*r = Required{Names: names}
		return nil
	}
	var flag bool
	if err := json.Unmarshal(b, &flag); err != nil {
		return fmt.Errorf("required is true, false or a list of names")
	}
	*r = Required{Flag: flag}
	return nil
}

// HasDefault reports whether the definition carries a default. A default
// of null is the same as none.
func (d InputDef) HasDefault() bool { return d.Default != nil }

// TypeList is a JSON-Schema `type`: one name or a list of names.
type TypeList []string

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (t *TypeList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*t = TypeList{n.Value}
		return nil
	case yaml.SequenceNode:
		out := make(TypeList, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: type entries are names", c.Line)
			}
			out = append(out, c.Value)
		}
		*t = out
		return nil
	}
	return fmt.Errorf("line %d: type is a name or a list of names", n.Line)
}

// OutputDef is node.md §3.2.
type OutputDef struct {
	From     string `yaml:"from,omitempty" json:"from,omitempty"`
	Format   string `yaml:"format,omitempty" json:"format,omitempty"` // json | lines | text
	Map      string `yaml:"map,omitempty" json:"map,omitempty"`
	FailWhen string `yaml:"fail_when,omitempty" json:"fail_when,omitempty"`
	// Schema is a JSON Schema: an inline mapping, or a file name (string).
	Schema any `yaml:"schema,omitempty" json:"schema,omitempty"`
	// DryRun is the value a write-shared or irreversible node returns when
	// it does not execute (node.md §8).
	DryRun any `yaml:"dry_run,omitempty" json:"dry_run,omitempty"`
}

// TrustRef is the `trust` key of a node: the trust class of its action.
type TrustRef struct {
	Class    string `yaml:"class" json:"class"` // string or a placeholder over `in`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
}

// Cost is node.md §13.
type Cost struct {
	Class    CostClass    `yaml:"class" json:"class"`
	Estimate CostEstimate `yaml:"estimate,omitempty" json:"estimate,omitempty"`
}

// CostEstimate is the per-call estimate.
type CostEstimate struct {
	USD    float64 `yaml:"usd,omitempty" json:"usd,omitempty"`
	Tokens int64   `yaml:"tokens,omitempty" json:"tokens,omitempty"`
}

// RateLimit is node.md §13.1: a provider quota shared by name.
type RateLimit struct {
	Bucket string `yaml:"bucket" json:"bucket"`
	Max    int    `yaml:"max" json:"max"`
	Per    string `yaml:"per" json:"per"` // duration
}

// ExitCodes is node.md §4.1: success codes, and codes that fail transiently.
type ExitCodes struct {
	OK    []int `yaml:"ok,omitempty" json:"ok,omitempty"`
	Retry []int `yaml:"retry,omitempty" json:"retry,omitempty"`
}

// Retry is the node's default retry policy.
type Retry struct {
	MaxAttempts int    `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	Backoff     string `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

// Placement is node.md §15.
type Placement struct {
	Runner    string `yaml:"runner,omitempty" json:"runner,omitempty"`
	Isolation string `yaml:"isolation,omitempty" json:"isolation,omitempty"`
}

// Stdin is the `stdin` of a command node: JSON text or plain text.
type Stdin struct {
	JSON any    `yaml:"json,omitempty" json:"json,omitempty"`
	Text string `yaml:"text,omitempty" json:"text,omitempty"`
}

// Request is the `request` of an HTTP node (node.md §5.1). One of JSON,
// Form and Text is the body.
type Request struct {
	Method  string            `yaml:"method" json:"method"`
	URL     string            `yaml:"url" json:"url"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Query   map[string]any    `yaml:"query,omitempty" json:"query,omitempty"`
	JSON    any               `yaml:"json,omitempty" json:"json,omitempty"`
	Form    map[string]any    `yaml:"form,omitempty" json:"form,omitempty"`
	Text    string            `yaml:"text,omitempty" json:"text,omitempty"`
	Auth    *Auth             `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// Auth names a secret by reference (node.md §5.4); the value never appears
// in a spec.
type Auth struct {
	Secret string `yaml:"secret" json:"secret"`
	Scheme string `yaml:"scheme" json:"scheme"` // bearer | basic | header:<Name>
}

// Response is node.md §5.2.
type Response struct {
	Success []int `yaml:"success,omitempty" json:"success,omitempty"`
}

// Example is a documented call: input, and the output it yields. It also
// serves as a fixture and as a dry-run output.
type Example struct {
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	In   any    `yaml:"in,omitempty" json:"in,omitempty"`
	Out  any    `yaml:"out,omitempty" json:"out,omitempty"`
}

// Type is a parsed node type `ns/name@N` (node.md §2).
type Type struct {
	NS    string
	Name  string
	Major int
}

// String renders the canonical `ns/name@N`.
func (t Type) String() string { return t.NS + "/" + t.Name + "@" + strconv.Itoa(t.Major) }

// Ref renders `ns/name` without the major.
func (t Type) Ref() string { return t.NS + "/" + t.Name }

var (
	nsRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$`)
)

// BuiltinNS is the namespace reserved for built-ins.
const BuiltinNS = "flo"

// ParseType parses `ns/name@N`. N is a positive integer; ns and name follow
// node.md §2.
func ParseType(s string) (Type, error) {
	ref, major, ok := strings.Cut(s, "@")
	if !ok {
		return Type{}, fmt.Errorf("node type %q: want ns/name@N", s)
	}
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return Type{}, fmt.Errorf("node type %q: want ns/name@N", s)
	}
	if !nsRe.MatchString(ns) {
		return Type{}, fmt.Errorf("node type %q: namespace %q must match %s", s, ns, nsRe)
	}
	if !nameRe.MatchString(name) {
		return Type{}, fmt.Errorf("node type %q: name %q must match %s", s, name, nameRe)
	}
	n, err := strconv.Atoi(major)
	if err != nil || n < 1 || strconv.Itoa(n) != major {
		return Type{}, fmt.Errorf("node type %q: major %q must be a positive integer", s, major)
	}
	return Type{NS: ns, Name: name, Major: n}, nil
}
