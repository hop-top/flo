package core

import (
	"time"

	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

// FlowState is where a flow version stands in its life.
type FlowState string

// Flow states.
const (
	// FlowDraft: stored by apply, not yet activated.
	FlowDraft FlowState = "draft"
	// FlowActive: the version new runs start on. At most one per flow.
	FlowActive FlowState = "active"
	// FlowPaused: the flow's triggers fire nothing; running and parked runs
	// continue.
	FlowPaused FlowState = "paused"
	// FlowRetired: replaced by a newer active version.
	FlowRetired FlowState = "retired"
)

// Valid reports whether s is a defined state.
func (s FlowState) Valid() bool {
	switch s {
	case FlowDraft, FlowActive, FlowPaused, FlowRetired:
		return true
	}
	return false
}

// FlowVersion is one stored version of a flow: the raw document and where it
// stands. Versions are keyed by content: the same bytes are one version.
type FlowVersion struct {
	Tenant TenantID `json:"tenant"`
	Name   string   `json:"flow"`
	// Version is the document's own `version`.
	Version string `json:"version"`
	// Hash is the SHA-256 of Source.
	Hash string `json:"hash"`
	// Seq orders a flow's versions by first storage, from 1.
	Seq int `json:"seq"`
	// Source is the raw document bytes.
	Source []byte `json:"-"`
	// Bundle is the directory the document was applied from, for its nodes/
	// layer; empty for a single file with none.
	Bundle string `json:"bundle,omitempty"`

	State       FlowState  `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
}

// Ref returns the version's FlowRef.
func (v FlowVersion) Ref() FlowRef { return FlowRef{Name: v.Name, Version: v.Version, Hash: v.Hash} }

// NewFlowVersion is what FlowStore.PutVersion needs.
type NewFlowVersion struct {
	Tenant  TenantID
	Name    string
	Version string
	Hash    string
	Source  []byte
	Bundle  string
	At      time.Time
}

// FlowSummary is one row of a flow listing.
type FlowSummary struct {
	Name string `json:"flow"`
	// Latest is the newest stored version; Active the version new runs use,
	// nil when none is active.
	Latest FlowVersion  `json:"latest"`
	Active *FlowVersion `json:"active,omitempty"`
	Paused bool         `json:"paused,omitempty"`
}

// Verdict is a decision on one approval item.
type Verdict string

// Verdicts. Auto is approved by trust, without a person.
const (
	VerdictApprove Verdict = "approve"
	VerdictReject  Verdict = "reject"
	VerdictCorrect Verdict = "correct"
	VerdictAuto    Verdict = "auto"
)

// Valid reports whether v is a defined verdict.
func (v Verdict) Valid() bool {
	switch v {
	case VerdictApprove, VerdictReject, VerdictCorrect, VerdictAuto:
		return true
	}
	return false
}

// Approving reports a verdict that lets the guarded step act on the item:
// approve, correct (with the person's replacement) and auto.
func (v Verdict) Approving() bool { return v != VerdictReject && v.Valid() }

// ItemDecision is the decision recorded on an item.
type ItemDecision struct {
	Verdict Verdict `json:"verdict"`
	// By is who decided: a profile, "trust" for auto, "engine" for a timeout
	// or a cancellation.
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
	// Correction is the person's replacement for a corrected item.
	Correction any `json:"correction,omitempty"`
}

// Reasons the engine itself gives when it decides.
const (
	DecisionReasonTimeout      = "timeout"       // undecided at the approval's timeout
	DecisionReasonRunCancelled = "run_cancelled" // the run was cancelled; not reported to trust
	DeciderEngine              = "engine"
	DeciderTrust               = "trust"
	ApprovalSurfaceTLC         = "tlc"
	ApprovalSurfaceNone        = "none"
)

// ApprovalItem is one thing waiting for a decision (node.md §6.6). Its shape
// follows dek's Proposal: the exact operation a person would approve.
type ApprovalItem struct {
	// ID is stable across runs when the underlying thing is.
	ID     string `json:"id"`
	Action string `json:"action,omitempty"`
	Why    string `json:"why,omitempty"`
	// Class is the trust action type(s); with a provider the item is auto
	// when every class is at level auto.
	Class []string `json:"class,omitempty"`
	// Capability and Args are the exact operation (node.md §10.4).
	Capability string `json:"capability,omitempty"`
	Args       any    `json:"args,omitempty"`
	// Digest is the request digest of Capability and Args, when set.
	Digest string `json:"digest,omitempty"`
	// Extra carries the item's other fields to the provider and the output.
	Extra    map[string]any `json:"extra,omitempty"`
	Decision *ItemDecision  `json:"decision,omitempty"`
}

// Decided reports an item with a decision.
func (i ApprovalItem) Decided() bool { return i.Decision != nil }

// ApprovalKind is the shape of an approval.
type ApprovalKind string

// Approval kinds.
const (
	// ApprovalItems: item-level decisions of a flo/approval@1 step.
	ApprovalItems ApprovalKind = "items"
	// ApprovalHuman: the whole-step decision of a core `human` step; one
	// item, id HumanItemID.
	ApprovalHuman ApprovalKind = "human"
)

// HumanItemID is the id of the single item of a human approval.
const HumanItemID = "decision"

// ApprovalStatus is where an approval stands.
type ApprovalStatus string

// Approval statuses.
const (
	// ApprovalOpen: at least one item is undecided.
	ApprovalOpen ApprovalStatus = "open"
	// ApprovalDecided: every item has a decision (by a person, trust, or a timeout).
	ApprovalDecided ApprovalStatus = "decided"
	// ApprovalCancelled: its run was cancelled; undecided items were
	// rejected with reason run_cancelled.
	ApprovalCancelled ApprovalStatus = "cancelled"
)

// Approval is the set of items one step parked on, with their decisions.
// Version rises by one on every change; a write names the version it read
// (ApprovalStore.UpdateApproval), as dek's proposal decisions do.
type Approval struct {
	ID       ApprovalID     `json:"id"`
	Tenant   TenantID       `json:"tenant"`
	Run      RunID          `json:"run"`
	StepRun  StepRunID      `json:"step_run"`
	StepID   string         `json:"step"`
	Kind     ApprovalKind   `json:"kind"`
	Title    string         `json:"title,omitempty"`
	Surface  string         `json:"surface,omitempty"`
	Trust    string         `json:"trust,omitempty"`
	Context  map[string]any `json:"context,omitempty"`
	Verdicts []Verdict      `json:"verdicts,omitempty"`
	Items    []ApprovalItem `json:"items"`
	Assignee string         `json:"assignee,omitempty"`

	Status  ApprovalStatus `json:"status"`
	Version uint64         `json:"version"`
	// TaskRef is the task the person sees (surface tlc); empty until created.
	TaskRef string `json:"task_ref,omitempty"`
	// ExpiresAt is when undecided items fall to OnTimeout.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	OnTimeout string     `json:"on_timeout,omitempty"`
	// Reported says the trust provider's decide has been called, so it is
	// called once (node.md §10.3).
	Reported bool `json:"reported,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Undecided returns the indexes of the items without a decision.
func (a Approval) Undecided() []int {
	var out []int
	for i, it := range a.Items {
		if !it.Decided() {
			out = append(out, i)
		}
	}
	return out
}

// Complete reports that every item has a decision.
func (a Approval) Complete() bool { return len(a.Undecided()) == 0 }

// Item returns the item with id.
func (a Approval) Item(id string) (ApprovalItem, bool) {
	for _, it := range a.Items {
		if it.ID == id {
			return it, true
		}
	}
	return ApprovalItem{}, false
}

// ApprovalFilter selects approvals for listing. Zero fields do not filter.
type ApprovalFilter struct {
	Status ApprovalStatus
	Run    RunID
	StepID string
	// Limit bounds the result; 0 means the store default. Newest first.
	Limit int
}

// ItemVerdict is a decision a caller gives for one item.
type ItemVerdict struct {
	Item       string
	Verdict    Verdict // approve, reject or correct
	Reason     string
	Correction any
}

// DecideRequest records decisions on an approval (`flo approval set`). Items
// not named, and not covered by All, stay undecided. Items already decided
// keep their decision: the same request twice yields the same state.
type DecideRequest struct {
	Approval ApprovalID
	Items    []ItemVerdict
	// All decides every still-undecided item (`--all=approve`); empty to not.
	All       Verdict
	AllReason string
	// By is the deciding profile.
	By string
	// Version, when non-zero, is the approval version the caller read; a
	// different current version is approval_version_conflict.
	Version uint64
}

// DecideResult is what a decision changed.
type DecideResult struct {
	Approval Approval
	// Changed reports that at least one item got a decision.
	Changed bool
	// Resumed reports that the approval completed and the run was resumed.
	Resumed bool
}

// GuardQuery asks whether an irreversible step may run (node.md §10.1).
type GuardQuery struct {
	Tenant TenantID
	Run    RunID
	StepID string
	// Approvals are the ancestor flo/approval@1 step ids that guard it.
	Approvals []string
	// Agent is the profile the step runs as: a trust provider reads its level.
	Agent string
	// TrustProvider and TrustClass, when set, let trust guard the step.
	TrustProvider string
	TrustClass    string
}

// GrantSource says who owns a grant.
type GrantSource string

// Grant sources. A provisioned grant comes from the governance backend and
// is refused by set and unset (grant_provisioned, exit 4).
const (
	GrantLocal       GrantSource = "local"
	GrantProvisioned GrantSource = "provisioned"
)

// CapabilityAny is the wildcard capability: it covers every capability.
const CapabilityAny = "*"

// Grant gives a profile a capability (`<domain>.<verb>`) or the wildcard.
type Grant struct {
	Tenant     TenantID    `json:"tenant"`
	Profile    string      `json:"profile"`
	Capability string      `json:"capability"`
	Source     GrantSource `json:"source"`
	CreatedAt  time.Time   `json:"created_at"`
	// ExpiresAt ends the grant (exclusive); nil is no end. A provisioned grant
	// ends at the earlier of its own end and the staleness limit of the set it
	// came from, so a set nobody refreshes stops covering by itself.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Live reports whether the grant covers at now.
func (g Grant) Live(now time.Time) bool { return g.ExpiresAt == nil || now.Before(*g.ExpiresAt) }

// GrantCheck asks whether a flow may be saved or activated by a caller
// (flow.md V18).
type GrantCheck struct {
	Tenant TenantID
	// Caller is the profile creating, editing or activating the flow.
	Caller string
	// RunAs is the flow's profile; empty means Caller.
	RunAs string
	// Capabilities are every capability the flow's nodes declare.
	Capabilities []string
}

// AdmissionResult is a backend's answer.
type AdmissionResult string

// Admission results.
const (
	AdmissionAllow    AdmissionResult = "allow"
	AdmissionDeny     AdmissionResult = "deny"
	AdmissionDeferred AdmissionResult = "deferred"
)

// Purpose names why an operation is requested: the flow and the step.
type Purpose struct {
	Flow    string `json:"flow"`
	Version string `json:"version"`
	StepID  string `json:"step"`
}

// AdmissionRequest is asked before every step with a side effect above
// `read`, and before every step whose run_as is not the starter's
// (node.md §10.2). The backend computes the request digest from it.
type AdmissionRequest struct {
	Tenant TenantID
	// Agent is the run_as profile; Principal the person the run acts for.
	Agent     string
	Principal string
	Purpose   Purpose
	Run       RunID
	StepRun   StepRunID
	// Operation is the node type, or the core kind for a step without a node.
	Operation    string
	Capabilities []string
	SideEffect   node.SideEffect
	// Input is the rendered input the digest covers.
	Input any
}

// AdmissionDecision is a backend's answer and its receipt.
type AdmissionDecision struct {
	Result  AdmissionResult
	Receipt Receipt
	Reason  string
}

// Receipt records one admission decision.
type Receipt struct {
	ID       ReceiptID       `json:"id"`
	Tenant   TenantID        `json:"tenant"`
	Run      RunID           `json:"run"`
	StepRun  StepRunID       `json:"step_run"`
	StepID   string          `json:"step"`
	Digest   string          `json:"digest"`
	Decision AdmissionResult `json:"decision"`
	Agent    string          `json:"agent"`
	// Principal is the person the agent acted for.
	Principal    string          `json:"principal,omitempty"`
	Operation    string          `json:"operation"`
	Capabilities []string        `json:"capabilities,omitempty"`
	SideEffect   node.SideEffect `json:"side_effect"`
	// Backend is who decided: "local", or the external system.
	Backend string    `json:"backend"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
	// Proposal and ExecutionKey bind an allowed step to the approval of an
	// external backend: the step runs the operation once with them.
	Proposal     string `json:"proposal,omitempty"`
	ExecutionKey string `json:"execution_key,omitempty"`
	// CorrectedInput is the input the approver changed the request to; the
	// step runs it, never its own. Nil when the approval was as asked.
	CorrectedInput map[string]any `json:"corrected_input,omitempty"`
}

// OutputKind is what a flo/output@1 step publishes (node.md §6.7).
type OutputKind string

// Output kinds.
const (
	OutputFindings OutputKind = "findings"
	OutputBrief    OutputKind = "brief"
)

// OutputRetention is how long outputs are kept (node.md §6.7).
const OutputRetention = 90 * 24 * time.Hour

// Output is a published result: findings or a brief, listed by `flo brief`.
type Output struct {
	ID     OutputID   `json:"id"`
	Tenant TenantID   `json:"tenant"`
	Run    RunID      `json:"run"`
	StepID string     `json:"step"`
	Kind   OutputKind `json:"kind"`
	Title  string     `json:"title,omitempty"`
	Body   string     `json:"body,omitempty"`
	// Items are the findings: {title, label?, detail?, url?, refs?}.
	Items     []any     `json:"items,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// OutputFilter selects outputs. Zero fields do not filter.
type OutputFilter struct {
	Run   RunID
	Kind  OutputKind
	Since time.Time
	// Limit bounds the result; 0 means the store default. Newest first.
	Limit int
}

// TriggerState says whether a trigger registration fires.
type TriggerState string

// Trigger states.
const (
	TriggerActive TriggerState = "active"
	TriggerPaused TriggerState = "paused"
)

// TriggerRecord is one trigger registration of an active flow version: the
// declared trigger and what is queued for it.
type TriggerRecord struct {
	ID     TriggerID `json:"id"`
	Tenant TenantID  `json:"tenant"`
	Flow   string    `json:"flow"`
	// Version and Hash name the flow version that declared it.
	Version string `json:"version"`
	Hash    string `json:"hash"`
	// Index is the position in `on:`.
	Index int             `json:"index"`
	Spec  specrun.Trigger `json:"spec"`
	State TriggerState    `json:"state"`
	// NextAt and NextJob are the next scheduled fire and its job; nil/empty
	// when none is queued (manual, event, or paused).
	NextAt    *time.Time `json:"next_at,omitempty"`
	NextJob   JobID      `json:"next_job,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// FireOutcome is what a trigger fire came to.
type FireOutcome string

// Fire outcomes.
const (
	// FireStarted: a run was created.
	FireStarted FireOutcome = "started"
	// FireDuplicate: the run key already had a run; it was returned.
	FireDuplicate FireOutcome = "duplicate"
	// FireSkipped: the concurrency group was full and on_overlap is skip.
	FireSkipped FireOutcome = "skipped"
	// FireMissed: a fire missed while the engine was down, past its period.
	FireMissed FireOutcome = "missed"
)

// Fire records one trigger fire, including the ones that started nothing.
type Fire struct {
	Tenant  TenantID    `json:"tenant"`
	Flow    string      `json:"flow"`
	Trigger TriggerID   `json:"trigger"`
	Key     string      `json:"key"`
	At      time.Time   `json:"at"`
	Outcome FireOutcome `json:"outcome"`
	Reason  string      `json:"reason,omitempty"`
	Run     RunID       `json:"run,omitempty"`
	// RecordedAt is when the fire was handled.
	RecordedAt time.Time `json:"recorded_at"`
}

// PlannedFire is one pending calendar fire, keyed by (event UID, recurrence
// id, offset): the FireMap of flow.md §6.
type PlannedFire struct {
	Tenant  TenantID  `json:"tenant"`
	Flow    string    `json:"flow"`
	Trigger TriggerID `json:"trigger"`
	// Key is the (UID, recurrence id, offset) key.
	Key string `json:"key"`
	// At is when it fires; EventStart the event's start it was planned from.
	At         time.Time      `json:"at"`
	EventStart time.Time      `json:"event_start"`
	Job        JobID          `json:"job,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

// TaskRef identifies a task in the task tool.
type TaskRef string

// TaskSpec is a task to create.
type TaskSpec struct {
	Title       string
	Description string
	Assignee    string
	Due         *time.Time
	// Parent is the accountable run task an approval task hangs under.
	Parent TaskRef
	Run    RunID
	// Approval is set for an approval task.
	Approval ApprovalID
	Tags     []string
}

// TaskState is what a person did to a task.
type TaskState string

// Task states. A person approving the task approves every item still
// undecided; rejecting it rejects them (flow.md §7.8).
const (
	TaskOpen     TaskState = "open"
	TaskApproved TaskState = "approved"
	TaskRejected TaskState = "rejected"
)

// TaskInfo is a task as the task tool reports it.
type TaskInfo struct {
	Ref   TaskRef
	State TaskState
	Title string
	By    string
	At    *time.Time
	Note  string
}

// TrustLevelName is a trust level: ask (a person decides) or auto.
type TrustLevelName string

// Trust levels.
const (
	TrustAsk  TrustLevelName = "ask"
	TrustAuto TrustLevelName = "auto"
)

// TrustLevel is the level of one action class (node.md §10.3).
type TrustLevel struct {
	Class     string         `json:"class"`
	Level     TrustLevelName `json:"level"`
	Streak    int            `json:"streak,omitempty"`
	Threshold int            `json:"threshold,omitempty"`
	Resets    int            `json:"resets,omitempty"`
	LastReset *time.Time     `json:"last_reset,omitempty"`
}

// TrustDecision is one decision reported to a trust provider.
type TrustDecision struct {
	ID      string         `json:"id"`
	Verdict Verdict        `json:"verdict"`
	Reason  string         `json:"reason,omitempty"`
	Item    map[string]any `json:"item,omitempty"`
}

// TrustApplied is the provider's answer to decide: any skipped entry fails
// the approval step (decision_not_recorded).
type TrustApplied struct {
	Applied []string `json:"applied"`
	Skipped []string `json:"skipped"`
}
