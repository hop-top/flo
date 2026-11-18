package core

import (
	"errors"
	"fmt"
	"testing"
)

// The exit-code table is part of the CLI contract (design doc, 12fc F4/F11):
// pin it row by row so a change is a visible decision.
func TestCodeTablePinned(t *testing.T) {
	want := map[Code]int{
		// 1 general
		CodeRunFailed: 1, CodeCheckFailed: 1, CodeBudgetExceeded: 1, CodeOutputInvalid: 1, CodeNodeReportedFailure: 1,
		CodeTimeout: 1, CodeStateTooLarge: 1, CodeDecisionNotRecorded: 1, CodeUpstreamFailed: 1, CodeUncertain: 1,
		// 2 usage
		CodeInvalidFlow: 2, CodeFlowNotTested: 2, CodeInvalidVar: 2, CodeConfirmationRequired: 2, CodeTTYRequired: 2, CodeEngineNode: 2,
		CodeEngineFeature: 2, CodeReservedKey: 2, CodeUnguardedIrreversible: 2, CodePrivacyViolation: 2,
		CodeCapabilityMismatch: 2, CodeUnknownFunction: 2, CodeSyntaxError: 2, CodeCycle: 2, CodeInvalidNode: 2,
		// 3 not found
		CodeFlowNotFound: 3, CodeRunNotFound: 3, CodeNodeNotFound: 3, CodeApprovalNotFound: 3, CodeSecretNotFound: 3,
		// 4 conflict
		CodeFlowVersionConflict: 4, CodeDraftStale: 4, CodeGrantProvisioned: 4, CodeRunStateConflict: 4, CodeApprovalConflict: 4,
		// 5 permission
		CodeGrantDenied: 5, CodeAdmissionDenied: 5,
		// 6 transient
		CodeProviderRateLimited: 6, CodeEngineUnreachable: 6, CodeCheckUnavailable: 6,
		// 7 flo: waiting for approval
		CodeAwaitingApproval: 7,
	}
	for code, exit := range want {
		if got := code.Exit(); got != exit {
			t.Errorf("%s exits %d, want %d", code, got, exit)
		}
	}
}

func TestCodeTableShape(t *testing.T) {
	seen := map[Code]bool{}
	for _, e := range Codes() {
		if seen[e.Code] {
			t.Errorf("duplicate code %s", e.Code)
		}
		seen[e.Code] = true
		if e.Exit < 1 || e.Exit > 7 {
			t.Errorf("%s: exit %d outside 1..7", e.Code, e.Exit)
		}
		if e.Transience == "" {
			t.Errorf("%s: no transience", e.Code)
		}
		if string(e.Code) == "" || string(e.Code) != toSnake(string(e.Code)) {
			t.Errorf("code %q is not snake_case", e.Code)
		}
	}
	if Code("nope").Exit() != ExitGeneral || Code("nope").Transience() != TransienceUnknown || Code("nope").Known() {
		t.Error("an unknown code is a general failure of unknown transience")
	}
	for _, c := range []Code{CodeProviderRateLimited, CodeEngineUnreachable, CodeTimeout, CodeCheckUnavailable} {
		if c.Transience() != TransienceTransient {
			t.Errorf("%s must be transient", c)
		}
	}
	for _, c := range []Code{CodeInvalidFlow, CodeGrantDenied, CodeCheckFailed, CodeFlowNotFound} {
		if c.Transience() != TransiencePermanent {
			t.Errorf("%s must be permanent", c)
		}
	}
}

func toSnake(s string) string {
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return s
}

func TestErrorBasics(t *testing.T) {
	cause := errors.New("disk full")
	err := Errorf(CodeInternal, "write %s: %w", "state", cause)
	if err.Error() != "internal: write state: disk full" {
		t.Errorf("Error() = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("%w operand must be the cause")
	}
	if !errors.Is(fmt.Errorf("ctx: %w", err), New(CodeInternal, "")) {
		t.Error("errors.Is matches by code through wrapping")
	}
	if errors.Is(err, New(CodeRunFailed, "")) {
		t.Error("a different code must not match")
	}
	if CodeOf(fmt.Errorf("x: %w", err)) != CodeInternal || CodeOf(errors.New("plain")) != "" || CodeOf(nil) != "" {
		t.Error("CodeOf")
	}
	if !IsCode(err, CodeInternal) {
		t.Error("IsCode")
	}
}

func TestWrapKeepsFirstCode(t *testing.T) {
	inner := New(CodeGrantDenied, "no")
	if got := Wrap(CodeInternal, fmt.Errorf("outer: %w", inner)); got.Code != CodeGrantDenied {
		t.Errorf("Wrap re-coded: %s", got.Code)
	}
	if got := Wrap(CodeInternal, errors.New("plain")); got.Code != CodeInternal {
		t.Errorf("Wrap = %s", got.Code)
	}
	if Wrap(CodeInternal, nil) != nil {
		t.Error("Wrap(nil) must be nil")
	}
}

func TestExitCodeAndClass(t *testing.T) {
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 || ExitCode(New(CodeFlowNotFound, "")) != 3 ||
		ExitCode(fmt.Errorf("w: %w", New(CodeAwaitingApproval, ""))) != 7 {
		t.Error("ExitCode")
	}
	e := New(CodeInternal, "x")
	if e.Class() != TransiencePermanent || e.WithTransience(TransienceTransient).Class() != TransienceTransient || !e.WithTransience(TransienceTransient).Retryable() {
		t.Error("transience override")
	}
	if e.Class() != TransiencePermanent {
		t.Error("WithTransience must copy, not mutate")
	}
	if ClassOf(errors.New("x")) != TransienceUnknown {
		t.Error("a foreign error is unknown")
	}
}

func TestWithDetailCopies(t *testing.T) {
	e := New(CodeGrantDenied, "no").WithDetail("a", 1)
	f := e.WithDetail("b", 2)
	if len(e.Detail) != 1 || len(f.Detail) != 2 {
		t.Errorf("details: %v %v", e.Detail, f.Detail)
	}
}

func TestAsCLIError(t *testing.T) {
	err := Errorf(CodeGrantDenied, "profile noor lacks mail.send").
		WithSuggestion("flo grant set noor mail.send").
		WithDetail("missing", []string{"mail.send"})
	env := err.AsCLIError()
	if env.Code != "grant_denied" || env.ExitCode != 5 || env.Transience != "permanent" || env.SuggestedFix != "flo grant set noor mail.send" {
		t.Errorf("envelope = %+v", env)
	}
	if !errors.Is(env, New(CodeGrantDenied, "")) {
		t.Error("the envelope must keep the original error for errors.Is")
	}
}

func TestFailureRoundTrip(t *testing.T) {
	err := Errorf(CodeProviderRateLimited, "slow down").WithDetail("retry_after", "30s")
	f := FailureOf(err)
	back := f.Err()
	if back.Code != CodeProviderRateLimited || back.Class() != TransienceTransient || back.Detail["retry_after"] != "30s" {
		t.Errorf("round trip = %+v", back)
	}
	if FailureOf(nil) != nil || (*Failure)(nil).Err() != nil {
		t.Error("nil in, nil out")
	}
	if got := FailureOf(errors.New("boom")); got.Code != CodeInternal || got.Transience != TransienceUnknown {
		t.Errorf("foreign error = %+v", got)
	}
}
