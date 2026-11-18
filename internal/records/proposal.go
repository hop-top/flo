package records

import (
	"time"

	"hop.top/flo/internal/core"
)

// ProposalLevel is the one level of the chain of every flo proposal: the
// owner decides (phase 1 has no multi-level chain).
const ProposalLevel = "owner"

// ProposalKind is dek's kind of an item: an `apply`-gated operation.
const ProposalKind = "apply"

// Proposal statuses and level statuses flo uses.
const (
	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
)

// ChainLevel is one level of a proposal's chain.
type ChainLevel struct {
	Level  string  `json:"level"`
	Status string  `json:"status"`
	By     *string `json:"by,omitempty"`
	At     *string `json:"at,omitempty"`
	Reason *string `json:"reason,omitempty"`
}

// Proposal is dek's Proposal (contract/schemas/Proposal.json): one approval
// item with its decision.
type Proposal struct {
	ID           string       `json:"id"`
	Version      uint64       `json:"version"`
	Kind         string       `json:"kind"`
	Title        string       `json:"title"`
	Capability   *string      `json:"capability,omitempty"`
	Subject      string       `json:"subject"`
	ProposedBy   string       `json:"proposed_by"`
	CreatedAt    string       `json:"created_at"`
	TargetLevel  string       `json:"target_level"`
	CurrentLevel *string      `json:"current_level,omitempty"`
	Chain        []ChainLevel `json:"chain"`
	Status       string       `json:"status"`
}

// ProposalID is the id of an item's proposal: `<approval>/<item>`.
func ProposalID(a core.ApprovalID, item string) string { return string(a) + "/" + item }

// ProposalOf projects item i of an approval onto a Proposal.
//
//   - id is `<approval>/<item id>`, version the approval's version;
//   - title is the item's action (else its capability, else its id);
//   - subject is the item's request digest when it has one, else its id;
//   - proposed_by is who proposed it: the run's profile (flo when empty);
//   - the chain is the single level "owner": pending until the item has a
//     decision; approved for approve, correct and auto; rejected for reject.
func ProposalOf(a core.Approval, i int, proposedBy string) Proposal {
	it := a.Items[i]
	if proposedBy == "" {
		proposedBy = "flo"
	}
	title := it.Action
	if title == "" {
		title = it.Capability
	}
	if title == "" {
		title = it.ID
	}
	subject := it.Digest
	if subject == "" {
		subject = it.ID
	}
	level := ChainLevel{Level: ProposalLevel, Status: ProposalPending}
	p := Proposal{
		ID: ProposalID(a.ID, it.ID), Version: a.Version, Kind: ProposalKind, Title: title,
		Capability: text(it.Capability), Subject: subject, ProposedBy: proposedBy,
		CreatedAt: Timestamp(a.CreatedAt), TargetLevel: ProposalLevel, Status: ProposalPending,
	}
	if d := it.Decision; d != nil {
		status := ProposalRejected
		if d.Verdict.Approving() {
			status = ProposalApproved
		}
		level.Status, p.Status = status, status
		level.By, level.At, level.Reason = text(d.By), stamp(&d.At), text(d.Reason)
	} else {
		cur := ProposalLevel
		p.CurrentLevel = &cur
	}
	p.Chain = []ChainLevel{level}
	return p
}

// ProposalsOf projects every item of an approval.
func ProposalsOf(a core.Approval, proposedBy string) []Proposal {
	out := make([]Proposal, len(a.Items))
	for i := range a.Items {
		out[i] = ProposalOf(a, i, proposedBy)
	}
	return out
}

// PlanRecord is dek's Plan (contract/schemas/Plan.json): a dry-run result.
type PlanRecord struct {
	ID             string `json:"id"`
	Capability     string `json:"capability"`
	Args           any    `json:"args"`
	Rendered       any    `json:"rendered"`
	IdempotencyKey string `json:"idempotency_key"`
	CreatedAt      string `json:"created_at"`
}

// PlanOf projects a dry-run Plan onto dek's Plan under id.
func PlanOf(id string, p core.Plan, at time.Time) PlanRecord {
	return PlanRecord{
		ID: id, Capability: p.Capability, Args: p.Args, Rendered: p.Rendered,
		IdempotencyKey: p.IdempotencyKey, CreatedAt: Timestamp(at),
	}
}

// PlanOfStep projects the dry-run plan of a step record; its id is the
// step record's. ok is false for a step that was not dry-run.
func PlanOfStep(s core.StepRun) (PlanRecord, bool) {
	if s.Plan == nil {
		return PlanRecord{}, false
	}
	return PlanOf(string(s.ID), *s.Plan, s.UpdatedAt), true
}
