package run

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"hop.top/vstar/rrule"

	"hop.top/flo/spec"
)

// Severity of an Issue. Warnings never refuse a flow.
type Severity string

// Severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Codes of run-level validation. They reuse the engine's vocabulary.
const (
	CodeReservedKey = "reserved_key"
)

// Issue is one validation finding: the flow.md §8 rule that found it, a
// stable code, and where.
type Issue struct {
	// Rule is the V rule id (V07, V16, ...); empty for a shape error of a
	// run-level key that has no rule of its own.
	Rule     string
	Code     string
	Severity Severity
	// Step is the step id, empty for a flow-level key.
	Step  string
	Field string
	Msg   string
}

func (i Issue) Error() string {
	var b strings.Builder
	if i.Rule != "" {
		b.WriteString(i.Rule + ": ")
	}
	if i.Step != "" {
		b.WriteString("step " + i.Step + ": ")
	}
	if i.Field != "" {
		b.WriteString(i.Field + ": ")
	}
	fmt.Fprintf(&b, "%s (%s)", i.Msg, i.Code)
	if i.Severity == SeverityWarning {
		b.WriteString(" [warning]")
	}
	return b.String()
}

// Report is a list of issues. Error joins the errors; warnings do not make
// a report fail.
type Report []Issue

// Errors returns the issues that refuse a flow.
func (r Report) Errors() Report { return r.bySeverity(SeverityError) }

// Warnings returns the issues that do not.
func (r Report) Warnings() Report { return r.bySeverity(SeverityWarning) }

func (r Report) bySeverity(s Severity) Report {
	var out Report
	for _, i := range r {
		if i.Severity == s {
			out = append(out, i)
		}
	}
	return out
}

// HasErrors reports whether any issue is an error.
func (r Report) HasErrors() bool { return len(r.Errors()) > 0 }

// Err returns nil when there are no errors, else an error listing them.
// It unwraps to the Issues.
func (r Report) Err() error {
	errs := r.Errors()
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func (r Report) Error() string {
	parts := make([]string, len(r))
	for i, is := range r {
		parts[i] = is.Error()
	}
	return strings.Join(parts, "\n")
}

// Unwrap exposes the issues to errors.As.
func (r Report) Unwrap() []error {
	out := make([]error, len(r))
	for i := range r {
		out[i] = r[i]
	}
	return out
}

// Has reports whether the report holds an issue of the rule.
func (r Report) Has(rule string) bool {
	for _, i := range r {
		if i.Rule == rule {
			return true
		}
	}
	return false
}

// Check applies the flow-only run-level rules of flow.md §8, those that
// need no node spec:
//
//   - V05 a var default has the declared type;
//   - V07 every state key has a default;
//   - V16 durations parse and budget, retry, timeout, retention are well-formed;
//   - V17 at most one step writes a state key, a written key is declared,
//     and `outputs` has only `state`;
//   - V25 a step's privacy only narrows the flow's;
//   - V26 schedule.rrule parses and tz is an IANA name.
//
// A run-level key that does not decode (Decode) is reported once, with the
// rule that owns it. The flow is not mutated.
func Check(f *spec.Flow) Report {
	c := &checker{f: f}
	c.varDefaults()
	s, err := Decode(f)
	if err != nil {
		c.decodeFailure(err)
		s = &Settings{Privacy: PrivacyLocalOnly}
	}
	c.stateDecls(f)
	c.triggers(s)
	c.walk(f.Steps, s, 1, nil)
	return c.out
}

type checker struct {
	f   *spec.Flow
	out Report
	// writers maps a state key to the step that writes it.
	writers map[string]string
}

func (c *checker) add(rule, code, step, field, format string, args ...any) {
	c.out = append(c.out, Issue{Rule: rule, Code: code, Severity: SeverityError, Step: step, Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) varDefaults() {
	names := make([]string, 0, len(c.f.Vars))
	for n := range c.f.Vars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		def := c.f.Vars[n]
		if def.Default == nil {
			continue
		}
		if err := CheckVarType(def.Type, def.Default); err != nil {
			c.add("V05", CodeInvalidFlow, "", "vars."+n+".default", "default %v", err)
		}
	}
}

// decodeFailure reports a Decode error under the rule that owns the key.
func (c *checker) decodeFailure(err error) {
	e, ok := err.(*Error) //nolint:errorlint // Decode returns *Error directly
	if !ok {
		c.add("", CodeInvalidFlow, "", "", "%v", err)
		return
	}
	rule := ""
	switch {
	case strings.HasPrefix(e.Field, "budget") || strings.HasPrefix(e.Field, "retention") || strings.HasSuffix(e.Field, "jitter") ||
		strings.HasSuffix(e.Field, "offset") || strings.HasSuffix(e.Field, "horizon"):
		rule = "V16"
	}
	c.add(rule, e.Code, "", e.Field, "%s", e.Msg)
}

func (c *checker) stateDecls(f *spec.Flow) {
	keys := make([]string, 0, len(f.State))
	for k := range f.State {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m, ok := f.State[k].(map[string]any)
		if !ok {
			continue // Decode reported the shape
		}
		if _, has := m["default"]; !has {
			c.add("V07", CodeInvalidFlow, "", "state."+k, "state key %q has no default; declare { default: <value> }", k)
		}
	}
}

func (c *checker) triggers(s *Settings) {
	for i, t := range s.Triggers {
		f := fmt.Sprintf("on[%d].%s", i, t.Kind)
		if t.Kind.Reserved() {
			c.add("V04", CodeReservedKey, "", f, "trigger %q is reserved for a later version of the flow spec", t.Kind)
			continue
		}
		switch t.Kind {
		case TriggerSchedule:
			c.schedule(f, t)
		case TriggerCalendar, TriggerEvent:
			if t.Source == "" {
				c.add("V27", CodeInvalidFlow, "", f+".source", "a %s trigger names a trigger node as its source", t.Kind)
			}
			c.duration("V16", f+".offset", strings.TrimLeft(t.Offset, "+-"))
			c.duration("V16", f+".horizon", t.Horizon)
		}
	}
}

func (c *checker) duration(rule, field, v string) {
	if v == "" {
		return
	}
	if _, err := spec.ParseDuration(v); err != nil {
		c.add(rule, CodeInvalidFlow, "", field, "%v", err)
	}
}

// schedule is V26: the RRULE parses and the zone is an IANA name.
func (c *checker) schedule(field string, t Trigger) {
	if strings.TrimSpace(t.RRule) == "" {
		c.add("V26", CodeInvalidFlow, "", field+".rrule", "a schedule trigger requires rrule")
	} else if err := rrule.ValidateRRule(t.RRule); err != nil {
		c.add("V26", CodeInvalidFlow, "", field+".rrule", "rrule %q: %v", t.RRule, err)
	}
	if _, err := time.LoadLocation(t.TZ); err != nil || strings.EqualFold(t.TZ, "local") {
		c.add("V26", CodeInvalidFlow, "", field+".tz", "tz %q is not an IANA zone name", t.TZ)
	}
	c.duration("V16", field+".jitter", t.Jitter)
}

// walk visits steps recursively. mult is how many times the step's list is
// unrolled (the product of the enclosing repeat counts).
func (c *checker) walk(steps []spec.Step, s *Settings, mult int, outer *Privacy) {
	for i := range steps {
		st := &steps[i]
		if st.IsRepeat() {
			m := mult
			if st.Repeat > 1 {
				m *= st.Repeat
			}
			c.walk(st.Steps, s, m, outer)
			continue
		}
		if st.IsInclude() {
			continue // expanded by flowcheck, which knows the locator
		}
		c.step(st, s, mult)
	}
}

func (c *checker) step(st *spec.Step, s *Settings, mult int) {
	ss, err := DecodeStep(st)
	if err != nil {
		e, _ := err.(*Error) //nolint:errorlint // DecodeStep returns *Error directly
		msg := err.Error()
		field := ""
		if e != nil {
			msg, field = e.Msg, strings.TrimPrefix(e.Field, "steps."+st.ID+".")
		}
		c.add("V16", CodeInvalidFlow, st.ID, field, "%s", msg)
	}
	if ss.Privacy != "" && !ss.Privacy.Narrows(s.Privacy) {
		c.add("V25", CodeInvalidFlow, st.ID, "privacy", "step privacy %s widens the flow's %s; a step may only narrow", ss.Privacy, s.Privacy)
	}
	c.stateWrites(st, mult)
}

// stateWrites is V17 for one step: only `state` under outputs, declared
// keys, one writer per key.
func (c *checker) stateWrites(st *spec.Step, mult int) {
	if len(st.Outputs) == 0 {
		return
	}
	for k := range st.Outputs {
		if k != "state" {
			c.add("V17", CodeInvalidFlow, st.ID, "outputs."+k, "outputs has only state, not %q", k)
		}
	}
	m, ok := st.Outputs["state"].(map[string]any)
	if st.Outputs["state"] != nil && !ok {
		c.add("V17", CodeInvalidFlow, st.ID, "outputs.state", "outputs.state maps a state key to a placeholder")
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if c.writers == nil {
		c.writers = map[string]string{}
	}
	for _, k := range keys {
		field := "outputs.state." + k
		if _, declared := c.f.State[k]; !declared {
			c.add("V17", CodeInvalidFlow, st.ID, field, "writes undeclared state key %q; declare it under state", k)
		}
		if prev, dup := c.writers[k]; dup {
			c.add("V17", CodeInvalidFlow, st.ID, field, "state key %q is also written by step %s; one writer per key", k, prev)
		} else if mult > 1 {
			c.add("V17", CodeInvalidFlow, st.ID, field, "state key %q is written inside a repeat block of %d iterations; one writer per key", k, mult)
		}
		c.writers[k] = st.ID
	}
}
