package run

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"hop.top/flo/spec"
)

// CodeInvalidFlow is the code of every decode error: the document is
// well-formed YAML but a run-level key does not follow flow.md.
const CodeInvalidFlow = "invalid_flow"

// Error is one problem with one run-level key.
type Error struct {
	Code  string
	Field string // key path, e.g. "budget.usd", "on[1]"
	Msg   string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s (%s)", e.Msg, e.Code)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Field, e.Msg, e.Code)
}

func errf(field, format string, args ...any) *Error {
	return &Error{Code: CodeInvalidFlow, Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Decode types the run-level keys of f: on, run_as, privacy, budget, tasks,
// concurrency, retention and state. Absent keys take their defaults. It checks shape
// (types, key names, duration syntax, enum membership) and stops at the
// first problem; the cross-field rules (V06, V07, V16, V25, V26...) belong
// to validation.
func Decode(f *spec.Flow) (*Settings, error) {
	s := &Settings{
		RunAs:       f.RunAs,
		Privacy:     PrivacyLocalOnly,
		Tasks:       TasksPerRun,
		Concurrency: Concurrency{Max: DefaultMax},
	}
	if f.Privacy != "" {
		s.Privacy = Privacy(f.Privacy)
		if !s.Privacy.Valid() {
			return nil, errf("privacy", "%q: want local-only or cloud-allowed", f.Privacy)
		}
	}
	if f.Tasks != "" {
		s.Tasks = Tasks(f.Tasks)
		if !s.Tasks.Valid() {
			return nil, errf("tasks", "%q: want none, per-run or per-step", f.Tasks)
		}
	}
	var err error
	if s.Budget, err = decodeBudget("budget", f.Budget); err != nil {
		return nil, err
	}
	if s.Concurrency, err = decodeConcurrency(f.Concurrency); err != nil {
		return nil, err
	}
	if s.Retention, err = decodeRetention(f.Retention); err != nil {
		return nil, err
	}
	if s.State, err = decodeState(f.State); err != nil {
		return nil, err
	}
	if s.Triggers, err = decodeTriggers(f.On); err != nil {
		return nil, err
	}
	return s, nil
}

// DecodeStep types the run-level keys of one step: run_as, privacy, budget,
// timeout, retry, task, concurrency and on_item_error. Absent keys stay at
// their zero value, except the for_each keys which take their defaults.
func DecodeStep(st *spec.Step) (StepSettings, error) {
	out := StepSettings{
		RunAs:              st.RunAs,
		Task:               st.Task,
		ForEachConcurrency: DefaultForEachConcurrency,
		OnItemError:        OnItemErrorFail,
	}
	field := func(name string) string { return "steps." + st.ID + "." + name }
	if st.Privacy != "" {
		out.Privacy = Privacy(st.Privacy)
		if !out.Privacy.Valid() {
			return out, errf(field("privacy"), "%q: want local-only or cloud-allowed", st.Privacy)
		}
	}
	var err error
	if out.Budget, err = decodeBudget(field("budget"), st.Budget); err != nil {
		return out, err
	}
	if st.Timeout != "" {
		if out.Timeout, err = parseDur(field("timeout"), st.Timeout); err != nil {
			return out, err
		}
	}
	if st.Retry != nil {
		r := &Retry{MaxAttempts: st.Retry.MaxAttempts}
		if r.MaxAttempts == 0 {
			r.MaxAttempts = DefaultMaxAttempts
		}
		if r.MaxAttempts < 1 {
			return out, errf(field("retry.max_attempts"), "%d: want at least 1", st.Retry.MaxAttempts)
		}
		if st.Retry.Backoff != "" {
			if r.Backoff, err = parseDur(field("retry.backoff"), st.Retry.Backoff); err != nil {
				return out, err
			}
		}
		out.Retry = r
	}
	if st.Concurrency != 0 {
		if st.Concurrency < 1 {
			return out, errf(field("concurrency"), "%d: want at least 1", st.Concurrency)
		}
		out.ForEachConcurrency = st.Concurrency
	}
	if st.OnItemError != "" {
		if st.OnItemError != OnItemErrorFail && st.OnItemError != OnItemErrorSkip {
			return out, errf(field("on_item_error"), "%q: want fail or skip", st.OnItemError)
		}
		out.OnItemError = st.OnItemError
	}
	return out, nil
}

// strictInto decodes a generic value into a struct, refusing unknown keys.
func strictInto(in, out any) error {
	b, err := yaml.Marshal(in)
	if err != nil {
		return err //nolint:wrapcheck // plain marshal failure, caller adds the field
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err //nolint:wrapcheck // caller adds the field
	}
	return nil
}

func parseDur(field, v string) (time.Duration, error) {
	d, err := spec.ParseDuration(v)
	if err != nil {
		return 0, errf(field, "%v", err)
	}
	return d, nil
}

func decodeBudget(field string, m map[string]any) (Budget, error) {
	if len(m) == 0 {
		return Budget{}, nil
	}
	var raw struct {
		USD      float64 `yaml:"usd"`
		Tokens   int64   `yaml:"tokens"`
		Duration string  `yaml:"duration"`
		Calls    int     `yaml:"calls"`
	}
	if err := strictInto(m, &raw); err != nil {
		return Budget{}, errf(field, "%v", err)
	}
	if raw.USD < 0 || raw.Tokens < 0 || raw.Calls < 0 {
		return Budget{}, errf(field, "limits are not negative")
	}
	b := Budget{USD: raw.USD, Tokens: raw.Tokens, Calls: raw.Calls}
	if raw.Duration != "" {
		d, err := parseDur(field+".duration", raw.Duration)
		if err != nil {
			return Budget{}, err
		}
		b.Duration = d
	}
	return b, nil
}

func decodeConcurrency(m map[string]any) (Concurrency, error) {
	c := Concurrency{Max: DefaultMax}
	if len(m) == 0 {
		return c, nil
	}
	var raw struct {
		Group     string `yaml:"group"`
		Max       int    `yaml:"max"`
		OnOverlap string `yaml:"on_overlap"`
	}
	if err := strictInto(m, &raw); err != nil {
		return c, errf("concurrency", "%v", err)
	}
	c.Group = raw.Group
	if raw.Max != 0 {
		if raw.Max < 1 {
			return c, errf("concurrency.max", "%d: want at least 1", raw.Max)
		}
		c.Max = raw.Max
	}
	if raw.OnOverlap != "" {
		c.OnOverlap = OnOverlap(raw.OnOverlap)
		if !c.OnOverlap.Valid() {
			return c, errf("concurrency.on_overlap", "%q: want skip, queue or cancel_previous", raw.OnOverlap)
		}
	}
	return c, nil
}

func decodeRetention(m map[string]any) (Retention, error) {
	if len(m) == 0 {
		return Retention{}, nil
	}
	var raw struct {
		Recordings string `yaml:"recordings"`
	}
	if err := strictInto(m, &raw); err != nil {
		return Retention{}, errf("retention", "%v", err)
	}
	if raw.Recordings == "" {
		return Retention{}, nil
	}
	d, err := parseDur("retention.recordings", raw.Recordings)
	if err != nil {
		return Retention{}, err
	}
	if d < MinRecordingRetention || d > MaxRecordingRetention {
		return Retention{}, errf("retention.recordings", "%q: want between 1d and 3650d", raw.Recordings)
	}
	return Retention{Recordings: d}, nil
}

func decodeState(m map[string]any) (map[string]StateDecl, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]StateDecl, len(m))
	for key, v := range m {
		field := "state." + key
		decl, ok := v.(map[string]any)
		if !ok {
			return nil, errf(field, "a state key is declared as { default: <value> }")
		}
		d := StateDecl{}
		for k, val := range decl {
			if k != "default" {
				return nil, errf(field, "unknown key %q (want default)", k)
			}
			d.Default, d.HasDefault = val, true
		}
		out[key] = d
	}
	return out, nil
}

func decodeTriggers(on []map[string]any) ([]Trigger, error) {
	if len(on) == 0 {
		return []Trigger{{Kind: TriggerManual}}, nil
	}
	out := make([]Trigger, 0, len(on))
	for i, entry := range on {
		field := fmt.Sprintf("on[%d]", i)
		if len(entry) != 1 {
			return nil, errf(field, "an entry is a mapping with one key naming the trigger kind")
		}
		var kind string
		var body any
		for k, v := range entry {
			kind, body = k, v
		}
		t := Trigger{Kind: TriggerKind(kind)}
		if !t.Kind.Valid() {
			return nil, errf(field, "unknown trigger kind %q", kind)
		}
		if body != nil {
			m, ok := body.(map[string]any)
			if !ok {
				return nil, errf(field+"."+kind, "options are a mapping")
			}
			if err := fillTrigger(&t, m, field+"."+kind); err != nil {
				return nil, err
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// known option keys per trigger kind; every other key of calendar and
// event entries is an option of the source node.
func fillTrigger(t *Trigger, m map[string]any, field string) error {
	str := func(key string) (string, error) {
		v, ok := m[key]
		if !ok || v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", errf(field+"."+key, "want a string")
		}
		return s, nil
	}
	var err error
	switch t.Kind {
	case TriggerSchedule:
		if err := onlyKeys(m, field, "rrule", "tz", "jitter"); err != nil {
			return err
		}
		if t.RRule, err = str("rrule"); err != nil {
			return err
		}
		if t.TZ, err = str("tz"); err != nil {
			return err
		}
		if t.TZ == "" {
			t.TZ = DefaultTZ
		}
		t.Jitter, err = str("jitter")
	case TriggerCalendar, TriggerEvent:
		if t.Source, err = str("source"); err != nil {
			return err
		}
		if t.Offset, err = str("offset"); err != nil {
			return err
		}
		if t.When, err = str("when"); err != nil {
			return err
		}
		if t.Horizon, err = str("horizon"); err != nil {
			return err
		}
		for k, v := range m {
			switch k {
			case "source", "offset", "when", "horizon":
			default:
				if t.Options == nil {
					t.Options = map[string]any{}
				}
				t.Options[k] = v
			}
		}
	default:
		if len(m) > 0 && t.Kind == TriggerManual {
			return errf(field, "manual takes no options")
		}
	}
	return err
}

func onlyKeys(m map[string]any, field string, allowed ...string) error {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var bad []string
	for k := range m {
		if !ok[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return errf(field, "unknown key %q (want %s)", strings.Join(bad, ", "), strings.Join(allowed, ", "))
}
