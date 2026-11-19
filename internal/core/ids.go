package core

import (
	"fmt"

	kitid "hop.top/kit/go/core/id"
)

// Typed identifiers. All are kit typeids: `<prefix>_<26 base32 chars>`, time
// ordered, so a listing sorted by id is a listing sorted by creation.
type (
	// RunID identifies a run.
	RunID string
	// StepRunID identifies one step record (or one element of a for_each step).
	StepRunID string
	// ApprovalID identifies an approval: the set of items one step parked on.
	ApprovalID string
	// ReceiptID identifies an admission receipt.
	ReceiptID string
	// OutputID identifies a published output (finding set or brief).
	OutputID string
	// TriggerID identifies one trigger registration of a flow.
	TriggerID string
	// JobID identifies a queued job.
	JobID string
)

// Id prefixes. `ap` is the prefix the CLI shows (`flo approval set ap_...`).
const (
	PrefixRun      = "run"
	PrefixStepRun  = "stp"
	PrefixApproval = "ap"
	PrefixReceipt  = "rcp"
	PrefixOutput   = "out"
	PrefixTrigger  = "trg"
	PrefixJob      = "job"
	PrefixDraft    = "drf"
)

// IDs mints identifiers. It is a port so tests get deterministic ones.
type IDs interface {
	// New returns a fresh id with the given prefix. It never repeats.
	New(prefix string) string
}

// SystemIDs mints kit typeids from the system's entropy and clock.
type SystemIDs struct{}

// New implements IDs.
func (SystemIDs) New(prefix string) string { return kitid.MustNew(prefix) }

// NewRunID mints a run id.
func NewRunID(ids IDs) RunID { return RunID(ids.New(PrefixRun)) }

// NewStepRunID mints a step record id.
func NewStepRunID(ids IDs) StepRunID { return StepRunID(ids.New(PrefixStepRun)) }

// NewApprovalID mints an approval id.
func NewApprovalID(ids IDs) ApprovalID { return ApprovalID(ids.New(PrefixApproval)) }

// NewReceiptID mints a receipt id.
func NewReceiptID(ids IDs) ReceiptID { return ReceiptID(ids.New(PrefixReceipt)) }

// NewOutputID mints an output id.
func NewOutputID(ids IDs) OutputID { return OutputID(ids.New(PrefixOutput)) }

// NewTriggerID mints a trigger id.
func NewTriggerID(ids IDs) TriggerID { return TriggerID(ids.New(PrefixTrigger)) }

// CheckID verifies that s is a typeid with the given prefix. It returns a
// usage *Error naming the expected prefix otherwise, for CLI arguments.
func CheckID(prefix, s string) error {
	p, err := kitid.Parse(s)
	if err != nil || p.Prefix != prefix {
		return Errorf(CodeUsage, "%q is not a %s id (want %s_...)", s, prefix, prefix)
	}
	return nil
}

// ParseRunID validates s as a run id.
func ParseRunID(s string) (RunID, error) {
	if err := CheckID(PrefixRun, s); err != nil {
		return "", err
	}
	return RunID(s), nil
}

// ParseApprovalID validates s as an approval id.
func ParseApprovalID(s string) (ApprovalID, error) {
	if err := CheckID(PrefixApproval, s); err != nil {
		return "", err
	}
	return ApprovalID(s), nil
}

// String returns the id as text.
func (id RunID) String() string { return string(id) }

// String returns the id as text.
func (id StepRunID) String() string { return string(id) }

// String returns the id as text.
func (id ApprovalID) String() string { return string(id) }

// String returns the id as text.
func (id ReceiptID) String() string { return string(id) }

// String returns the id as text.
func (id OutputID) String() string { return string(id) }

// String returns the id as text.
func (id TriggerID) String() string { return string(id) }

// String returns the id as text.
func (id JobID) String() string { return string(id) }

// WaitingForApproval is the `waiting_for` a run carries while parked on an
// approval, the event pattern dek's Job shape expects.
func WaitingForApproval(id ApprovalID) string { return fmt.Sprintf("flo.approval.%s", id) }
