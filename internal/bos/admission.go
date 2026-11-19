package bos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"hop.top/flo/internal/access/digest"
	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// Backend is the name a receipt of this admission carries.
const Backend = "bos"

// DefaultPollEvery is how often a run parked on an open proposal looks at it
// again.
const DefaultPollEvery = 30 * time.Second

// Proposal statuses a peer reads, as bos keeps them.
const (
	statusProposed          = "proposed"
	statusApproved          = "approved"
	statusApprovedCorrected = "approved_corrected"
	statusExecuting         = "executing"
	statusExecuted          = "executed"
)

// Admission is the core.Admission that defers to bos for every step with a
// side effect above read (docs/adapters/flo.md, "Proposals bound to the
// request digest"): it renders the request with bos ids, opens the proposal
// (idempotent by the request digest), and maps what bos answers:
//
//	proposed            deferred; the run parks and asks again
//	approved            allow, with the proposal and the execution key
//	approved_corrected  allow the approver's input, never the step's own
//	executing, executed deny: the proposal runs once
//	anything else       deny (rejected, expired, withdrawn, failed, reversed)
//
// bos down keeps the step parked; flo never allows without an approval.
type Admission struct {
	gw       *Gateway
	receipts core.ReceiptStore
	jobs     core.Jobs
	ids      core.IDs
	clock    core.Clock
	every    time.Duration
}

var _ core.Admission = (*Admission)(nil)

// AdmissionDeps are what the backend is built from. Jobs may be nil, in which
// case nothing wakes a parked run and a caller polls by asking again.
type AdmissionDeps struct {
	Gateway  *Gateway
	Receipts core.ReceiptStore
	Jobs     core.Jobs
	IDs      core.IDs
	Clock    core.Clock
	// PollEvery spaces the looks at an open proposal; DefaultPollEvery if zero.
	PollEvery time.Duration
}

// NewAdmission returns the bos admission backend.
func NewAdmission(d AdmissionDeps) *Admission {
	every := d.PollEvery
	if every <= 0 {
		every = DefaultPollEvery
	}
	return &Admission{gw: d.Gateway, receipts: d.Receipts, jobs: d.Jobs, ids: d.IDs, clock: d.Clock, every: every}
}

// Render renders the request of an admission request with bos identifiers and
// computes its digest: operation is the node's operation name, scope the bos
// business scope id, actor the bos principal id of the run_as profile,
// acting_for the principal id of the person, input the rendered input.
// A capability is an operation name (`<namespace>.<Operation>`, not renamed),
// so a node that declares exactly one is run as that operation.
func (a *Admission) Render(req core.AdmissionRequest) (Request, error) {
	cfg := a.gw.Config()
	actor, ok := cfg.Principal(req.Agent)
	if !ok {
		return Request{}, core.Errorf(core.CodeAdmissionDenied, "profile %q has no bos principal", req.Agent).
			WithSuggestion("map it in the config key bos.principals")
	}
	person, ok := cfg.Person(req.Principal)
	if !ok {
		return Request{}, core.Errorf(core.CodeAdmissionDenied, "%q has no bos principal to act for", req.Principal).
			WithSuggestion("map it in bos.principals, or set bos.acting_for")
	}
	if cfg.Scope == "" {
		return Request{}, core.New(core.CodeAdmissionDenied, "no bos business scope is configured").
			WithSuggestion("set bos.scope")
	}
	capability := ""
	if len(req.Capabilities) == 1 {
		capability = req.Capabilities[0]
	}
	in, err := digest.Input(req.Input)
	if err != nil {
		return Request{}, err //nolint:wrapcheck // *core.Error
	}
	r := Request{
		Operation: digest.Operation(capability, req.Operation), Scope: cfg.Scope, Actor: actor, ActingFor: person, Input: in,
		Purpose: fmt.Sprintf("flow %s@%s step %s", req.Purpose.Flow, req.Purpose.Version, req.Purpose.StepID),
		Profile: req.Agent,
	}
	d, err := digest.Request{Operation: r.Operation, Scope: r.Scope, Actor: r.Actor, ActingFor: r.ActingFor, Input: in}.Digest()
	if err != nil {
		return Request{}, core.Wrap(core.CodeInternal, err)
	}
	r.Digest = d
	return r, nil
}

// Admit implements core.Admission. Asking again for the same (run, step run,
// digest) returns the receipt's decision; a deferred one is looked up in bos
// again and the receipt updated when bos has decided.
func (a *Admission) Admit(ctx context.Context, req core.AdmissionRequest) (core.AdmissionDecision, error) {
	if err := core.RequireTenant(req.Tenant); err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // *core.Error
	}
	if req.SideEffect.Rank() <= node.SideEffectRead.Rank() {
		return a.record(ctx, req, receiptFor(req, "", core.AdmissionAllow, "read: no proposal needed"), false)
	}
	r, err := a.Render(req)
	if err != nil {
		if core.IsCode(err, core.CodeAdmissionDenied) {
			return a.record(ctx, req, receiptFor(req, "", core.AdmissionDeny, errMessage(err)), false)
		}
		return core.AdmissionDecision{}, err
	}
	existing, err := a.receipts.ListReceipts(ctx, req.Tenant, req.Run)
	if err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	var have *core.Receipt
	for i := range existing {
		if existing[i].StepRun == req.StepRun && existing[i].Digest == r.Digest {
			have = &existing[i]
		}
	}
	if have != nil && have.Decision != core.AdmissionDeferred {
		return core.AdmissionDecision{Result: have.Decision, Receipt: *have, Reason: have.Reason}, nil
	}
	st, err := a.ask(ctx, r, have)
	if err != nil {
		switch core.CodeOf(err) {
		case core.CodeEngineUnreachable, core.CodeTimeout:
			// bos is out of reach: the step stays parked, never allowed.
			return a.settle(ctx, req, have, receiptFor(req, r.Digest, core.AdmissionDeferred, "bos unavailable: "+errMessage(err)))
		case core.CodeGrantDenied:
			// proposal_refused and its kin: a denial with bos's reason, never a failure.
			return a.settle(ctx, req, have, receiptFor(req, r.Digest, core.AdmissionDeny, errMessage(err)))
		}
		return core.AdmissionDecision{}, err
	}
	res, reason, corrected := decide(r, st)
	rc := receiptFor(req, r.Digest, res, reason)
	rc.Proposal = st.ID
	if res == core.AdmissionAllow {
		rc.ExecutionKey = st.ExecutionKey
		rc.CorrectedInput = corrected
	}
	return a.settle(ctx, req, have, rc)
}

// ask opens the proposal when there is none yet, else reads it by digest.
func (a *Admission) ask(ctx context.Context, r Request, have *core.Receipt) (ProposalState, error) {
	if have == nil || have.Proposal == "" {
		st, err := a.gw.Propose(ctx, r)
		if err != nil || st.Status == statusProposed {
			return st, err
		}
		// opening an already decided request answers with its proposal: read
		// the state document, which carries the approved digest and the key
	}
	return a.gw.ProposalByDigest(ctx, r)
}

func (a *Admission) record(ctx context.Context, req core.AdmissionRequest, rc core.Receipt, wake bool) (core.AdmissionDecision, error) {
	existing, err := a.receipts.ListReceipts(ctx, req.Tenant, req.Run)
	if err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	for _, e := range existing {
		if e.StepRun == req.StepRun && e.Digest == rc.Digest && e.Decision == rc.Decision {
			return core.AdmissionDecision{Result: e.Decision, Receipt: e, Reason: e.Reason}, nil
		}
	}
	rc.ID, rc.At = core.NewReceiptID(a.ids), a.clock.Now()
	if err := a.receipts.PutReceipt(ctx, rc); err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	if wake {
		if err := a.wake(ctx, rc); err != nil {
			return core.AdmissionDecision{}, err
		}
	}
	return core.AdmissionDecision{Result: rc.Decision, Receipt: rc, Reason: rc.Reason}, nil
}

// settle stores the receipt (replacing the deferred one it follows) and, for a
// deferred answer, queues the look that wakes the parked run.
func (a *Admission) settle(ctx context.Context, req core.AdmissionRequest, have *core.Receipt, rc core.Receipt) (core.AdmissionDecision, error) {
	if have != nil {
		rc.ID = have.ID
		if rc.Proposal == "" {
			rc.Proposal = have.Proposal
		}
		rc.At = have.At
	} else {
		rc.ID, rc.At = core.NewReceiptID(a.ids), a.clock.Now()
	}
	if err := a.receipts.PutReceipt(ctx, rc); err != nil {
		return core.AdmissionDecision{}, err //nolint:wrapcheck // port error passes through
	}
	if rc.Decision == core.AdmissionDeferred {
		if err := a.wake(ctx, rc); err != nil {
			return core.AdmissionDecision{}, err
		}
	}
	return core.AdmissionDecision{Result: rc.Decision, Receipt: rc, Reason: rc.Reason}, nil
}

// wake queues the advance of the parked run one period from now, once per
// period and receipt: the run then asks again.
func (a *Admission) wake(ctx context.Context, rc core.Receipt) error {
	if a.jobs == nil {
		return nil
	}
	next := a.clock.Now().Truncate(a.every).Add(a.every)
	_, err := a.jobs.Enqueue(ctx, core.JobSpec{
		Type: core.JobRunAdvance, Tenant: rc.Tenant, Key: "admission:" + string(rc.ID) + "@" + strconv.FormatInt(next.Unix(), 10),
		Payload: core.RunAdvancePayload{Tenant: rc.Tenant, Run: rc.Run}, At: &next,
	})
	return err //nolint:wrapcheck // port error passes through
}

func receiptFor(req core.AdmissionRequest, d string, res core.AdmissionResult, reason string) core.Receipt {
	return core.Receipt{
		Tenant: req.Tenant, Run: req.Run, StepRun: req.StepRun, StepID: req.Purpose.StepID, Digest: d, Decision: res,
		Agent: req.Agent, Principal: req.Principal, Operation: req.Operation, Capabilities: req.Capabilities,
		SideEffect: req.SideEffect, Backend: Backend, Reason: reason,
	}
}

func errMessage(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Message
	}
	return err.Error()
}

// decide maps the proposal bos answers to a decision (the algorithm of the
// reference admission in bos, peer.FloAdmit).
func decide(r Request, st ProposalState) (res core.AdmissionResult, reason string, corrected map[string]any) {
	if st.Digest != r.Digest {
		return core.AdmissionDeny, "bos answered for another request than the one asked about", nil
	}
	switch st.Status {
	case statusProposed:
		return core.AdmissionDeferred, "", nil
	case statusApproved:
		if st.ApprovedDigest != r.Digest || st.ExecutionKey == "" {
			return core.AdmissionDeny, "the approval does not bind this request", nil
		}
		return core.AdmissionAllow, "", nil
	case statusApprovedCorrected:
		if st.ExecutionKey == "" || len(st.Request) == 0 {
			return core.AdmissionDeny, "the correction carries no request", nil
		}
		in := correctedInput(st.Request)
		if in == nil {
			return core.AdmissionDeny, "the correction carries no input the step can run", nil
		}
		return core.AdmissionAllow, "approved with a correction", in
	case statusExecuting, statusExecuted:
		return core.AdmissionDeny, "the proposal is used: it runs once", nil
	default: // rejected, expired, withdrawn, failed, reversed, and any status not known
		return core.AdmissionDeny, "proposal " + st.Status, nil
	}
}

// correctedInput is the input of a corrected request: the `input` of the
// request as bos keeps it, normalized as a step's input is. Nil when there is
// none the step can run.
func correctedInput(raw json.RawMessage) map[string]any {
	var rq struct {
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &rq) != nil || len(rq.Input) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(rq.Input))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil || v == nil {
		return nil
	}
	norm, err := core.NormalizeJSON(v)
	if err != nil {
		return nil
	}
	in, _ := norm.(map[string]any)
	return in
}
