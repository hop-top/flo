// Package budget meters a run against its budget (flow.md §7.7).
//
// A Meter tracks usd, tokens, calls and active wall-clock time. The executor
// asks it before an attempt starts (Admit: the node's estimate against what
// is left) and tells it what the attempt really cost (Charge). A step with
// its own budget gets a child Meter: its spend counts toward the parent's.
//
// Exceeding a limit is sticky: once a Meter has been exceeded every later
// Admit fails, so the executor stops starting work while steps already in
// flight finish. Duration is wall-clock time since the run started with the
// time a run spends parked excluded: Pause freezes it, Resume restarts it.
package budget

import (
	"fmt"
	"sync"
	"time"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// Limits names. They are the values of BudgetEvent.Limit.
const (
	LimitUSD      = "usd"
	LimitTokens   = "tokens"
	LimitCalls    = "calls"
	LimitDuration = "duration"
)

// Exceeded says which limit was passed and by what.
type Exceeded struct {
	// Scope is the step id of a step-level budget, empty for the run's.
	Scope string
	// Limit is one of the Limit* names.
	Limit string
	// Max and Used are in the limit's unit: usd, tokens, calls, seconds.
	Max, Used float64
}

// Error implements error.
func (e *Exceeded) Error() string {
	who := "run"
	if e.Scope != "" {
		who = "step " + e.Scope
	}
	return fmt.Sprintf("%s budget exceeded: %s %g of %g", who, e.Limit, e.Used, e.Max)
}

// Failure returns the failure a run ends with when this limit stopped it.
func (e *Exceeded) Failure() *core.Failure {
	f := core.FailureOf(core.New(core.CodeBudgetExceeded, e.Error()))
	f.Detail = map[string]any{"limit": e.Limit, "max": e.Max, "used": e.Used}
	if e.Scope != "" {
		f.Detail["step"] = e.Scope
	}
	return f
}

// Meter tracks one budget. It is safe for concurrent use.
type Meter struct {
	clock  core.Clock
	limits specrun.Budget
	scope  string
	parent *Meter

	mu       sync.Mutex
	used     core.Usage // Active holds only the time banked before since
	since    time.Time  // when the current running stretch began
	running  bool
	exceeded *Exceeded
}

// New returns a running Meter for a run that has already used start (the
// usage of earlier turns). A zero limits value meters without limiting.
func New(clock core.Clock, limits specrun.Budget, start core.Usage) *Meter {
	return &Meter{clock: clock, limits: limits, used: start, since: clock.Now(), running: true}
}

// Child returns a Meter for a step's own budget. Its Admit and Charge also
// go through m, so the step's spend counts toward the run's.
func (m *Meter) Child(scope string, limits specrun.Budget) *Meter {
	return &Meter{clock: m.clock, limits: limits, scope: scope, parent: m, since: m.clock.Now(), running: true}
}

// Usage returns what has been used so far, time included.
func (m *Meter) Usage() core.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usageLocked()
}

func (m *Meter) usageLocked() core.Usage {
	u := m.used
	if m.running {
		u.Active += m.clock.Now().Sub(m.since)
	}
	return u
}

// Pause stops the clock of the duration limit: the run is parked.
func (m *Meter) Pause() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		m.used.Active += m.clock.Now().Sub(m.since)
		m.running = false
	}
}

// Resume restarts the clock after a Pause.
func (m *Meter) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		m.since, m.running = m.clock.Now(), true
	}
}

// Exceeded returns the limit that stopped this meter or one of its parents,
// nil while none has.
func (m *Meter) Exceeded() *Exceeded {
	m.mu.Lock()
	ex := m.exceeded
	m.mu.Unlock()
	if ex == nil && m.parent != nil {
		return m.parent.Exceeded()
	}
	return ex
}

// Admit is the check before an attempt starts: spending est and one more
// call must still fit, and the duration must not already be over. A refusal
// is sticky.
func (m *Meter) Admit(est core.Cost, calls int) *Exceeded {
	if ex := m.admit(est, calls); ex != nil {
		return ex
	}
	if m.parent != nil {
		return m.parent.Admit(est, calls)
	}
	return nil
}

func (m *Meter) admit(est core.Cost, calls int) *Exceeded {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exceeded != nil {
		return m.exceeded
	}
	u := m.usageLocked()
	next := u.Add(core.Usage{Cost: est, Calls: calls})
	ex := m.over(next, u.Active)
	m.exceeded = ex
	return ex
}

// Charge adds the actual cost of a finished attempt and reports the limit it
// pushed the budget over, if any. The spend is kept even when it exceeds:
// the attempt happened.
func (m *Meter) Charge(cost core.Cost, calls int) *Exceeded {
	ex := m.charge(cost, calls)
	if m.parent != nil {
		if pex := m.parent.Charge(cost, calls); ex == nil {
			ex = pex
		}
	}
	return ex
}

func (m *Meter) charge(cost core.Cost, calls int) *Exceeded {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used.Cost = m.used.Cost.Add(cost)
	m.used.Calls += calls
	if m.exceeded != nil {
		return m.exceeded
	}
	u := m.usageLocked()
	m.exceeded = m.over(u, u.Active)
	return m.exceeded
}

// over finds the first limit that u passes. Money, tokens and calls are
// "more than" the limit; the duration is over when the time already used
// exceeds it (an attempt in flight is never interrupted).
func (m *Meter) over(u core.Usage, active time.Duration) *Exceeded {
	l := m.limits
	switch {
	case l.USD > 0 && u.USD > l.USD:
		return &Exceeded{Scope: m.scope, Limit: LimitUSD, Max: l.USD, Used: u.USD}
	case l.Tokens > 0 && u.Tokens > l.Tokens:
		return &Exceeded{Scope: m.scope, Limit: LimitTokens, Max: float64(l.Tokens), Used: float64(u.Tokens)}
	case l.Calls > 0 && u.Calls > l.Calls:
		return &Exceeded{Scope: m.scope, Limit: LimitCalls, Max: float64(l.Calls), Used: float64(u.Calls)}
	case l.Duration > 0 && active > l.Duration:
		return &Exceeded{Scope: m.scope, Limit: LimitDuration, Max: l.Duration.Seconds(), Used: active.Seconds()}
	}
	return nil
}
