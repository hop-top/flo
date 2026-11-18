package core

import (
	"time"

	"hop.top/kit/go/runtime/bus"

	specrun "hop.top/flo/spec/run"
)

// The run log is a stream of CloudEvents on kit's bus. Topics follow kit's
// four-segment, past-tense convention (source.category.object.action) and
// are checked against bus.ValidateTopic in tests.
const (
	// Run lifecycle (payload RunEvent).
	TopicRunCreated   bus.Topic = "flo.run.run.created"
	TopicRunStarted   bus.Topic = "flo.run.run.started"
	TopicRunParked    bus.Topic = "flo.run.run.parked"
	TopicRunResumed   bus.Topic = "flo.run.run.resumed"
	TopicRunSucceeded bus.Topic = "flo.run.run.succeeded"
	TopicRunFailed    bus.Topic = "flo.run.run.failed"
	TopicRunCancelled bus.Topic = "flo.run.run.cancelled"

	// Step lifecycle (payload StepEvent): start, end, inputs, outputs, cost.
	TopicStepStarted   bus.Topic = "flo.run.step.started"
	TopicStepRetried   bus.Topic = "flo.run.step.retried"
	TopicStepParked    bus.Topic = "flo.run.step.parked"
	TopicStepResumed   bus.Topic = "flo.run.step.resumed"
	TopicStepSucceeded bus.Topic = "flo.run.step.succeeded"
	TopicStepFailed    bus.Topic = "flo.run.step.failed"
	TopicStepSkipped   bus.Topic = "flo.run.step.skipped"
	TopicStepCancelled bus.Topic = "flo.run.step.cancelled"

	// Approvals (payload ApprovalEvent).
	TopicApprovalRequested bus.Topic = "flo.run.approval.requested"
	TopicApprovalDecided   bus.Topic = "flo.run.approval.decided" // one item
	TopicApprovalResolved  bus.Topic = "flo.run.approval.resolved"
	TopicApprovalCancelled bus.Topic = "flo.run.approval.cancelled"

	// Admission (payload AdmissionEvent) and budget (payload BudgetEvent).
	TopicAdmissionDecided bus.Topic = "flo.run.admission.decided"
	TopicBudgetExceeded   bus.Topic = "flo.run.budget.exceeded"

	// Triggers (payload TriggerEvent).
	TopicTriggerFired       bus.Topic = "flo.engine.trigger.fired"
	TopicTriggerSkipped     bus.Topic = "flo.engine.trigger.skipped" // reason overlap or missed
	TopicTriggerScheduled   bus.Topic = "flo.engine.trigger.scheduled"
	TopicTriggerRescheduled bus.Topic = "flo.engine.trigger.rescheduled"
	TopicTriggerCancelled   bus.Topic = "flo.engine.trigger.cancelled"
)

// TopicInfo describes one topic of the run log.
type TopicInfo struct {
	Topic bus.Topic
	// Payload is a zero value of the payload type, for documentation and
	// schema generation.
	Payload any
}

// Topics lists every topic the engine emits.
func Topics() []TopicInfo {
	run, step, apr := RunEvent{}, StepEvent{}, ApprovalEvent{}
	return []TopicInfo{
		{TopicRunCreated, run}, {TopicRunStarted, run}, {TopicRunParked, run}, {TopicRunResumed, run},
		{TopicRunSucceeded, run}, {TopicRunFailed, run}, {TopicRunCancelled, run},
		{TopicStepStarted, step}, {TopicStepRetried, step}, {TopicStepParked, step}, {TopicStepResumed, step},
		{TopicStepSucceeded, step}, {TopicStepFailed, step}, {TopicStepSkipped, step}, {TopicStepCancelled, step},
		{TopicApprovalRequested, apr}, {TopicApprovalDecided, apr}, {TopicApprovalResolved, apr}, {TopicApprovalCancelled, apr},
		{TopicAdmissionDecided, AdmissionEvent{}}, {TopicBudgetExceeded, BudgetEvent{}},
		{TopicTriggerFired, TriggerEvent{}}, {TopicTriggerSkipped, TriggerEvent{}}, {TopicTriggerScheduled, TriggerEvent{}},
		{TopicTriggerRescheduled, TriggerEvent{}}, {TopicTriggerCancelled, TriggerEvent{}},
	}
}

// RunEvent is the payload of the flo.run.run.* topics.
type RunEvent struct {
	Tenant     TenantID            `json:"tenant"`
	Run        RunID               `json:"run"`
	Flow       string              `json:"flow"`
	Version    string              `json:"version"`
	Hash       string              `json:"hash,omitempty"`
	Status     RunStatus           `json:"status"`
	Trigger    specrun.TriggerKind `json:"trigger,omitempty"`
	WaitingFor string              `json:"waiting_for,omitempty"`
	Failure    *Failure            `json:"failure,omitempty"`
	Usage      *Usage              `json:"usage,omitempty"`
}

// StepEvent is the payload of the flo.run.step.* topics.
type StepEvent struct {
	Tenant  TenantID   `json:"tenant"`
	Run     RunID      `json:"run"`
	StepRun StepRunID  `json:"step_run"`
	Step    string     `json:"step"`
	Index   *int       `json:"index,omitempty"`
	Node    string     `json:"node,omitempty"`
	Status  StepStatus `json:"status"`
	Reason  StepReason `json:"reason,omitempty"`
	Attempt int        `json:"attempt,omitempty"`
	// Inputs, Output and Cost are in the log automatically (design: no log node).
	Inputs   any         `json:"inputs,omitempty"`
	Output   any         `json:"output,omitempty"`
	Cost     Cost        `json:"cost,omitempty"`
	Failure  *Failure    `json:"failure,omitempty"`
	Receipts []ReceiptID `json:"receipts,omitempty"`
}

// ApprovalEvent is the payload of the flo.run.approval.* topics.
type ApprovalEvent struct {
	Tenant   TenantID       `json:"tenant"`
	Run      RunID          `json:"run"`
	Approval ApprovalID     `json:"approval"`
	Step     string         `json:"step,omitempty"`
	Status   ApprovalStatus `json:"status"`
	// Item, Verdict and By are set on flo.run.approval.decided.
	Item    string  `json:"item,omitempty"`
	Verdict Verdict `json:"verdict,omitempty"`
	By      string  `json:"by,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Items   int     `json:"items,omitempty"`
	Waiting int     `json:"waiting,omitempty"`
}

// AdmissionEvent is the payload of flo.run.admission.decided.
type AdmissionEvent struct {
	Tenant    TenantID        `json:"tenant"`
	Run       RunID           `json:"run"`
	Step      string          `json:"step"`
	Receipt   ReceiptID       `json:"receipt"`
	Digest    string          `json:"digest"`
	Decision  AdmissionResult `json:"decision"`
	Agent     string          `json:"agent"`
	Principal string          `json:"principal,omitempty"`
	Operation string          `json:"operation"`
	Reason    string          `json:"reason,omitempty"`
}

// BudgetEvent is the payload of flo.run.budget.exceeded.
type BudgetEvent struct {
	Tenant TenantID `json:"tenant"`
	Run    RunID    `json:"run"`
	// Step is set when a step's own budget was exceeded.
	Step string `json:"step,omitempty"`
	// Limit is usd, tokens, calls or duration.
	Limit string  `json:"limit"`
	Max   float64 `json:"max"`
	Used  float64 `json:"used"`
}

// TriggerEvent is the payload of the flo.engine.trigger.* topics.
type TriggerEvent struct {
	Tenant  TenantID            `json:"tenant"`
	Flow    string              `json:"flow"`
	Trigger TriggerID           `json:"trigger,omitempty"`
	Kind    specrun.TriggerKind `json:"kind"`
	Key     string              `json:"key,omitempty"`
	At      time.Time           `json:"at,omitempty"`
	// Outcome and Reason are set on fired and skipped.
	Outcome FireOutcome `json:"outcome,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	Run     RunID       `json:"run,omitempty"`
}

// EventSource returns the CloudEvents source of a tenant's events.
func EventSource(t TenantID) string { return "flo/" + string(t) }

// Extension names of the events flo emits.
const (
	// ExtTenant carries the tenant id.
	ExtTenant = "tenant"
	// ExtSeq is the per-run sequence number the RunLog stamps on append.
	ExtSeq = "seq"
)

// NewEvent builds a run-log event: id from ids, time from clock, subject the
// run id (empty for engine events), source and tenant extension from the
// tenant. The RunLog stamps ExtSeq.
func NewEvent(clock Clock, ids IDs, topic bus.Topic, tenant TenantID, run RunID, payload any) bus.Event {
	return bus.Event{
		ID:              ids.New(bus.EventIDPrefix),
		Topic:           topic,
		Source:          EventSource(tenant),
		Timestamp:       clock.Now().UTC(),
		Payload:         payload,
		Subject:         string(run),
		DataContentType: "application/json",
		Extensions:      map[string]any{ExtTenant: string(tenant)},
	}
}
