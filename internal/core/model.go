package core

import (
	"time"

	specrun "hop.top/flo/spec/run"
)

// RunStatus is where a run stands (flow.md §7.1):
//
//	queued -> running -> (awaiting_approval <-> running) -> succeeded | failed | cancelled
//
// A run awaiting approval is parked: no worker holds it and it survives a
// restart. budget_exceeded is a failed run with that failure code.
type RunStatus string

// Run statuses.
const (
	RunQueued           RunStatus = "queued"
	RunRunning          RunStatus = "running"
	RunAwaitingApproval RunStatus = "awaiting_approval"
	RunSucceeded        RunStatus = "succeeded"
	RunFailed           RunStatus = "failed"
	RunCancelled        RunStatus = "cancelled"
)

// Valid reports whether s is a defined status.
func (s RunStatus) Valid() bool {
	switch s {
	case RunQueued, RunRunning, RunAwaitingApproval, RunSucceeded, RunFailed, RunCancelled:
		return true
	}
	return false
}

// Terminal reports a status a run never leaves.
func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

// Active reports a status that counts against its concurrency group: a
// parked run is active (flow.md §7.3).
func (s RunStatus) Active() bool { return s.Valid() && !s.Terminal() }

// StepStatus is where one step record stands (flow.md §7.1).
type StepStatus string

// Step statuses.
const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepWaiting   StepStatus = "waiting"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
	StepCancelled StepStatus = "cancelled"
)

// Valid reports whether s is a defined status.
func (s StepStatus) Valid() bool {
	switch s {
	case StepPending, StepRunning, StepWaiting, StepSucceeded, StepFailed, StepSkipped, StepCancelled:
		return true
	}
	return false
}

// Terminal reports a status a step record never leaves. A waiting step is
// not terminal: it resumes.
func (s StepStatus) Terminal() bool {
	switch s {
	case StepSucceeded, StepFailed, StepSkipped, StepCancelled:
		return true
	}
	return false
}

// StepReason says why a step is skipped, waiting or cancelled.
type StepReason string

// Step reasons.
const (
	// Skipped.
	ReasonWhenFalse       StepReason = "when_false"       // `when` evaluated false
	ReasonUpstreamFailed  StepReason = "upstream_failed"  // a dependency failed
	ReasonUpstreamSkipped StepReason = "upstream_skipped" // it reads the absent result of a skipped step (flow.md §5.7)
	ReasonItemSkipped     StepReason = "item_skipped"     // a for_each element skipped by on_item_error: skip
	ReasonBudgetExceeded  StepReason = "budget_exceeded"  // never started: the run's budget ran out (cancelled)
	ReasonRunCancelled    StepReason = "run_cancelled"    // never finished: the run was cancelled
	ReasonRunFailed       StepReason = "run_failed"       // never started: the run already failed
	// Waiting.
	ReasonApproval          StepReason = "approval"           // parked on an approval
	ReasonHuman             StepReason = "human"              // parked on a human step
	ReasonUncertain         StepReason = "uncertain"          // unknown outcome of a non-repeatable step (flow.md §7.9)
	ReasonAdmissionDeferred StepReason = "admission_deferred" // the admission backend has not decided
)

// Cost is money and tokens spent. Model nodes report it with their result.
type Cost struct {
	USD    float64 `json:"usd,omitempty"`
	Tokens int64   `json:"tokens,omitempty"`
}

// Add returns the sum of two costs.
func (c Cost) Add(o Cost) Cost { return Cost{USD: c.USD + o.USD, Tokens: c.Tokens + o.Tokens} }

// IsZero reports no spend.
func (c Cost) IsZero() bool { return c == Cost{} }

// Usage is what a run has consumed against its budget (flow.md §7.7).
type Usage struct {
	Cost
	// Calls counts node and command invocations.
	Calls int `json:"calls,omitempty"`
	// Active is wall-clock time since run start, parked time excluded.
	Active time.Duration `json:"active_ns,omitempty"`
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{Cost: u.Cost.Add(o.Cost), Calls: u.Calls + o.Calls, Active: u.Active + o.Active}
}

// FlowRef names the exact flow version a run belongs to. The run keeps it
// for its whole life (flow.md §7.2).
type FlowRef struct {
	Name    string `json:"flow"`
	Version string `json:"version"`
	// Hash is the SHA-256 of the raw document: the run's flow_hash.
	Hash string `json:"hash"`
}

// String renders name@version.
func (r FlowRef) String() string { return r.Name + "@" + r.Version }

// TriggerInfo records what started a run: the `trigger` namespace of
// flow.md §4.2.
type TriggerInfo struct {
	Kind specrun.TriggerKind `json:"kind"`
	// At is the instant the trigger was due (schedule, calendar) or fired.
	At time.Time `json:"at"`
	// EventID is the source event id (event trigger) used in the run key.
	EventID string `json:"event_id,omitempty"`
	// Payload is the trigger's payload: `event` for calendar and event
	// triggers, `vars` for a manual one. It is exposed as trigger.<key>.
	Payload map[string]any `json:"payload,omitempty"`
}

// Run is one execution of a flow version. Its shape follows dek's Job
// (id, kind, subject, status, attempts, waiting_for, expires_at, outcome)
// so the record projects onto it (see internal/records); the flo-specific
// fields sit beside.
type Run struct {
	ID     RunID    `json:"id"`
	Tenant TenantID `json:"tenant"`
	Flow   FlowRef  `json:"flow"`

	Status RunStatus `json:"status"`
	// Vars are the bound, typed flow vars.
	Vars    map[string]any `json:"vars,omitempty"`
	Trigger TriggerInfo    `json:"trigger"`

	// IdemKey is the run key (flow.md §6.1); empty for a manual run without
	// --idempotency-key. (Tenant, Flow.Name, IdemKey) is unique when set.
	IdemKey string `json:"idempotency_key,omitempty"`
	// Group is the rendered concurrency group; Admitted reports that the run
	// holds one of its slots. A queued run waiting for a slot is not admitted.
	Group string `json:"group,omitempty"`
	// GroupMax is the group's concurrency limit, kept so a finished run can
	// promote the queued ones (Groups.Release) without re-reading the flow.
	GroupMax int  `json:"group_max,omitempty"`
	Admitted bool `json:"admitted"`

	// StartedBy is the profile that created the run; RunAs the profile it
	// acts as (the flow's run_as, else StartedBy).
	StartedBy string `json:"started_by,omitempty"`
	RunAs     string `json:"run_as,omitempty"`

	// Attempts and MaxAttempts are dek's job counters: how many times the
	// run.advance job has been picked up, and its limit.
	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`
	// WaitingFor is the event pattern the parked run waits on:
	// flo.approval.<id>. Empty unless awaiting_approval.
	WaitingFor string `json:"waiting_for,omitempty"`
	// ExpiresAt is when what the run waits for lapses (approval timeout).
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`

	// CancelRequested is set by RunStore.RequestCancel; the executor
	// finalizes the run as cancelled at its next turn.
	CancelRequested bool `json:"cancel_requested,omitempty"`

	// StateSnapshot is the flow state taken when the run started; it is
	// stable across park and restart. Nil until the executor takes it.
	StateSnapshot map[string]any `json:"state_snapshot,omitempty"`

	// Failure is why a failed or cancelled run ended; Result the value of
	// the flow's `returns` (dek's outcome) when it succeeded.
	Failure *Failure `json:"failure,omitempty"`
	Result  any      `json:"result,omitempty"`

	Usage Usage `json:"usage"`
	// TaskRef is the accountable task of the run (flow.md §7.8), created
	// lazily; empty until then.
	TaskRef string `json:"task_ref,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// NewRun is what RunStore.CreateRun needs. The caller mints ID.
type NewRun struct {
	ID        RunID
	Tenant    TenantID
	Flow      FlowRef
	Vars      map[string]any
	Trigger   TriggerInfo
	IdemKey   string
	Group     string
	GroupMax  int
	Admitted  bool
	StartedBy string
	RunAs     string
	// MaxAttempts bounds run.advance job pickups; 0 takes the store default.
	MaxAttempts int
	At          time.Time // creation time
}

// RunPatch changes the non-terminal fields of a run. A nil field is left
// alone; a non-nil string or time is set, and the zero value clears it.
// Terminal statuses are never set through a patch: use StateStore.FinishRun.
type RunPatch struct {
	Status        *RunStatus
	WaitingFor    *string
	ExpiresAt     *time.Time
	NextRetryAt   *time.Time
	StartedAt     *time.Time
	Attempts      *int
	Usage         *Usage
	TaskRef       *string
	Admitted      *bool
	StateSnapshot map[string]any // set when non-nil
}

// RunFinish moves a run to a terminal status and settles its state: staged
// state is committed when Status is RunSucceeded and dropped otherwise, in
// the same transaction as the status change.
type RunFinish struct {
	Status  RunStatus // succeeded, failed or cancelled
	Failure *Failure
	Result  any
	Usage   Usage
	At      time.Time
}

// RunFilter selects runs for listing. Zero fields do not filter.
type RunFilter struct {
	Flow     string
	Statuses []RunStatus
	Group    string
	// Since keeps runs created at or after it.
	Since time.Time
	// Limit bounds the result; 0 means the store default. Newest first.
	Limit int
}

// StepKind is the shape of a step after expansion.
type StepKind string

// Step kinds.
const (
	StepKindAgent StepKind = "agent"
	StepKindExec  StepKind = "exec"
	StepKindHuman StepKind = "human"
	StepKindNode  StepKind = "node"
	StepKindFlow  StepKind = "flow" // a sub-flow; refused in phase 1 (engine_feature)
)

// Element marks a StepRun as one element of a for_each step.
type Element struct {
	// Parent is the step-level record the element belongs to.
	Parent StepRunID `json:"parent"`
	// Index is the 0-based position in the list.
	Index int `json:"index"`
	// Item is the element value (the `item` namespace).
	Item any `json:"item,omitempty"`
}

// NodeSource says which spec served a step: the layer and the exact pack
// version (node.md §2).
type NodeSource struct {
	Layer       NodeLayer `json:"layer"`
	Pack        string    `json:"pack,omitempty"`
	PackVersion string    `json:"pack_version,omitempty"`
	Path        string    `json:"path,omitempty"`
}

// NodeLayer is where a node spec was found (node.md §1.2).
type NodeLayer string

// Node layers, highest precedence first.
const (
	LayerBundle  NodeLayer = "bundle"
	LayerPack    NodeLayer = "pack"
	LayerBuiltin NodeLayer = "builtin"
)

// Attempt is one execution try of a step.
type Attempt struct {
	N          int       `json:"n"` // 1-based
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Outcome    Outcome   `json:"outcome"`
	Effect     Effect    `json:"effect,omitempty"`
	Failure    *Failure  `json:"failure,omitempty"`
	Cost       Cost      `json:"cost,omitempty"`
}

// Plan is what a node would do, returned instead of doing it (dry run, test
// and recording). Its shape follows dek's Plan.
type Plan struct {
	// Capability is the node type (or core kind) the plan is for.
	Capability     string `json:"capability"`
	Args           any    `json:"args"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Rendered is the request the node would send: argv, HTTP request.
	Rendered any `json:"rendered,omitempty"`
}

// StepRun is the record of one step, or of one element of a for_each step:
// inputs, output, cost, attempts, receipts and the node spec that served it
// (flow.md §7.1). Records live in the run tree and are never rewritten once
// terminal.
type StepRun struct {
	ID     StepRunID `json:"id"`
	Tenant TenantID  `json:"tenant"`
	Run    RunID     `json:"run"`
	// StepID is the step's id in the expanded flow.
	StepID string `json:"step"`
	// Element is set for a for_each element record.
	Element *Element `json:"element,omitempty"`

	Kind StepKind `json:"kind"`
	// Node is the node type `ns/name@N` for a node step.
	Node       string     `json:"node,omitempty"`
	NodeSource NodeSource `json:"node_source,omitempty"`

	Status StepStatus `json:"status"`
	Reason StepReason `json:"reason,omitempty"`

	// Inputs are the rendered `with` values. Output is the value
	// `results.<step>` reads; OutputRef names a stored value when it was
	// too large to keep inline. Raw is the raw exec result, kept in the
	// record but not readable from a flow (flow.md §4.3).
	Inputs    any    `json:"inputs,omitempty"`
	Output    any    `json:"output,omitempty"`
	OutputRef string `json:"output_ref,omitempty"`
	Raw       any    `json:"raw,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	Cost     Cost      `json:"cost,omitempty"`
	Attempts []Attempt `json:"attempts,omitempty"`
	// Receipts are the admission receipts of the step.
	Receipts []ReceiptID `json:"receipts,omitempty"`
	// IdemKey is the idempotency key passed to the target (node.md §11).
	IdemKey string `json:"idempotency_key,omitempty"`
	// Plan is set when the step was dry-run.
	Plan *Plan `json:"plan,omitempty"`
	// Approval is the approval the step is parked on, once opened.
	Approval ApprovalID `json:"approval,omitempty"`
	Failure  *Failure   `json:"failure,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// IsElement reports a for_each element record.
func (s StepRun) IsElement() bool { return s.Element != nil }
