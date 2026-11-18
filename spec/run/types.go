// Package run types the run-level keys of a flow (docs/spec/flow.md §6, §7):
// triggers, concurrency, budget, state declarations, tasks and privacy.
// The flow parser keeps them as plain maps; Decode turns them into the
// values an engine acts on.
//
// Like the rest of the spec module it imports no engine, no store and no
// kit package.
package run

import (
	"time"
)

// TriggerKind names a trigger (flow.md §6).
type TriggerKind string

// Trigger kinds. Webhook and file are reserved for phase 2.
const (
	TriggerManual   TriggerKind = "manual"
	TriggerSchedule TriggerKind = "schedule"
	TriggerCalendar TriggerKind = "calendar"
	TriggerEvent    TriggerKind = "event"
	TriggerWebhook  TriggerKind = "webhook"
	TriggerFile     TriggerKind = "file"
)

// Valid reports whether k is a defined kind (reserved ones included).
func (k TriggerKind) Valid() bool {
	switch k {
	case TriggerManual, TriggerSchedule, TriggerCalendar, TriggerEvent, TriggerWebhook, TriggerFile:
		return true
	}
	return false
}

// Reserved reports a kind that exists in the grammar but is not run in
// phase 1.
func (k TriggerKind) Reserved() bool { return k == TriggerWebhook || k == TriggerFile }

// Timed reports a kind the engine fires from a clock (a delayed job)
// rather than on a request.
func (k TriggerKind) Timed() bool { return k == TriggerSchedule || k == TriggerCalendar }

// Trigger is one entry of `on:`.
type Trigger struct {
	Kind TriggerKind

	// Schedule.
	RRule  string // RFC 5545 recurrence, required for schedule
	TZ     string // IANA zone, default UTC
	Jitter string // duration, optional

	// Calendar and event.
	Source  string // a trigger node, ns/name@N
	Offset  string // signed duration from the event start, default 0
	When    string // condition over trigger.event, templatable against trigger only
	Horizon string // how far ahead calendar fires are planned, default 7d

	// Options is every other key of the entry: the options of the source
	// trigger node (its `inputs`).
	Options map[string]any
}

// Defaults of flow.md §6.
const (
	DefaultTZ      = "UTC"
	DefaultHorizon = 7 * 24 * time.Hour
)

// OnOverlap says what a new fire does when its concurrency group is full
// (flow.md §7.3).
type OnOverlap string

// Overlap policies.
const (
	OverlapSkip           OnOverlap = "skip"
	OverlapQueue          OnOverlap = "queue"
	OverlapCancelPrevious OnOverlap = "cancel_previous"
)

// Valid reports whether o is a defined policy.
func (o OnOverlap) Valid() bool {
	return o == OverlapSkip || o == OverlapQueue || o == OverlapCancelPrevious
}

// Concurrency is the `concurrency` key.
type Concurrency struct {
	// Group is a template; empty means the flow name.
	Group string
	// Max is how many runs of a group are active at once; default 1.
	Max int
	// OnOverlap is empty when the flow does not say; see OverlapFor.
	OnOverlap OnOverlap
}

// DefaultMax is the default concurrency limit.
const DefaultMax = 1

// OverlapFor resolves the policy for a run started by kind: the flow's own
// value, else skip for schedule triggers and queue for the rest (flow.md
// §7.3).
func (c Concurrency) OverlapFor(kind TriggerKind) OnOverlap {
	if c.OnOverlap != "" {
		return c.OnOverlap
	}
	if kind == TriggerSchedule {
		return OverlapSkip
	}
	return OverlapQueue
}

// Budget is flow.md §7.7. A zero field is unset: no limit.
type Budget struct {
	USD      float64
	Tokens   int64
	Duration time.Duration // wall clock from run start, parked time excluded
	Calls    int
}

// IsZero reports a budget with no limit at all.
func (b Budget) IsZero() bool { return b == Budget{} }

// StateDecl is one declared state key (flow.md §7.4). HasDefault is false
// when the declaration lacks a `default`, which V07 refuses; a default of
// null is a default.
type StateDecl struct {
	Default    any
	HasDefault bool
}

// Tasks is the `tasks` key (flow.md §7.8).
type Tasks string

// Task modes.
const (
	TasksNone    Tasks = "none"
	TasksPerRun  Tasks = "per-run"
	TasksPerStep Tasks = "per-step"
)

// Valid reports whether t is a defined mode.
func (t Tasks) Valid() bool { return t == TasksNone || t == TasksPerRun || t == TasksPerStep }

// Privacy is the egress ceiling (flow.md §7.6).
type Privacy string

// Privacy ceilings.
const (
	PrivacyLocalOnly    Privacy = "local-only"
	PrivacyCloudAllowed Privacy = "cloud-allowed"
)

// Valid reports whether p is a defined ceiling.
func (p Privacy) Valid() bool { return p == PrivacyLocalOnly || p == PrivacyCloudAllowed }

// Narrows reports whether p is no wider than outer: a step value may only
// narrow the flow's (V25).
func (p Privacy) Narrows(outer Privacy) bool {
	return p == outer || p == PrivacyLocalOnly
}

// Retention is the `retention` key (flow.md §7.9): how long data a run left
// behind is kept. A zero field is unset: the engine's default applies.
type Retention struct {
	// Recordings is how long the recordings of the flow's runs
	// (`run create --record`) are kept, counted from when they were made.
	Recordings time.Duration
}

// Bounds of retention.recordings.
const (
	MinRecordingRetention = 24 * time.Hour
	MaxRecordingRetention = 3650 * 24 * time.Hour
)

// RecordingsOr is the retention of recordings: the flow's own, else def.
func (r Retention) RecordingsOr(def time.Duration) time.Duration {
	if r.Recordings > 0 {
		return r.Recordings
	}
	return def
}

// Settings are the run-level keys of a flow, decoded and defaulted.
type Settings struct {
	// RunAs is the aps profile; empty means the profile that started the run.
	RunAs       string
	Privacy     Privacy
	Budget      Budget
	Tasks       Tasks
	Concurrency Concurrency
	Retention   Retention
	State       map[string]StateDecl
	// Triggers is `on:` in order. An absent `on:` decodes to a single
	// manual trigger, as the spec says.
	Triggers []Trigger
}

// Retry is a step's retry policy, durations parsed.
type Retry struct {
	MaxAttempts int // includes the first attempt; at least 1
	Backoff     time.Duration
}

// StepSettings are the run-level keys of one step, decoded.
type StepSettings struct {
	RunAs   string
	Privacy Privacy // empty: inherit the flow's
	Budget  Budget
	Timeout time.Duration // zero: the node spec's default
	// Retry is nil when the step does not say.
	Retry *Retry
	// Task is nil when the step does not say.
	Task *bool
	// ForEachConcurrency is the in-flight bound of a for_each step.
	ForEachConcurrency int
	// OnItemError is "fail" or "skip".
	OnItemError string
}

// Defaults of flow.md §5.6 and §5.10.
const (
	DefaultForEachConcurrency = 4
	DefaultMaxAttempts        = 1
	OnItemErrorFail           = "fail"
	OnItemErrorSkip           = "skip"
)
