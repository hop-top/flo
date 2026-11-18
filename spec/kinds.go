package spec

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind selects how a step is dispatched: to an agent (the default), as a
// literal argv run, or to a human who approves or rejects it.
type Kind string

// Core step kinds.
const (
	KindAgent Kind = "agent"
	KindExec  Kind = "exec"
	KindHuman Kind = "human"
)

// Valid reports whether k names a known core kind. Empty is valid: it means
// the default, agent.
func (k Kind) Valid() bool {
	switch k {
	case "", KindAgent, KindExec, KindHuman:
		return true
	}
	return false
}

// ExecSpec describes an exec-kind step: a literal command run without an
// agent. Timeout is a duration string parsed on use so the spec stays a
// plain JSON/YAML document.
type ExecSpec struct {
	Argv      []string          `json:"argv" yaml:"argv"`
	Cwd       string            `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Timeout   string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	StdoutMax int               `json:"stdout_max,omitempty" yaml:"stdout_max,omitempty"`
}

// HumanSpec describes a human-kind step. OnTimeout is "approve" or
// "reject" and is applied once Timeout has elapsed.
type HumanSpec struct {
	Assignee  string `json:"assignee,omitempty" yaml:"assignee,omitempty"`
	Timeout   string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty" yaml:"on_timeout,omitempty"`
}

// RetrySpec bounds re-dispatch after a failed attempt. MaxAttempts counts
// the first attempt; Backoff is a duration string.
type RetrySpec struct {
	MaxAttempts int    `json:"max_attempts,omitempty" yaml:"max_attempts,omitempty"`
	Backoff     string `json:"backoff,omitempty" yaml:"backoff,omitempty"`
}

// Check names a contract that a step's output must satisfy before the step
// counts as done. It is the flow spec's word for the recipe `gate`.
type Check struct {
	Contract string `json:"contract" yaml:"contract"` // named contract
	EvaURL   string `json:"eva_url" yaml:"eva_url"`   // contract gateway base URL
}

// HumanTimeoutAction is the auto-action applied when a human step's
// timeout elapses.
type HumanTimeoutAction string

// Human timeout actions.
const (
	HumanTimeoutApprove     HumanTimeoutAction = "approve"
	HumanTimeoutReject      HumanTimeoutAction = "reject"
	HumanTimeoutKeepWaiting HumanTimeoutAction = "keep_waiting"
)

// ParseDuration parses a Go duration extended with day (d) and week (w)
// units, the forms flows use for timeouts, backoffs and due offsets:
// "30s", "48h", "5d", "2w".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unit := s[len(s)-1]
	if unit == 'd' || unit == 'w' {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		day := 24 * time.Hour
		if unit == 'w' {
			day *= 7
		}
		return time.Duration(n * float64(day)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}
