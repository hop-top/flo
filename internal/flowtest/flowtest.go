// Package flowtest holds what `flo flow test` and the activation gate share:
// the result of testing one flow version, where results are kept, the digest
// that binds a result to the exact version it tested, xrr cassettes at the
// exec and HTTP seams (the recording and the replay), and the `expect:` blocks
// of a bundle's tests/.
//
// The driver that runs a flow in a sandbox engine lives in the serve package
// (it needs the engine); this package has no dependency on it, so the gate
// can read results without a cycle.
package flowtest

import "time"

// SchemaVersion is the version of the stored Result document.
const SchemaVersion = "1"

// Mode is how a test got its external calls: from cassettes (replay) or from
// the real world, recording them (record).
type Mode string

// The test modes.
const (
	ModeReplay Mode = "replay"
	ModeRecord Mode = "record"
)

// Valid reports whether m is a mode.
func (m Mode) Valid() bool { return m == ModeReplay || m == ModeRecord }

// Problem kinds: what a failing test says went wrong.
const (
	// ProblemRun: the run did not succeed (it failed, parked, was cancelled).
	ProblemRun = "run"
	// ProblemStep: a step failed.
	ProblemStep = "step"
	// ProblemOutput: a step's output does not match its node spec's schema.
	ProblemOutput = "output"
	// ProblemCheck: a `check:` failed, or eva could not be reached.
	ProblemCheck = "check"
	// ProblemExpect: an `expect:` block did not hold.
	ProblemExpect = "expect"
	// ProblemCassette: a call had no recording to replay.
	ProblemCassette = "cassette"
)

// Problem is one reason a test failed.
type Problem struct {
	Kind    string `json:"kind" yaml:"kind"`
	Step    string `json:"step,omitempty" yaml:"step,omitempty"`
	Code    string `json:"code,omitempty" yaml:"code,omitempty"`
	Message string `json:"message" yaml:"message"`
}

// Step is one step record of the test run.
type Step struct {
	ID     string `json:"id" yaml:"id"`
	Index  *int   `json:"index,omitempty" yaml:"index,omitempty"`
	Kind   string `json:"kind" yaml:"kind"`
	Node   string `json:"node,omitempty" yaml:"node,omitempty"`
	Status string `json:"status" yaml:"status"`
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Planned is true when the step is a write the test did not execute: its
	// plan was recorded instead.
	Planned bool `json:"planned,omitempty" yaml:"planned,omitempty"`
	Plan    any  `json:"plan,omitempty" yaml:"plan,omitempty"`
	Output  any  `json:"output,omitempty" yaml:"output,omitempty"`
	Failure any  `json:"failure,omitempty" yaml:"failure,omitempty"`
}

// Result is the outcome of testing one flow version. It is bound to the
// version by Hash (the flow document) and Digest (the bundle around it):
// activation looks the result up by both.
type Result struct {
	Schema  string `json:"schema" yaml:"schema"`
	Flow    string `json:"flow" yaml:"flow"`
	Version string `json:"version" yaml:"version"`
	Hash    string `json:"hash" yaml:"hash"`
	// Digest fingerprints the bundle directory (empty for a single file).
	Digest string    `json:"digest,omitempty" yaml:"digest,omitempty"`
	Mode   Mode      `json:"mode" yaml:"mode"`
	Passed bool      `json:"passed" yaml:"passed"`
	At     time.Time `json:"at" yaml:"at"`

	Run      string    `json:"run,omitempty" yaml:"run,omitempty"`
	Status   string    `json:"status,omitempty" yaml:"status,omitempty"`
	Steps    []Step    `json:"steps" yaml:"steps"`
	Problems []Problem `json:"problems" yaml:"problems"`
	// Expectations is how many `expect:` entries held, out of Expected.
	Expected int `json:"expected" yaml:"expected"`
	Held     int `json:"held" yaml:"held"`
	// Calls is how many exec and HTTP calls the test made, and how many came
	// from a cassette.
	Calls    int `json:"calls" yaml:"calls"`
	Replayed int `json:"replayed" yaml:"replayed"`
	// Recorded names what a recording wrote, relative to the bundle.
	Recorded []string `json:"recorded,omitempty" yaml:"recorded,omitempty"`
}
