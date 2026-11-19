package core

import (
	"errors"
	"fmt"
	"sort"

	"hop.top/kit/go/console/output/envelope"
)

// Code is the stable, machine-readable name of a failure. Messages change;
// codes do not.
type Code string

// Transience classifies a failure for retry decisions (12-factor F4).
type Transience string

// Transience classes. They are kit's envelope values.
const (
	// TransienceTransient: a retry may clear it.
	TransienceTransient Transience = envelope.TransienceTransient
	// TransiencePermanent: the same request fails the same way.
	TransiencePermanent Transience = envelope.TransiencePermanent
	// TransienceUnknown: not characterized.
	TransienceUnknown Transience = envelope.TransienceUnknown
)

// Exit codes of the flo CLI. 0 to 6 are the shared taxonomy of kit and the
// 12-factor AI-CLI spec; 7 is flo's own.
//
// Note: kit's envelope package names 7 for consent_refused, a class flo does
// not use (a missing confirmation is confirmation_required, exit 2).
const (
	ExitOK           = 0 // success, including no-op
	ExitGeneral      = 1 // run_failed, check_failed, budget_exceeded
	ExitUsage        = 2 // invalid_flow, confirmation_required, tty_required, engine_node
	ExitNotFound     = 3 // flow_not_found, run_not_found, node_not_found
	ExitConflict     = 4 // flow_version_conflict, grant_provisioned
	ExitPermission   = 5 // grant_denied, admission_denied
	ExitTransient    = 6 // provider_rate_limited, engine_unreachable
	ExitAwaiting     = 7 // run waiting for approval (run create --wait only)
	exitUnclassified = ExitGeneral
)

// Failures a step or a run can end with (exit 1 unless noted).
const (
	CodeRunFailed            Code = "run_failed"
	CodeCheckFailed          Code = "check_failed"
	CodeBudgetExceeded       Code = "budget_exceeded"
	CodeOutputInvalid        Code = "output_invalid"
	CodeInputInvalid         Code = "input_invalid"
	CodeNodeReportedFailure  Code = "node_reported_failure"
	CodeTimeout              Code = "timeout" // transient: the outcome is unknown
	CodeUpstreamFailed       Code = "upstream_failed"
	CodeStateTooLarge        Code = "state_too_large"
	CodeDecisionNotRecorded  Code = "decision_not_recorded"
	CodeUncertain            Code = "uncertain"
	CodeTrustCapped          Code = "trust_capped"
	CodeUnresolvedRef        Code = "unresolved_ref" // a reference found no value at run time
	CodeEvalError            Code = "eval_error"
	CodeRunCancelled         Code = "run_cancelled"
	CodeInternal             Code = "internal"
	CodePlacementRefused     Code = "placement_refused"
	CodeSecretNotFound       Code = "secret_not_found" // exit 3
	CodeIrreversibleUnsafe   Code = "irreversible_unguarded"
	CodeApprovalTimedOut     Code = "approval_timed_out"
	CodeAdmissionUnavailable Code = "admission_unavailable"
)

// Usage and validation failures (exit 2). The spec-module validation codes
// (syntax_error and the other cel codes) are listed so a flow error carries
// its exit code without a second table.
const (
	CodeUsage                 Code = "usage"
	CodeInvalidFlow           Code = "invalid_flow"
	CodeFlowNotTested         Code = "flow_not_tested" // activation needs a passing test of that exact version
	CodeInvalidVar            Code = "invalid_var"
	CodeInvalidNode           Code = "invalid_node"
	CodeInvalidDecision       Code = "invalid_decision"
	CodeReservedKey           Code = "reserved_key"
	CodeEngineNode            Code = "engine_node"
	CodeEngineFeature         Code = "engine_feature"
	CodeConfirmationRequired  Code = "confirmation_required"
	CodeTTYRequired           Code = "tty_required"
	CodeUnguardedIrreversible Code = "unguarded_irreversible"
	CodePrivacyViolation      Code = "privacy_violation"
	CodeCapabilityMismatch    Code = "capability_mismatch"
	CodeSyntaxError           Code = "syntax_error"
	CodeUndeclaredRef         Code = "undeclared_ref"
	CodeUnknownFunction       Code = "unknown_function"
	CodeTypeError             Code = "type_error"
	CodeNotBoolean            Code = "not_boolean"
	CodeNotList               Code = "not_list"
	CodeUnterminated          Code = "unterminated_placeholder"
	CodeUnknownStep           Code = "unknown_step"
	CodeUnknownStateKey       Code = "unknown_state_key"
	CodeOutOfScope            Code = "out_of_scope"
	CodeCycle                 Code = "cycle"
)

// Not found (exit 3).
const (
	CodeNotFound         Code = "not_found"
	CodeFlowNotFound     Code = "flow_not_found"
	CodeRunNotFound      Code = "run_not_found"
	CodeNodeNotFound     Code = "node_not_found"
	CodeApprovalNotFound Code = "approval_not_found"
	CodeItemNotFound     Code = "item_not_found"
	CodeTriggerNotFound  Code = "trigger_not_found"
	CodeTaskNotFound     Code = "task_not_found"
	CodeJobNotFound      Code = "job_not_found"
	CodeTenantNotFound   Code = "tenant_not_found"
	CodeOutputNotFound   Code = "output_not_found"
	CodeDraftNotFound    Code = "draft_not_found"
)

// Conflict (exit 4).
const (
	CodeFlowVersionConflict   Code = "flow_version_conflict"
	CodeDraftStale            Code = "draft_stale"
	CodeGrantProvisioned      Code = "grant_provisioned"
	CodeRunStateConflict      Code = "run_state_conflict"
	CodeApprovalConflict      Code = "approval_version_conflict"
	CodeFlowNotActive         Code = "flow_not_active"
	CodeDuplicate             Code = "duplicate"
	CodeNodeConflict          Code = "node_conflict" // two packs declare the same ns/name@N
	CodeTriggerCASLost        Code = "trigger_cas_lost"
	CodeApprovalAlreadyClosed Code = "approval_closed"
)

// Permission (exit 5), transient (exit 6) and waiting (exit 7).
const (
	CodeGrantDenied     Code = "grant_denied"
	CodeAdmissionDenied Code = "admission_denied"

	CodeProviderRateLimited Code = "provider_rate_limited"
	CodeEngineUnreachable   Code = "engine_unreachable"
	CodeCheckUnavailable    Code = "check_unavailable"

	CodeAwaitingApproval Code = "awaiting_approval"
)

type codeInfo struct {
	exit       int
	transience Transience
}

var codeTable = buildCodeTable()

func buildCodeTable() map[Code]codeInfo {
	t := map[Code]codeInfo{}
	set := func(exit int, tr Transience, codes ...Code) {
		for _, c := range codes {
			t[c] = codeInfo{exit: exit, transience: tr}
		}
	}
	set(ExitGeneral, TransiencePermanent,
		CodeRunFailed, CodeCheckFailed, CodeBudgetExceeded, CodeOutputInvalid, CodeInputInvalid,
		CodeNodeReportedFailure, CodeUpstreamFailed, CodeStateTooLarge, CodeDecisionNotRecorded,
		CodeUncertain, CodeTrustCapped, CodeUnresolvedRef, CodeEvalError, CodeRunCancelled,
		CodeInternal, CodePlacementRefused, CodeIrreversibleUnsafe, CodeApprovalTimedOut)
	set(ExitGeneral, TransienceTransient, CodeTimeout, CodeAdmissionUnavailable)
	set(ExitUsage, TransiencePermanent,
		CodeUsage, CodeInvalidFlow, CodeFlowNotTested, CodeInvalidVar, CodeInvalidNode, CodeInvalidDecision, CodeReservedKey,
		CodeEngineNode, CodeEngineFeature, CodeConfirmationRequired, CodeTTYRequired,
		CodeUnguardedIrreversible, CodePrivacyViolation, CodeCapabilityMismatch, CodeSyntaxError,
		CodeUndeclaredRef, CodeUnknownFunction, CodeTypeError, CodeNotBoolean, CodeNotList,
		CodeUnterminated, CodeUnknownStep, CodeUnknownStateKey, CodeOutOfScope, CodeCycle)
	set(ExitNotFound, TransiencePermanent,
		CodeNotFound, CodeFlowNotFound, CodeRunNotFound, CodeNodeNotFound, CodeApprovalNotFound, CodeItemNotFound,
		CodeTriggerNotFound, CodeTaskNotFound, CodeJobNotFound, CodeTenantNotFound, CodeOutputNotFound, CodeDraftNotFound,
		CodeSecretNotFound)
	set(ExitConflict, TransiencePermanent,
		CodeFlowVersionConflict, CodeDraftStale, CodeGrantProvisioned, CodeFlowNotActive, CodeDuplicate,
		CodeNodeConflict, CodeApprovalAlreadyClosed)
	// A lost race is worth one retry against fresh state.
	set(ExitConflict, TransienceTransient, CodeRunStateConflict, CodeApprovalConflict, CodeTriggerCASLost)
	set(ExitPermission, TransiencePermanent, CodeGrantDenied, CodeAdmissionDenied)
	set(ExitTransient, TransienceTransient, CodeProviderRateLimited, CodeEngineUnreachable, CodeCheckUnavailable)
	set(ExitAwaiting, TransienceTransient, CodeAwaitingApproval)
	return t
}

// Exit returns the process exit code for c. An unknown code is a general
// failure (1).
func (c Code) Exit() int {
	if i, ok := codeTable[c]; ok {
		return i.exit
	}
	return exitUnclassified
}

// Transience returns the default retry class of c; unknown codes are
// TransienceUnknown.
func (c Code) Transience() Transience {
	if i, ok := codeTable[c]; ok {
		return i.transience
	}
	return TransienceUnknown
}

// Known reports whether c is in the code table.
func (c Code) Known() bool { _, ok := codeTable[c]; return ok }

// CodeEntry is one row of the code table.
type CodeEntry struct {
	Code       Code
	Exit       int
	Transience Transience
}

// Codes lists the code table ordered by exit code, then name. It is the
// source for the help text and for tests that pin the table.
func Codes() []CodeEntry {
	out := make([]CodeEntry, 0, len(codeTable))
	for c, i := range codeTable {
		out = append(out, CodeEntry{Code: c, Exit: i.exit, Transience: i.transience})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Exit != out[b].Exit {
			return out[a].Exit < out[b].Exit
		}
		return out[a].Code < out[b].Code
	})
	return out
}

// Error is the one error type of the engine. It carries a stable Code, a
// message, a retry class and optional detail for the caller.
type Error struct {
	Code    Code
	Message string
	// Transience overrides the code's default class when set.
	Transience Transience
	// Detail is structured context: the failing input, the missing
	// capabilities, the conflicting version.
	Detail map[string]any
	// Suggestion is the next step for the caller (12-factor F4).
	Suggestion string
	// Cause is the wrapped error.
	Cause error
}

// New returns an *Error with a code and message.
func New(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Errorf returns an *Error whose message is formatted. A %w operand becomes
// the Cause.
func Errorf(code Code, format string, args ...any) *Error {
	err := fmt.Errorf(format, args...)
	return &Error{Code: code, Message: err.Error(), Cause: errors.Unwrap(err)}
}

// Wrap returns an *Error with code that wraps err. A nil err gives nil. If
// err already is an *Error it is returned unchanged: the first code wins.
func Wrap(code Code, err error) *Error {
	if err == nil {
		return nil
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	return &Error{Code: code, Message: err.Error(), Cause: err}
}

// Error implements error: "code: message".
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is makes errors.Is(err, core.New(code, "")) match by code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error) //nolint:errorlint // matching our own type by code
	return ok && t != nil && t.Code == e.Code
}

// Exit returns the exit code of the error's code.
func (e *Error) Exit() int {
	if e == nil {
		return ExitOK
	}
	return e.Code.Exit()
}

// Class returns the retry class: the override, else the code's default.
func (e *Error) Class() Transience {
	if e == nil {
		return TransienceUnknown
	}
	if e.Transience != "" {
		return e.Transience
	}
	return e.Code.Transience()
}

// Retryable reports Class() == TransienceTransient.
func (e *Error) Retryable() bool { return e.Class() == TransienceTransient }

func (e *Error) clone() *Error { c := *e; return &c }

// WithDetail returns a copy with one detail key set.
func (e *Error) WithDetail(key string, v any) *Error {
	c := e.clone()
	m := make(map[string]any, len(e.Detail)+1)
	for k, x := range e.Detail {
		m[k] = x
	}
	m[key] = v
	c.Detail = m
	return c
}

// WithSuggestion returns a copy with the suggestion set.
func (e *Error) WithSuggestion(s string) *Error {
	c := e.clone()
	c.Suggestion = s
	return c
}

// WithTransience returns a copy with the retry class overridden.
func (e *Error) WithTransience(t Transience) *Error {
	c := e.clone()
	c.Transience = t
	return c
}

// AsCLIError satisfies kit's conversion interface, so a command that returns
// an *Error exits with its code and renders the structured envelope.
func (e *Error) AsCLIError() *envelope.Error {
	env := &envelope.Error{
		Code:         string(e.Code),
		Message:      e.Message,
		SuggestedFix: e.Suggestion,
		ExitCode:     e.Exit(),
		Transience:   string(e.Class()),
	}
	if e.Cause != nil {
		env.Cause = e.Cause.Error()
	}
	return env.Retaining(e)
}

// CodeOf returns the code of the first *Error in err's chain, or "" when
// there is none (or err is nil).
func CodeOf(err error) Code {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// IsCode reports whether the first *Error in err's chain has code.
func IsCode(err error, code Code) bool { return CodeOf(err) == code }

// ExitCode returns the exit code for err: 0 for nil, the code's exit for an
// *Error, 1 for any other error.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Exit()
	}
	return exitUnclassified
}

// ClassOf returns the retry class of err; non-*Error errors are unknown.
func ClassOf(err error) Transience {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Class()
	}
	return TransienceUnknown
}

// Failure is an *Error in the form stored in a record and sent in an event.
type Failure struct {
	Code       Code           `json:"code"`
	Message    string         `json:"message"`
	Transience Transience     `json:"transience,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// FailureOf converts err to a Failure; nil gives nil, a non-*Error becomes
// an internal failure with unknown transience.
func FailureOf(err error) *Failure {
	if err == nil {
		return nil
	}
	var ce *Error
	if !errors.As(err, &ce) {
		return &Failure{Code: CodeInternal, Message: err.Error(), Transience: TransienceUnknown}
	}
	return &Failure{Code: ce.Code, Message: ce.Message, Transience: ce.Class(), Detail: ce.Detail}
}

// Err returns the failure as an *Error; a nil Failure gives nil.
func (f *Failure) Err() *Error {
	if f == nil {
		return nil
	}
	return &Error{Code: f.Code, Message: f.Message, Transience: f.Transience, Detail: f.Detail}
}
