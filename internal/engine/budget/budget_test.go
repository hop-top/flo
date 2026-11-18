package budget

import (
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	specrun "hop.top/flo/spec/run"
)

func TestChargeKeepsSpendAndReportsFirstLimitPassed(t *testing.T) {
	m := New(coretest.NewClock(time.Time{}), specrun.Budget{USD: 0.5, Calls: 10}, core.Usage{})
	if ex := m.Charge(core.Cost{USD: 0.5}, 1); ex != nil {
		t.Fatalf("spending exactly the limit is within it, got %v", ex)
	}
	ex := m.Charge(core.Cost{USD: 0.1}, 1)
	if ex == nil || ex.Limit != LimitUSD || ex.Max != 0.5 {
		t.Fatalf("exceeded = %+v, want usd over 0.5", ex)
	}
	if got := m.Usage(); got.USD != 0.6 || got.Calls != 2 {
		t.Errorf("usage = %+v, want the overrun kept", got)
	}
	if m.Admit(core.Cost{}, 1) == nil {
		t.Error("exceeding is sticky: Admit must refuse")
	}
}

func TestAdmitComparesEstimateWithRemaining(t *testing.T) {
	m := New(coretest.NewClock(time.Time{}), specrun.Budget{USD: 0.01, Tokens: 1000}, core.Usage{Cost: core.Cost{USD: 0.008}})
	if ex := m.Admit(core.Cost{USD: 0.002}, 1); ex != nil {
		t.Errorf("an estimate that fits was refused: %v", ex)
	}
	m2 := New(coretest.NewClock(time.Time{}), specrun.Budget{USD: 0.01}, core.Usage{Cost: core.Cost{USD: 0.008}})
	if ex := m2.Admit(core.Cost{USD: 0.003}, 1); ex == nil || ex.Limit != LimitUSD {
		t.Errorf("an estimate past the remainder must be refused, got %v", ex)
	}
	m3 := New(coretest.NewClock(time.Time{}), specrun.Budget{Calls: 2}, core.Usage{Calls: 2})
	if ex := m3.Admit(core.Cost{}, 1); ex == nil || ex.Limit != LimitCalls {
		t.Errorf("a third call against calls: 2 must be refused, got %v", ex)
	}
}

func TestDurationExcludesParkedTime(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	m := New(clock, specrun.Budget{Duration: 10 * time.Minute}, core.Usage{})
	clock.Advance(6 * time.Minute)
	m.Pause()
	clock.Advance(24 * time.Hour)
	if got := m.Usage().Active; got != 6*time.Minute {
		t.Fatalf("active while parked = %v, want 6m", got)
	}
	if ex := m.Admit(core.Cost{}, 1); ex != nil {
		t.Fatalf("a day parked must not count: %v", ex)
	}
	m.Resume()
	clock.Advance(3 * time.Minute)
	if ex := m.Admit(core.Cost{}, 1); ex != nil {
		t.Fatalf("9m active of 10m: %v", ex)
	}
	clock.Advance(2 * time.Minute)
	if ex := m.Admit(core.Cost{}, 1); ex == nil || ex.Limit != LimitDuration {
		t.Fatalf("11m active of 10m must be refused, got %v", ex)
	}
}

func TestStartCarriesEarlierTurns(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	m := New(clock, specrun.Budget{Duration: time.Hour}, core.Usage{Active: 59 * time.Minute})
	clock.Advance(2 * time.Minute)
	if ex := m.Admit(core.Cost{}, 1); ex == nil {
		t.Error("time banked by an earlier turn must count")
	}
}

func TestChildCountsTowardParent(t *testing.T) {
	run := New(coretest.NewClock(time.Time{}), specrun.Budget{USD: 1}, core.Usage{})
	step := run.Child("s", specrun.Budget{USD: 0.4})
	if ex := step.Charge(core.Cost{USD: 0.3}, 1); ex != nil {
		t.Fatal(ex)
	}
	if got := run.Usage().USD; got != 0.3 {
		t.Errorf("parent usd = %v, want the step's spend", got)
	}
	ex := step.Charge(core.Cost{USD: 0.2}, 1)
	if ex == nil || ex.Scope != "s" || ex.Limit != LimitUSD {
		t.Fatalf("step cap: exceeded = %+v", ex)
	}
	if run.Exceeded() != nil {
		t.Error("the run's own budget is not exceeded by 0.5 of 1")
	}
	other := run.Child("o", specrun.Budget{})
	if ex := other.Charge(core.Cost{USD: 0.6}, 1); ex == nil || ex.Scope != "" {
		t.Errorf("a step with no cap must still trip the run's: %+v", ex)
	}
	if other.Exceeded() == nil {
		t.Error("child reports the parent's exceeded state")
	}
}

func TestExceededFailure(t *testing.T) {
	f := (&Exceeded{Scope: "s", Limit: LimitCalls, Max: 2, Used: 3}).Failure()
	if f.Code != core.CodeBudgetExceeded || f.Detail["limit"] != LimitCalls || f.Detail["step"] != "s" {
		t.Errorf("failure = %+v", f)
	}
}
