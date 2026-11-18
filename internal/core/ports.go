package core

import (
	"context"
	"time"
)

// This file holds the storage ports. A store is per tenant: every method
// takes the tenant first after the context, and a record of one tenant is
// never visible through another's. Methods are safe for concurrent use.
//
// Failures are *Error: a missing record is a *_not_found code, a lost
// compare-and-set a conflict code.

// RunStore keeps runs and their step records: the run tree.
type RunStore interface {
	// CreateRun stores a new run in status queued. It is idempotent on the
	// run key: when in.IdemKey is set and a run of the same tenant and flow
	// already has it, that run is returned with created false and nothing is
	// written. The caller mints in.ID.
	CreateRun(ctx context.Context, in NewRun) (run Run, created bool, err error)
	// LookupIdem returns the run holding the run key, if any.
	LookupIdem(ctx context.Context, tenant TenantID, flow, key string) (Run, bool, error)
	// GetRun returns a run, or run_not_found.
	GetRun(ctx context.Context, tenant TenantID, id RunID) (Run, error)
	// ListRuns returns runs newest first.
	ListRuns(ctx context.Context, tenant TenantID, f RunFilter) ([]Run, error)
	// UpdateRun applies a patch when the run's status is one of expect (any
	// status when expect is empty); otherwise run_state_conflict. A patch
	// never sets a terminal status (see StateStore.FinishRun) and never
	// changes a terminal run.
	UpdateRun(ctx context.Context, tenant TenantID, id RunID, expect []RunStatus, p RunPatch) (Run, error)
	// RequestCancel sets CancelRequested. It is idempotent and valid in any
	// status; on a terminal run it changes nothing. It does not finish the
	// run: the executor does at its next turn, and the caller resumes a
	// parked or queued run so that turn comes (Resumer).
	RequestCancel(ctx context.Context, tenant TenantID, id RunID) (Run, error)

	// PutStep writes a step record, creating or replacing it. A record that
	// is terminal is never replaced by a different one (step_conflict is a
	// bug in the caller): it is returned unchanged.
	PutStep(ctx context.Context, tenant TenantID, s StepRun) error
	// GetStep returns one step record, or run_not_found.
	GetStep(ctx context.Context, tenant TenantID, run RunID, id StepRunID) (StepRun, error)
	// ListSteps returns a run's step records in creation order.
	ListSteps(ctx context.Context, tenant TenantID, run RunID) ([]StepRun, error)
}

// StateStore keeps flow state: a JSON key-value map per flow and tenant, and
// the writes a run has staged but not committed (flow.md §7.4).
type StateStore interface {
	// Snapshot returns the committed state of a flow. Keys never written are
	// absent; the executor applies the declared defaults.
	Snapshot(ctx context.Context, tenant TenantID, flow string) (map[string]any, error)
	// Stage records a write against a run, replacing its earlier staged
	// write of the same key. A value over MaxStateValueBytes (JSON) is
	// state_too_large. Staged writes survive a park and a restart.
	Stage(ctx context.Context, tenant TenantID, run RunID, key string, value any) error
	// Staged returns the run's staged writes.
	Staged(ctx context.Context, tenant TenantID, run RunID) (map[string]any, error)
	// FinishRun moves the run to its terminal status, commits the staged
	// writes when it succeeded and drops them otherwise, in one transaction.
	// Finishing a run that already has that terminal status is a no-op that
	// returns it; a different terminal status is run_state_conflict.
	FinishRun(ctx context.Context, tenant TenantID, id RunID, fin RunFinish) (Run, error)
}

// MaxStateValueBytes is the largest JSON value one state key may hold.
const MaxStateValueBytes = 1 << 20

// FlowStore keeps flow versions.
type FlowStore interface {
	// PutVersion stores a flow version. The same bytes (same hash) are one
	// version: the existing one is returned with created false. A new hash
	// is a new version (draft) with the next Seq.
	PutVersion(ctx context.Context, in NewFlowVersion) (v FlowVersion, created bool, err error)
	// GetVersion returns a version by hash, by document version, or the
	// latest when ref is empty. flow_not_found otherwise.
	GetVersion(ctx context.Context, tenant TenantID, name, ref string) (FlowVersion, error)
	// Active returns the version new runs start on; flow_not_found when the
	// flow does not exist, flow_not_active when it has no active version.
	Active(ctx context.Context, tenant TenantID, name string) (FlowVersion, error)
	// ListFlows returns one summary per flow, by name.
	ListFlows(ctx context.Context, tenant TenantID) ([]FlowSummary, error)
	// ListVersions returns a flow's versions, newest first.
	ListVersions(ctx context.Context, tenant TenantID, name string) ([]FlowVersion, error)
	// SetState moves a version to active or paused. Activating a version
	// retires the flow's previous active version; pausing keeps the version
	// and stops its triggers. Setting the state a version already has is a
	// no-op.
	SetState(ctx context.Context, tenant TenantID, name, hash string, state FlowState, at time.Time) (FlowVersion, error)
}

// ApprovalStore keeps approvals.
type ApprovalStore interface {
	// CreateApproval stores a new approval (status open, version 1). It is
	// idempotent on (run, step run): the existing approval is returned with
	// created false.
	CreateApproval(ctx context.Context, a Approval) (out Approval, created bool, err error)
	// GetApproval returns an approval, or approval_not_found.
	GetApproval(ctx context.Context, tenant TenantID, id ApprovalID) (Approval, error)
	// ListApprovals returns approvals newest first.
	ListApprovals(ctx context.Context, tenant TenantID, f ApprovalFilter) ([]Approval, error)
	// UpdateApproval replaces an approval when a.Version equals the stored
	// version, and raises the version by one; otherwise
	// approval_version_conflict. It returns the stored record.
	UpdateApproval(ctx context.Context, a Approval) (Approval, error)
}

// GrantStore keeps grants. It stores; the Grants service decides.
type GrantStore interface {
	// SetGrant stores a grant. It reports whether anything changed: setting
	// an existing grant is a no-op.
	SetGrant(ctx context.Context, g Grant) (changed bool, err error)
	// UnsetGrant removes a grant. It reports whether anything changed.
	UnsetGrant(ctx context.Context, tenant TenantID, profile, capability string) (changed bool, err error)
	// ListGrants returns grants ordered by profile, capability; profile ""
	// lists every profile.
	ListGrants(ctx context.Context, tenant TenantID, profile string) ([]Grant, error)
}

// ReceiptStore keeps admission receipts.
type ReceiptStore interface {
	// PutReceipt stores a receipt, replacing one with the same id (a
	// deferred decision is updated in place).
	PutReceipt(ctx context.Context, r Receipt) error
	// GetReceipt returns a receipt; ok is false when there is none.
	GetReceipt(ctx context.Context, tenant TenantID, id ReceiptID) (Receipt, bool, error)
	// ListReceipts returns a run's receipts in creation order.
	ListReceipts(ctx context.Context, tenant TenantID, run RunID) ([]Receipt, error)
}

// OutputStore keeps published outputs.
type OutputStore interface {
	// PutOutput stores an output.
	PutOutput(ctx context.Context, o Output) error
	// GetOutput returns an output, or output_not_found.
	GetOutput(ctx context.Context, tenant TenantID, id OutputID) (Output, error)
	// ListOutputs returns outputs newest first.
	ListOutputs(ctx context.Context, tenant TenantID, f OutputFilter) ([]Output, error)
	// PruneOutputs deletes outputs created before cutoff and reports how
	// many (retention is OutputRetention).
	PruneOutputs(ctx context.Context, tenant TenantID, cutoff time.Time) (int, error)
}

// TriggerStore keeps trigger registrations, the record of fires, and the
// calendar FireMap.
type TriggerStore interface {
	// PutTrigger stores a registration, replacing the one with its id.
	PutTrigger(ctx context.Context, t TriggerRecord) error
	// GetTrigger returns a registration, or trigger_not_found.
	GetTrigger(ctx context.Context, tenant TenantID, id TriggerID) (TriggerRecord, error)
	// ListTriggers returns registrations ordered by flow, index; flow ""
	// lists every flow.
	ListTriggers(ctx context.Context, tenant TenantID, flow string) ([]TriggerRecord, error)
	// DeleteTriggers removes a flow's registrations.
	DeleteTriggers(ctx context.Context, tenant TenantID, flow string) error
	// AdvanceTrigger is the compare-and-set that enqueues the next fire
	// once: it sets NextAt and NextJob when the stored NextAt equals
	// expected (nil for none), and returns false when another worker won.
	AdvanceTrigger(ctx context.Context, tenant TenantID, id TriggerID, expected, next *time.Time, job JobID) (bool, error)

	// RecordFire records a fire, including the ones that started nothing.
	RecordFire(ctx context.Context, f Fire) error
	// ListFires returns a trigger's fires newest first; trigger "" lists a
	// flow's; limit 0 means the store default.
	ListFires(ctx context.Context, tenant TenantID, flow string, trigger TriggerID, limit int) ([]Fire, error)

	// PutPlannedFire stores a pending calendar fire, replacing the one with
	// its (trigger, key).
	PutPlannedFire(ctx context.Context, p PlannedFire) error
	// GetPlannedFire returns a pending fire; ok is false when there is none.
	GetPlannedFire(ctx context.Context, tenant TenantID, trigger TriggerID, key string) (PlannedFire, bool, error)
	// ListPlannedFires returns a trigger's pending fires ordered by At.
	ListPlannedFires(ctx context.Context, tenant TenantID, trigger TriggerID) ([]PlannedFire, error)
	// DeletePlannedFire removes a pending fire; absent is not an error.
	DeletePlannedFire(ctx context.Context, tenant TenantID, trigger TriggerID, key string) error
}

// Tenants is the registry of tenants a store serves.
type Tenants interface {
	// Ensure registers a tenant, creating its storage; registering a known
	// tenant is a no-op.
	Ensure(ctx context.Context, t TenantID) error
	// List returns the registered tenants, sorted.
	List(ctx context.Context) ([]TenantID, error)
}
