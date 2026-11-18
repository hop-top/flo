package core

import (
	"time"

	"hop.top/flo/spec/node"
)

// Outcome is how one dispatch ended.
type Outcome string

// Outcomes.
const (
	// OutcomeOK: the step produced its output.
	OutcomeOK Outcome = "ok"
	// OutcomeFailed: the step failed; Result.Err says why.
	OutcomeFailed Outcome = "failed"
	// OutcomePark: the step needs a person; Result.Park says what for.
	OutcomePark Outcome = "park"
)

// Effect says whether a dispatch may have changed the outside world. The
// executor needs it to decide what a retry may do (node.md §11): a failure
// that is EffectNone is retried whatever the node; one that is EffectUnknown
// is retried only when the node is idempotent or carries an idempotency key.
type Effect string

// Effects.
const (
	// EffectNone: known not to have executed (spawn failure, connection
	// refused, HTTP 429 or 503 declared safe, input rejected).
	EffectNone Effect = "none"
	// EffectDone: executed; the outcome is known.
	EffectDone Effect = "done"
	// EffectUnknown: may have executed (timeout, crash, lost connection).
	EffectUnknown Effect = "unknown"
)

// RunContext is the read-only `run` namespace a node sees (node.md §3.1).
type RunContext struct {
	ID        RunID     `json:"id"`
	Flow      string    `json:"flow"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Iteration int       `json:"iteration,omitempty"`
}

// ExecInput is a core `exec` step, rendered.
type ExecInput struct {
	Argv      []string
	Cwd       string
	Env       map[string]string
	Timeout   time.Duration
	StdoutMax int
}

// HumanInput is a core `human` step, rendered.
type HumanInput struct {
	Title       string
	Description string
	Assignee    string
	Timeout     time.Duration
	// OnTimeout is approve, reject or keep_waiting.
	OnTimeout string
}

// Resume is the completed approval a parked step is re-dispatched with. The
// executor sets it when the approval a step waited on is complete, so the
// node (flo/approval@1, a human step) can turn the decisions into its output.
type Resume struct {
	Approval Approval
}

// Invocation is one request to execute a step attempt. The executor renders
// `with` and fills the rest from the run; the dispatcher routes it by Kind
// and the runner places it. It is a value: a handler may keep it.
type Invocation struct {
	Tenant TenantID
	Run    RunContext
	// StepRun is the record this attempt belongs to.
	StepRun StepRunID
	StepID  string
	// Index is the for_each element position, nil for a plain step.
	Index   *int
	Attempt int // 1-based

	Kind StepKind
	// Node is the resolved spec of a node step; nil for core kinds.
	Node       *node.Spec
	NodeSource NodeSource
	// In is the rendered `with`, typed. The dispatcher validates it against
	// Node.Inputs and applies defaults.
	In map[string]any
	// Config is the pack configuration of the node (`config.<pack>.*`).
	Config map[string]any

	Exec  *ExecInput
	Human *HumanInput
	// Agent is the agent name of an `agent` step.
	Agent string

	// RunAs is the aps profile the step acts as; Principal the person the
	// run acts for.
	RunAs     string
	Principal string

	DryRun  bool
	Timeout time.Duration
	// IdempotencyKey is passed to the target (FLO_IDEMPOTENCY_KEY, the
	// Idempotency-Key header).
	IdempotencyKey string
	// Resume is set when a parked step is re-dispatched after its approval
	// completed.
	Resume *Resume
}

// ApprovalRequest is what a parking step asks the engine to open. Items
// already decided by trust carry their decision (verdict auto).
type ApprovalRequest struct {
	Kind     ApprovalKind
	Title    string
	Surface  string // tlc | none
	Trust    string // trust provider name
	Context  map[string]any
	Verdicts []Verdict
	Items    []ApprovalItem
	// Timeout is how long undecided items wait before OnTimeout applies;
	// zero waits for ever.
	Timeout time.Duration
	// OnTimeout is reject (approval node) or approve, reject, keep_waiting
	// (human step).
	OnTimeout string
	Assignee  string
	// Description is the task body (human steps and tasks).
	Description string
}

// Result is what a dispatch returns.
type Result struct {
	Outcome Outcome
	// Output is the value `results.<step>` reads, typed per the node spec.
	Output any
	// Raw is the raw exec result (exit_code, stdout, stderr, duration_ms,
	// truncated), kept in the step record.
	Raw       any
	Truncated bool
	Cost      Cost
	// Calls is how many node or command invocations the dispatch made, for
	// the budget's `calls`; 0 counts as 1.
	Calls int
	// Plan is set when the node was dry-run.
	Plan *Plan
	// Err is set when Outcome is OutcomeFailed.
	Err *Error
	// Effect says whether the world may have changed.
	Effect Effect
	// Park is set when Outcome is OutcomePark.
	Park *ApprovalRequest
}

// BillableCalls returns Calls, at least 1.
func (r Result) BillableCalls() int {
	if r.Calls < 1 {
		return 1
	}
	return r.Calls
}

// Placement says where a step runs (node.md §15).
type Placement struct {
	Runner    string
	Isolation string
}

// CommandSpec is one process to run. Nothing is passed through a shell.
type CommandSpec struct {
	Argv []string
	// Env is added to the child's environment.
	Env map[string]string
	Cwd string
	// Stdin is written to the child's standard input.
	Stdin []byte
	// Profile, when set, runs the command as `aps run <profile> -- argv`;
	// empty runs it directly.
	Profile string
	// Timeout bounds the process; on expiry the process group is killed.
	// Zero means no bound.
	Timeout time.Duration
	// MaxOutput caps each of stdout and stderr in bytes; excess is dropped
	// and flagged. Zero means 1 MiB.
	MaxOutput int
}

// DefaultMaxOutput is the default cap on a command's stdout and stderr.
const DefaultMaxOutput = 1 << 20

// CommandResult is how a process ended. A non-zero exit is a result, not an
// error.
type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
	// Truncated reports that stdout or stderr hit MaxOutput.
	Truncated bool
	// TimedOut reports that the process was killed at Timeout; Stdout and
	// Stderr hold what it had written.
	TimedOut bool
}

// CheckRequest is a contract check of a step's output (flow.md §5.8).
type CheckRequest struct {
	Contract string
	// URL is the gateway base URL.
	URL  string
	Body any
}

// CheckResult is a gateway verdict. 200 passes; 422 does not, with the
// gateway's message.
type CheckResult struct {
	Passed  bool
	Message string
}

// NodeScope is where a node reference is resolved from.
type NodeScope struct {
	Tenant TenantID
	// Bundle is the flow bundle directory whose nodes/ layer takes
	// precedence (flow.md §2.4); empty for none.
	Bundle string
}

// NodeInfo is a resolved node: its spec and where it came from.
type NodeInfo struct {
	Spec   *node.Spec
	Source NodeSource
}

// JobType names a kind of job.
type JobType string

// Job types.
const (
	// JobRunAdvance advances one run (D2: one job per run). Payload:
	// RunAdvancePayload. Key: the run id.
	JobRunAdvance JobType = "run.advance"
	// JobTriggerFire fires one schedule occurrence. Payload:
	// TriggerFirePayload. Key: trigger id and instant.
	JobTriggerFire JobType = "trigger.fire"
	// JobCalendarFire fires one planned calendar fire. Payload:
	// TriggerFirePayload. Key: the planned fire's key.
	JobCalendarFire JobType = "calendar.fire"
	// JobCalendarSync re-plans a calendar trigger's fires. Payload:
	// TriggerFirePayload.
	JobCalendarSync JobType = "calendar.sync"
	// JobApprovalPoll polls open approvals' tasks and timeouts. Payload:
	// ApprovalPollPayload.
	JobApprovalPoll JobType = "approval.poll"
)

// RunAdvancePayload is the payload of a JobRunAdvance job.
type RunAdvancePayload struct {
	Tenant TenantID `json:"tenant"`
	Run    RunID    `json:"run"`
}

// TriggerFirePayload is the payload of trigger and calendar jobs.
type TriggerFirePayload struct {
	Tenant  TenantID  `json:"tenant"`
	Flow    string    `json:"flow"`
	Trigger TriggerID `json:"trigger"`
	// At is the occurrence instant the job fires for.
	At time.Time `json:"at"`
	// Key is the planned fire's key (calendar).
	Key string `json:"key,omitempty"`
}

// ApprovalPollPayload is the payload of a JobApprovalPoll job.
type ApprovalPollPayload struct {
	Tenant TenantID `json:"tenant"`
}

// JobSpec asks the queue for one job.
type JobSpec struct {
	Type   JobType
	Tenant TenantID
	// Key identifies the job's subject. At most one job that is not yet
	// finished exists per (Type, Tenant, Key): enqueuing again returns the
	// existing job's id.
	Key     string
	Payload any
	// At delays the job until then; nil runs it now.
	At          *time.Time
	MaxAttempts int
}

// JobStatus is where a job stands.
type JobStatus string

// Job statuses.
const (
	JobPending   JobStatus = "pending"
	JobActive    JobStatus = "active"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Finished reports a status a job never leaves.
func (s JobStatus) Finished() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Job is a claimed unit of work handed to a handler.
type Job struct {
	ID          JobID
	Type        JobType
	Tenant      TenantID
	Key         string
	Payload     []byte // JSON
	Attempt     int    // 1-based
	ScheduledAt time.Time
}

// JobInfo is a job and its status.
type JobInfo struct {
	Job
	Status JobStatus
	Error  string
}

// JobFilter selects jobs. Zero fields do not filter.
type JobFilter struct {
	Type     JobType
	Tenant   TenantID
	Key      string
	Statuses []JobStatus
}
