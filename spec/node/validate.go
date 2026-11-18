package node

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"hop.top/flo/spec"
)

// Codes of node validation. They reuse the engine's vocabulary
// (invalid_node, capability_mismatch, out_of_scope, ...) so one table maps
// them to exit codes; Rule tells the N rules apart.
const (
	// CodeCapabilityMismatch is N07: a capability stronger than side_effect.
	CodeCapabilityMismatch = "capability_mismatch"
	// CodeOutOfScope is N04 and N14: a placeholder reads a namespace the
	// field does not have.
	CodeOutOfScope = "out_of_scope"
	// CodeReservedKey is a reserved value (container isolation, ...).
	CodeReservedKey = "reserved_key"
	// CodePrivacyViolation is a local node placed on a remote runner.
	CodePrivacyViolation = "privacy_violation"
)

// Rule ids of node.md §18 enforced by Check. N01 and N02 are Parse's.
const (
	RuleN03 = "N03" // kind-specific keys only
	RuleN04 = "N04" // declared nodes read only in, config, run
	RuleN05 = "N05" // outputs.schema on declared nodes
	RuleN06 = "N06" // approval consistent with side_effect
	RuleN07 = "N07" // capabilities consistent with side_effect
	RuleN08 = "N08" // trust declared when approval: trust
	RuleN09 = "N09" // idempotency_key only on write-shared, irreversible
	RuleN10 = "N10" // lowers_to: exec only where §14 allows
	RuleN11 = "N11" // record: false only on none nodes
	RuleN12 = "N12" // secrets by reference only
	RuleN13 = "N13" // examples validate against inputs and outputs
	RuleN14 = "N14" // fail_when and map read only out, in, config, run
	RuleN15 = "N15" // rate_limit max positive, per a duration
)

var (
	inputNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	capRe       = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z0-9_-]*$`)
	secretRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Load is Parse followed by Check: the document decodes and every rule of
// node.md §18 holds. The error is Errors.
func Load(r io.Reader, filename string) (*Spec, error) {
	s, err := Parse(r, filename)
	if err != nil {
		return nil, err
	}
	if errs := Check(s); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", filename, errs)
	}
	return s, nil
}

// Validate is Check as an error: nil when the spec is clean, Errors
// otherwise.
func Validate(s *Spec) error {
	if errs := Check(s); len(errs) > 0 {
		return errs
	}
	return nil
}

// Check applies N03..N15 of node.md §18 to a decoded spec (N01 and N02 are
// Parse's) and the structural rules the grammar implies: kind and
// namespace, input definitions, durations, placement. It reports every
// problem, in rule order.
func Check(s *Spec) Errors {
	c := &checker{s: s}
	c.general()
	c.n03()
	c.n04()
	c.n05()
	c.n06()
	c.n07()
	c.n08()
	c.n09()
	c.n10()
	c.n11()
	c.n12()
	c.n13()
	c.n14()
	c.n15()
	return c.errs
}

type checker struct {
	s    *Spec
	errs Errors
}

func (c *checker) add(rule, code, field, format string, args ...any) {
	c.errs = append(c.errs, &Error{Code: code, Rule: rule, Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) invalid(rule, field, format string, args ...any) {
	c.add(rule, CodeInvalidNode, field, format, args...)
}

func (c *checker) declared() bool { return c.s.Kind == KindCommand || c.s.Kind == KindHTTP }

// general holds the structural rules with no N number.
func (c *checker) general() {
	s := c.s
	if t, err := s.Type(); err == nil {
		isFlo := t.NS == BuiltinNS
		switch {
		case s.Kind == KindBuiltin && !isFlo:
			c.invalid("", "kind", "a builtin node lives in the %q namespace, not %q", BuiltinNS, t.NS)
		case s.Kind != KindBuiltin && s.Kind.Valid() && isFlo:
			c.invalid("", "node", "namespace %q is reserved for built-ins (kind %s)", BuiltinNS, s.Kind)
		}
	}
	if s.EffectiveRole() == RoleTrigger && s.Kind != KindPlugin {
		c.invalid("", "role", "a trigger node is a plugin (kind %s)", s.Kind)
	}
	c.defs("inputs", s.Inputs)
	c.defs("config", s.Config)
	for i, cp := range s.Capabilities {
		if !capRe.MatchString(cp) {
			c.invalid(RuleN07, fmt.Sprintf("capabilities[%d]", i), "capability %q: want <domain>.<verb>", cp)
		}
	}
	c.durations()
	if s.Cost != nil && (s.Cost.Estimate.USD < 0 || s.Cost.Estimate.Tokens < 0) {
		c.invalid("", "cost.estimate", "estimates are not negative")
	}
	c.placement()
	c.exitCodes()
	if o := s.Outputs; o.Format != "" && o.Format != "json" && o.Format != "lines" && o.Format != "text" {
		c.invalid("", "outputs.format", "format %q: want json, lines or text", o.Format)
	}
}

func (c *checker) defs(field string, defs map[string]InputDef) {
	for name, d := range defs {
		f := field + "." + name
		if !inputNameRe.MatchString(name) {
			c.invalid("", f, "name %q must match %s", name, inputNameRe)
		}
		c.def(f, d)
		if field == "inputs" && d.Required.Flag && d.HasDefault() {
			c.invalid("", f, "required: true with a default")
		}
	}
}

func (c *checker) def(field string, d InputDef) {
	for _, t := range d.Type {
		if !schemaTypes[t] {
			c.invalid("", field+".type", "unknown type %q", t)
		}
	}
	if d.Minimum != nil && d.Maximum != nil && *d.Minimum > *d.Maximum {
		c.invalid("", field, "minimum %v exceeds maximum %v", *d.Minimum, *d.Maximum)
	}
	switch d.Format {
	case "", "duration", "date-time", "uri":
	default:
		c.invalid("", field+".format", "format %q: want duration, date-time or uri", d.Format)
	}
	if d.HasDefault() {
		for _, msg := range CheckValue(schemaOf(d), d.Default) {
			c.invalid("", field+".default", "%s", msg)
		}
	}
	if d.Items != nil {
		c.def(field+".items", *d.Items)
	}
	for name, p := range d.Properties {
		c.def(field+".properties."+name, p)
	}
}

func (c *checker) durations() {
	s := c.s
	dur := func(field, v string) {
		if v == "" {
			return
		}
		if _, err := spec.ParseDuration(v); err != nil {
			c.invalid("", field, "%v", err)
		}
	}
	dur("timeout", s.Timeout)
	if s.Retry != nil {
		dur("retry.backoff", s.Retry.Backoff)
		if s.Retry.MaxAttempts < 0 {
			c.invalid("", "retry.max_attempts", "must not be negative")
		}
	}
}

func (c *checker) placement() {
	p := c.s.EffectivePlacement()
	switch p.Isolation {
	case IsolationNone, IsolationProcess:
	case IsolationContainer, IsolationPod:
		c.add("", CodeReservedKey, "placement.isolation", "isolation %q is reserved for a later version", p.Isolation)
	default:
		c.invalid("", "placement.isolation", "isolation %q: want none or process", p.Isolation)
	}
	if p.Runner != RunnerLocal && c.s.Privacy == PrivacyLocal {
		c.add("", CodePrivacyViolation, "placement.runner", "a local node cannot be placed on runner %q", p.Runner)
	}
}

func (c *checker) exitCodes() {
	e := c.s.ExitCodes
	if e == nil {
		return
	}
	for _, code := range append(append([]int{}, e.OK...), e.Retry...) {
		if code < 0 || code > 255 {
			c.invalid("", "exit_codes", "exit code %d out of range", code)
		}
	}
	for _, r := range e.Retry {
		for _, ok := range e.OK {
			if r == ok {
				c.invalid("", "exit_codes", "exit code %d is both ok and retry", r)
			}
		}
	}
}

// n03: only the keys of the node's kind.
func (c *checker) n03() {
	s := c.s
	cmd := map[string]bool{
		"argv": len(s.Argv) > 0, "env": s.Env != nil, "cwd": s.Cwd != "", "stdin": s.Stdin != nil,
		"stdout_max": s.StdoutMax != 0, "exit_codes": s.ExitCodes != nil,
	}
	httpKeys := map[string]bool{"request": s.Request != nil, "response": s.Response != nil}
	forbid := func(set map[string]bool, kind string) {
		for _, k := range sortedKeys(set) {
			if set[k] {
				c.add(RuleN03, CodeInvalidNode, k, "%q is only valid on a %s node, not %s", k, kind, s.Kind)
			}
		}
	}
	switch s.Kind {
	case KindCommand:
		forbid(httpKeys, "http")
		if len(s.Argv) == 0 {
			c.invalid(RuleN03, "argv", "a command node requires argv")
		}
	case KindHTTP:
		forbid(cmd, "command")
		c.httpShape()
	case KindBuiltin, KindPlugin:
		forbid(cmd, "command")
		forbid(httpKeys, "http")
	}
}

func (c *checker) httpShape() {
	r := c.s.Request
	if r == nil {
		c.invalid(RuleN03, "request", "an http node requires request")
		return
	}
	switch strings.ToUpper(r.Method) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		c.invalid(RuleN03, "request.method", "method %q: want GET, POST, PUT, PATCH, DELETE or HEAD", r.Method)
	}
	if r.URL == "" {
		c.invalid(RuleN03, "request.url", "url is required")
	}
	bodies := 0
	for _, has := range []bool{r.JSON != nil, r.Form != nil, r.Text != ""} {
		if has {
			bodies++
		}
	}
	if bodies > 1 {
		c.invalid(RuleN03, "request", "at most one of json, form and text is the body")
	}
	if c.s.Response != nil {
		for _, code := range c.s.Response.Success {
			if code < 100 || code > 599 {
				c.invalid(RuleN03, "response.success", "status %d out of range", code)
			}
		}
	}
}

// n05: declared nodes carry an output schema.
func (c *checker) n05() {
	if !c.declared() {
		return
	}
	switch sc := c.s.Outputs.Schema.(type) {
	case nil:
		c.invalid(RuleN05, "outputs.schema", "a declared node requires outputs.schema")
	case string:
		if sc == "" {
			c.invalid(RuleN05, "outputs.schema", "outputs.schema is empty")
		}
	}
}

// n06: an irreversible node cannot waive approval.
func (c *checker) n06() {
	s := c.s
	if s.SideEffect == SideEffectIrreversible && s.Approval == ApprovalNone {
		c.invalid(RuleN06, "approval", "an irreversible node may not declare approval: none")
	}
}

// capability verb classes (node.md §8, §12): the weakest side effect that
// can honestly carry a capability with that verb.
var verbMin = map[string]SideEffect{
	"read": SideEffectRead, "list": SideEffectRead, "get": SideEffectRead, "find": SideEffectRead,
	"search": SideEffectRead, "watch": SideEffectRead,
	"write": SideEffectWriteLocal, "create": SideEffectWriteLocal, "update": SideEffectWriteLocal,
	"label": SideEffectWriteLocal, "archive": SideEffectWriteLocal, "move": SideEffectWriteLocal,
	"send": SideEffectIrreversible, "delete": SideEffectIrreversible, "publish": SideEffectIrreversible,
	"unsubscribe": SideEffectIrreversible, "destroy": SideEffectIrreversible, "purge": SideEffectIrreversible,
}

// n07: a node cannot claim a side effect weaker than its capabilities.
func (c *checker) n07() {
	if !c.s.SideEffect.Valid() {
		return
	}
	for i, cp := range c.s.Capabilities {
		_, verb, ok := strings.Cut(cp, ".")
		if !ok {
			continue
		}
		min, known := verbMin[verb]
		if known && !c.s.SideEffect.AtLeast(min) {
			c.add(RuleN07, CodeCapabilityMismatch, fmt.Sprintf("capabilities[%d]", i),
				"capability %q needs side_effect %s or stronger, the node claims %s", cp, min, c.s.SideEffect)
		}
	}
}

// n08: approval: trust names a trust class.
func (c *checker) n08() {
	s := c.s
	if s.Approval == ApprovalTrust && (s.Trust == nil || strings.TrimSpace(s.Trust.Class) == "") {
		c.invalid(RuleN08, "trust", "approval: trust requires trust.class")
	}
}

// n09: an idempotency key is for nodes that write.
func (c *checker) n09() {
	s := c.s
	if s.IdempotencyKey != "" && s.SideEffect != SideEffectWriteShared && s.SideEffect != SideEffectIrreversible {
		c.invalid(RuleN09, "idempotency_key", "only write-shared and irreversible nodes take an idempotency key, not %s", s.SideEffect)
	}
}

// ExecLowerable reports why a node cannot lower to exec (node.md §14); the
// empty string means it can. Only command nodes with no stdin, output
// straight from stdout, no map, no output check, no trust and a side
// effect at most write-shared qualify.
func ExecLowerable(s *Spec) string {
	switch {
	case s.Kind != KindCommand:
		return fmt.Sprintf("kind %s is not a command", s.Kind)
	case s.Stdin != nil:
		return "stdin needs the engine"
	case s.Outputs.From != "" && s.Outputs.From != "stdout":
		return fmt.Sprintf("outputs.from %q is not stdout", s.Outputs.From)
	case s.Outputs.Map != "":
		return "outputs.map needs the engine"
	case s.Outputs.FailWhen != "":
		return "outputs.fail_when needs the engine"
	case s.Approval == ApprovalTrust || s.Trust != nil:
		return "a trust class needs the engine"
	case customExitCodes(s.ExitCodes):
		return "exit_codes beyond the default need the engine"
	case s.SideEffect.AtLeast(SideEffectIrreversible):
		return "an irreversible node never lowers to exec"
	}
	return ""
}

// n10: lowers_to: exec only where §14 allows.
func (c *checker) n10() {
	if c.s.LowersTo != LowersExec {
		return
	}
	if why := ExecLowerable(c.s); why != "" {
		c.invalid(RuleN10, "lowers_to", "lowers_to: exec not allowed: %s", why)
	}
}

// n11: record: false only on pure nodes.
func (c *checker) n11() {
	if c.s.Record != nil && !*c.s.Record && c.s.SideEffect != SideEffectNone {
		c.invalid(RuleN11, "record", "record: false only on side_effect none, not %s", c.s.SideEffect)
	}
}

func (c *checker) n15() {
	rl := c.s.RateLimit
	if rl == nil {
		return
	}
	if rl.Bucket == "" {
		c.invalid(RuleN15, "rate_limit.bucket", "bucket is required")
	}
	if rl.Max <= 0 {
		c.invalid(RuleN15, "rate_limit.max", "max must be positive, got %d", rl.Max)
	}
	if _, err := spec.ParseDuration(rl.Per); err != nil {
		c.invalid(RuleN15, "rate_limit.per", "per must be a duration: %v", err)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// customExitCodes reports exit_codes beyond the default success of [0]: a
// plain exec step cannot express them.
func customExitCodes(e *ExitCodes) bool {
	if e == nil {
		return false
	}
	defaultOK := len(e.OK) == 0 || (len(e.OK) == 1 && e.OK[0] == 0)
	return len(e.Retry) > 0 || !defaultOK
}
