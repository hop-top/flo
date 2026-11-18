package core

import "context"

// Grants decides what a profile may do. It is deny by default; a grant is a
// capability (`<domain>.<verb>`) or the wildcard "*" (node.md §12). The
// owner's profile holds the wildcard; the engine's own profile holds the
// union of what installed packs declare (phase 1).
type Grants interface {
	// Covers reports whether profile holds capability, directly or through
	// the wildcard.
	Covers(ctx context.Context, tenant TenantID, profile, capability string) (bool, error)
	// CanUseProfile reports whether caller may name profile as a run_as.
	CanUseProfile(ctx context.Context, tenant TenantID, caller, profile string) (bool, error)
	// CheckFlow is the check of flow.md V18: the caller may use the flow's
	// run_as, and that profile holds every capability. A failure is
	// grant_denied with Detail "missing" listing the capabilities.
	CheckFlow(ctx context.Context, c GrantCheck) error
	// Set grants a capability. It reports whether anything changed. For a
	// scope the governance backend provisions it fails with
	// grant_provisioned.
	Set(ctx context.Context, tenant TenantID, profile, capability string) (changed bool, err error)
	// Unset revokes a grant; same rules as Set.
	Unset(ctx context.Context, tenant TenantID, profile, capability string) (changed bool, err error)
	// List returns the grants of a profile, or of every profile when empty.
	List(ctx context.Context, tenant TenantID, profile string) ([]Grant, error)
}

// Admission is the port in front of every step with a side effect above
// `read` and every step whose run_as is not the starter's (node.md §10.2).
// Phase 1 implements it locally (allow what the grants cover, record the
// receipt); a governance backend can replace it.
type Admission interface {
	// Admit decides and records the receipt. Asking again for the same
	// (run, step run, digest) returns the current decision of that receipt:
	// a deferred decision becomes allow or deny when the backend decides.
	Admit(ctx context.Context, req AdmissionRequest) (AdmissionDecision, error)
}

// ApprovalService is the approval service: it opens the approvals steps park on,
// records decisions, and answers the guard of irreversible steps.
type ApprovalService interface {
	// Open persists the approval a parked step asked for and creates its
	// task for the surface tlc. Idempotent per (run, step run). It emits
	// flo.run.approval.requested.
	Open(ctx context.Context, tenant TenantID, run RunID, step StepRun, req ApprovalRequest) (Approval, error)
	// Get returns an approval, or approval_not_found.
	Get(ctx context.Context, tenant TenantID, id ApprovalID) (Approval, error)
	// List returns approvals, newest first.
	List(ctx context.Context, tenant TenantID, f ApprovalFilter) ([]Approval, error)
	// Decide records item decisions (`flo approval set`). Idempotent: items
	// already decided keep their decision, so the same request twice yields
	// the same state. When the approval completes it closes the task and
	// resumes the run, once.
	Decide(ctx context.Context, tenant TenantID, req DecideRequest) (DecideResult, error)
	// CancelRun rejects every undecided item of the run's open approvals
	// with reason run_cancelled and closes them; the rejections are not
	// reported to a trust provider (flow.md §11.7).
	CancelRun(ctx context.Context, tenant TenantID, run RunID) error
	// GuardSatisfied reports whether an irreversible step may run: one of
	// its guarding approvals is complete, or its trust class is at level
	// auto.
	GuardSatisfied(ctx context.Context, q GuardQuery) (bool, error)
}

// ActivationGate decides whether a flow version may be activated. Validation
// and grants are checked by the caller first; the gate adds the rule about
// tests. Phase 1 implements "validated" (D7); the testing work swaps in
// "has a passing test of this exact version".
type ActivationGate interface {
	// Allow returns nil when v may be activated, else an *Error (invalid_flow
	// and its kin) naming what is missing.
	Allow(ctx context.Context, tenant TenantID, v FlowVersion) error
}

// Tasks is the port to the task tool (tlc by default): the human surface of
// approvals and the accountable task of a run (flow.md §7.8).
type Tasks interface {
	// Create makes a task and returns its reference.
	Create(ctx context.Context, tenant TenantID, spec TaskSpec) (TaskRef, error)
	// Get returns a task, or task_not_found.
	Get(ctx context.Context, tenant TenantID, ref TaskRef) (TaskInfo, error)
	// Close closes a task with an outcome (approved or rejected) and a note.
	// Closing a closed task is not an error.
	Close(ctx context.Context, tenant TenantID, ref TaskRef, outcome TaskState, note string) error
}

// Trust reads and updates a pack-declared trust provider (node.md §10.3).
// The provider's tool owns the counters; the engine never writes them.
type Trust interface {
	// Levels returns the provider's level per action class.
	Levels(ctx context.Context, tenant TenantID, provider string, params map[string]any) ([]TrustLevel, error)
	// Decide reports the person's decisions to the provider, once per
	// approval, after every item has one. Entries the provider skipped are
	// returned in TrustApplied.Skipped.
	Decide(ctx context.Context, tenant TenantID, provider string, params map[string]any, decisions []TrustDecision) (TrustApplied, error)
	// Set promotes or demotes an action class (`flo trust set`). A provider
	// may refuse a level: trust_capped.
	Set(ctx context.Context, tenant TenantID, provider, class string, level TrustLevelName, reason string) error
}
