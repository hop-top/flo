// Package bos is flo's side of the bos port (bos.flo/v1): a gateway that runs
// the bos CLI as the profile that acts, an admission backend that opens
// proposals bound to the request digest, a trust provider that reads
// graduated autonomy, and the readers of access sets and stage state. flo
// never imports bos: what crosses is JSON on the CLI's standard streams,
// addresses, digests, levels and statuses.
package bos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// DefaultBin is the bos executable looked up on the PATH of the profile.
const DefaultBin = "bos"

// DefaultTimeout bounds one bos call.
const DefaultTimeout = 30 * time.Second

// EnvActor is the variable bos reads the asserted acting principal from. The
// credential that proves it is the profile's own (the aps profile the call
// runs under holds it in its environment); flo never takes one on a flag.
const EnvActor = "BOS_ACTOR"

// Codes of the bos.flo/v1 error vocabulary.
const (
	CodeProposalDigestUnknown = "proposal_digest_unknown"
	CodeProposalRefused       = "proposal_refused"
	CodeStateVersionMismatch  = "state_version_mismatch"
	CodeNoTransition          = "no_transition"
	CodeTransitionClosed      = "transition_closed"
	CodeSubjectChanged        = "subject_changed"
	CodeAgentUnauthenticated  = "agent_unauthenticated"
	CodeTrustCapped           = "trust_capped"
	CodeForbidden             = "forbidden"
	CodeUnavailable           = "unavailable"
)

var contractCodes = []string{
	CodeProposalDigestUnknown, CodeProposalRefused, CodeStateVersionMismatch, CodeNoTransition,
	CodeTransitionClosed, CodeSubjectChanged, CodeAgentUnauthenticated, CodeTrustCapped, CodeForbidden, CodeUnavailable,
}

// Config says how flo reaches bos and which bos identities its names map to.
type Config struct {
	// Bin is the bos executable; DefaultBin when empty.
	Bin string
	// Scope is the bos business scope id the flo tenant is bound to.
	Scope string
	// Principals maps a flo profile (an acting profile or a person) to its bos
	// principal id.
	Principals map[string]string
	// ActingFor is the principal id a run acts for when its starter has no
	// entry in Principals (a trigger starts runs as the engine's profile).
	ActingFor string
	// Timeout bounds one call; DefaultTimeout when zero.
	Timeout time.Duration
	// NarrowOwner stops the engine seeding the owner profile's wildcard: in a
	// business that runs bos the owner holds what bos provisions (a flo
	// setting, not a bos decision). A wildcard already granted is left for
	// the owner to unset.
	NarrowOwner bool
}

func (c Config) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return DefaultBin
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// Principal returns the bos principal id of a flo profile.
func (c Config) Principal(profile string) (string, bool) {
	id, ok := c.Principals[profile]
	return id, ok && id != ""
}

// Person returns the principal id of the person a run acts for: the starter's
// own, else the configured default.
func (c Config) Person(starter string) (string, bool) {
	if id, ok := c.Principal(starter); ok {
		return id, true
	}
	return c.ActingFor, c.ActingFor != ""
}

// Request is one request flo wants to run as an agent, rendered with bos ids.
type Request struct {
	Operation string          `json:"operation"`
	Scope     string          `json:"scope"`
	Actor     string          `json:"actor"`
	ActingFor string          `json:"acting_for"`
	Input     json.RawMessage `json:"input"`
	// Purpose names the flow, its version and the step.
	Purpose string `json:"purpose"`
	// Digest is the request digest flo computed.
	Digest string `json:"digest"`
	// Profile is the flo profile the call runs as.
	Profile string `json:"-"`
}

// ProposalState is what flo reads of a proposal (the proposal-state document).
type ProposalState struct {
	ID             string          `json:"id"`
	Digest         string          `json:"digest"`
	Status         string          `json:"status"`
	ApprovedDigest string          `json:"approved_digest,omitempty"`
	ExecutionKey   string          `json:"execution_key,omitempty"`
	Request        json.RawMessage `json:"request,omitempty"`
	ExpiresAt      string          `json:"expires_at,omitempty"`
}

// StageTransition is one transition out of the current state with its quorum.
type StageTransition struct {
	Action   string `json:"action"`
	To       string `json:"to"`
	Required int    `json:"required"`
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Status   string `json:"status"`
}

// Stage is the workflow instance of a subject as flo reads it.
type Stage struct {
	InstanceID   string            `json:"instance_id"`
	State        string            `json:"state"`
	StateVersion int64             `json:"state_version"`
	Transitions  []StageTransition `json:"transitions,omitempty"`
}

// TransitionRequest is a request for the transition out of a state.
type TransitionRequest struct {
	Instance     string
	Action       string
	Reason       string
	StateVersion int64
	// Key makes the request idempotent.
	Key string
}

// TransitionRecorded is bos's answer: the request is kept; nothing moved.
type TransitionRecorded struct {
	InstanceID   string `json:"instance_id"`
	Action       string `json:"action"`
	From         string `json:"from"`
	To           string `json:"to"`
	StateVersion int64  `json:"state_version"`
	Required     int    `json:"required"`
	Accepted     int    `json:"accepted"`
	Replayed     bool   `json:"replayed,omitempty"`
}

// Gateway runs the bos verbs flo needs, over CLI exec with JSON, as the
// profile that acts: the command runs under that profile (aps run), so the
// profile's own environment carries its credential.
type Gateway struct {
	run core.CommandRunner
	cfg Config
}

// NewGateway returns the gateway over run.
func NewGateway(run core.CommandRunner, cfg Config) *Gateway { return &Gateway{run: run, cfg: cfg} }

// Config returns the gateway's configuration.
func (g *Gateway) Config() Config { return g.cfg }

// call runs one bos verb as profile and returns its standard output.
func (g *Gateway) call(ctx context.Context, profile, actor, actingFor string, stdin []byte, argv ...string) ([]byte, error) {
	full := append([]string{g.cfg.bin()}, argv...)
	full = append(full, "--format", "json")
	if g.cfg.Scope != "" {
		full = append(full, "--scope", g.cfg.Scope)
	}
	if actingFor != "" {
		full = append(full, "--acting-for", actingFor)
	}
	spec := core.CommandSpec{Argv: full, Profile: profile, Stdin: stdin, Timeout: g.cfg.timeout()}
	if actor != "" {
		spec.Env = map[string]string{EnvActor: actor}
	}
	res, err := g.run.Run(ctx, spec)
	if err != nil {
		return nil, core.Errorf(core.CodeEngineUnreachable, "bos %s: %w", argv[0], err)
	}
	if res.TimedOut {
		return nil, core.Errorf(core.CodeTimeout, "bos %s timed out after %s", strings.Join(argv, " "), spec.Timeout)
	}
	if res.ExitCode != 0 {
		return nil, bosError(argv, res)
	}
	return res.Stdout, nil
}

// Error is a failure bos reported, with the code of the error vocabulary when
// it named one.
type Error struct {
	// Code is a code of bos.flo/v1, or empty.
	Code    string
	Message string
	Exit    int
}

func (e *Error) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

var codePrefix = regexp.MustCompile(`\b([a-z]+(?:_[a-z]+)+)\b`)

// bosError reads the failure of a bos call: a JSON error document on either
// stream (`{code, message}` or `{error: {code, message}}`), else a known code
// named in the text.
func bosError(argv []string, res core.CommandResult) error {
	e := &Error{Exit: res.ExitCode}
	for _, b := range [][]byte{res.Stdout, res.Stderr} {
		var doc struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Error   *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		if doc.Error != nil {
			doc.Code, doc.Message = doc.Error.Code, doc.Error.Message
		}
		if doc.Code != "" {
			e.Code, e.Message = doc.Code, doc.Message
			break
		}
	}
	if e.Code == "" {
		text := strings.TrimSpace(string(res.Stderr))
		for _, m := range codePrefix.FindAllString(text, -1) {
			if known(m) {
				e.Code = m
				break
			}
		}
		e.Message = text
	}
	if e.Message == "" {
		e.Message = "exited " + strconv.Itoa(res.ExitCode)
	}
	verb := strings.Join(argv[:min(len(argv), 2)], " ")
	mk := func(code core.Code, format string, args ...any) error {
		ce := core.Errorf(code, format, args...).WithDetail("bos_code", e.Code)
		ce.Cause = e
		return ce
	}
	switch e.Code {
	case CodeProposalDigestUnknown:
		return mk(core.CodeNotFound, "bos %s: %s", verb, e.Message)
	case CodeForbidden, CodeProposalRefused, CodeAgentUnauthenticated:
		return mk(core.CodeGrantDenied, "bos %s: %s: %s", verb, e.Code, e.Message)
	case CodeUnavailable:
		return mk(core.CodeEngineUnreachable, "bos %s: %s", verb, e.Message)
	case CodeStateVersionMismatch, CodeSubjectChanged, CodeTransitionClosed, CodeNoTransition:
		return mk(core.CodeRunStateConflict, "bos %s: %s: %s", verb, e.Code, e.Message)
	}
	return mk(core.CodeNodeReportedFailure, "bos %s exited %d: %s", verb, res.ExitCode, e.Message)
}

func known(code string) bool {
	for _, c := range contractCodes {
		if c == code {
			return true
		}
	}
	return false
}

// BosCode returns the bos.flo/v1 error code behind err, or "".
func BosCode(err error) string {
	var be *Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

// proposalRow is a row of `bos agent propose` and `bos agent proposal show`.
type proposalRow struct {
	ID             string          `json:"id"`
	Status         string          `json:"status"`
	Digest         string          `json:"request_digest"`
	ApprovedDigest string          `json:"approved_digest"`
	ExecutionKey   string          `json:"execution_key"`
	Request        json.RawMessage `json:"request"`
	Expires        string          `json:"expires_at"`
}

// Propose opens the proposal for the request, as the agent: idempotent by
// digest, so the same request returns the same proposal.
func (g *Gateway) Propose(ctx context.Context, r Request) (ProposalState, error) {
	argv := []string{"agent", "propose", "--operation", r.Operation, "--input", "-", "--purpose", r.Purpose}
	out, err := g.call(ctx, r.Profile, r.Actor, r.ActingFor, r.Input, argv...)
	if err != nil {
		return ProposalState{}, err
	}
	return readProposal(out, "")
}

// ProposalByDigest reads the newest proposal for a request digest.
func (g *Gateway) ProposalByDigest(ctx context.Context, r Request) (ProposalState, error) {
	out, err := g.call(ctx, r.Profile, r.Actor, r.ActingFor, nil, "agent", "proposal", "show", "--digest", r.Digest)
	if err != nil {
		return ProposalState{}, err
	}
	return readProposal(out, r.Digest)
}

// readProposal reads the one row a proposal verb prints and maps it to the
// proposal-state document. asked is the digest the proposal was looked up by,
// "" when it was just opened.
func readProposal(out []byte, asked string) (ProposalState, error) {
	rows, err := rows[proposalRow](out)
	if err != nil {
		return ProposalState{}, core.Errorf(core.CodeOutputInvalid, "bos proposal: %w", err)
	}
	if len(rows) != 1 {
		return ProposalState{}, core.Errorf(core.CodeOutputInvalid, "bos proposal: want one row, got %d", len(rows))
	}
	r := rows[0]
	st := ProposalState{ID: r.ID, Digest: r.Digest, Status: r.Status, ApprovedDigest: r.ApprovedDigest,
		ExecutionKey: r.ExecutionKey, Request: r.Request, ExpiresAt: r.Expires}
	if st.ID == "" || st.Status == "" || st.Digest == "" {
		return ProposalState{}, core.New(core.CodeOutputInvalid, "bos proposal: a row needs id, status and request_digest")
	}
	switch st.Status {
	case "approved":
		if st.ApprovedDigest == "" {
			st.ApprovedDigest = st.Digest // approved as asked
		}
	case "approved_corrected":
		// A correction changes the proposal's own digest, not the one flo asked
		// with: bos finds the proposal by the digest it was first made with.
		if asked != "" && st.Digest != asked {
			if st.ApprovedDigest == "" {
				st.ApprovedDigest = st.Digest
			}
			st.Digest = asked
		}
	}
	return st, nil
}

// rows decodes what a bos list verb prints: a JSON array, or a single object.
func rows[T any](out []byte) ([]T, error) {
	var list []T
	if err := json.Unmarshal(out, &list); err == nil {
		return list, nil
	}
	var one T
	if err := json.Unmarshal(out, &one); err != nil {
		return nil, fmt.Errorf("not JSON rows: %w", err)
	}
	return []T{one}, nil
}

// TrustLevels reads the agent's level in each class, as the agent. Classes bos
// does not know come back ask.
func (g *Gateway) TrustLevels(ctx context.Context, profile, agent string, classes []string) ([]core.TrustLevel, error) {
	argv := []string{"agent", "trust", "--agent", agent}
	for _, c := range classes {
		argv = append(argv, "--class", c)
	}
	out, err := g.call(ctx, profile, "", "", nil, argv...)
	if err != nil {
		return nil, err
	}
	var levels []struct {
		Class     string     `json:"class"`
		Level     string     `json:"level"`
		Streak    *int       `json:"streak"`
		Threshold *int       `json:"threshold"`
		Resets    *int       `json:"resets"`
		LastReset *time.Time `json:"last_reset"`
	}
	if err := json.Unmarshal(out, &levels); err != nil {
		return nil, core.Errorf(core.CodeOutputInvalid, "bos agent trust: want a JSON list of levels: %w", err)
	}
	res := make([]core.TrustLevel, 0, len(levels))
	for _, l := range levels {
		if l.Class == "" || l.Streak == nil || l.Threshold == nil || l.Resets == nil ||
			(l.Level != string(core.TrustAsk) && l.Level != string(core.TrustAuto)) {
			return nil, core.Errorf(core.CodeOutputInvalid, "bos agent trust: row of class %q is not {class, level ask|auto, streak, threshold, resets}", l.Class)
		}
		res = append(res, core.TrustLevel{Class: l.Class, Level: core.TrustLevelName(l.Level), Streak: *l.Streak,
			Threshold: *l.Threshold, Resets: *l.Resets, LastReset: l.LastReset})
	}
	return res, nil
}

type stageRow struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	StateVersion int64  `json:"state_version"`
	Quorum       string `json:"quorum"`
	// Transitions is read when bos prints them as a list (the contract's form).
	Transitions []StageTransition `json:"transitions"`
	InstanceID  string            `json:"instance_id"`
}

// Stage reads a subject's workflow instance (`bos workflow status`), as the
// profile that reads it.
func (g *Gateway) Stage(ctx context.Context, profile, actor, instance string) (Stage, error) {
	out, err := g.call(ctx, profile, actor, "", nil, "workflow", "status", instance)
	if err != nil {
		return Stage{}, err
	}
	rs, err := rows[stageRow](out)
	if err != nil || len(rs) != 1 {
		return Stage{}, core.Errorf(core.CodeOutputInvalid, "bos workflow status: want one instance row: %v", err)
	}
	r := rs[0]
	st := Stage{InstanceID: firstOf(r.InstanceID, r.ID), State: r.State, StateVersion: r.StateVersion, Transitions: r.Transitions}
	if st.InstanceID == "" || st.State == "" || st.StateVersion < 1 {
		return Stage{}, core.New(core.CodeOutputInvalid, "bos workflow status: a row needs id, state and state_version")
	}
	if len(st.Transitions) == 0 && r.Quorum != "" {
		st.Transitions = parseQuorum(r.Quorum)
	}
	return st, nil
}

// quorumPart is one "action: status accepted/required" of the quorum column.
var quorumPart = regexp.MustCompile(`^\s*([^:]+):\s*(\w+)\s+(\d+)/(\d+)\s*$`)

// parseQuorum reads the quorum column bos prints ("approve: pending 1/2; ..."),
// for a bos that does not list the transitions as JSON.
func parseQuorum(q string) []StageTransition {
	var out []StageTransition
	for _, part := range strings.Split(q, ";") {
		m := quorumPart.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		acc, _ := strconv.Atoi(m[3])
		req, _ := strconv.Atoi(m[4])
		out = append(out, StageTransition{Action: strings.TrimSpace(m[1]), Status: m[2], Accepted: acc, Required: req})
	}
	return out
}

// RequestTransition asks for the transition out of the instance's current
// state. It is the one write flo makes to a stage and never a decision: bos
// records the request and the people the definition names decide.
func (g *Gateway) RequestTransition(ctx context.Context, profile, actor string, t TransitionRequest) (TransitionRecorded, error) {
	argv := []string{"workflow", "request-transition", t.Instance, "--reason", t.Reason}
	if t.Action != "" {
		argv = append(argv, "--action", t.Action)
	}
	if t.StateVersion > 0 {
		argv = append(argv, "--state-version", strconv.FormatInt(t.StateVersion, 10))
	}
	if t.Key != "" {
		argv = append(argv, "--idempotency-key", t.Key)
	}
	out, err := g.call(ctx, profile, actor, "", nil, argv...)
	if err != nil {
		return TransitionRecorded{}, err
	}
	rs, err := rows[TransitionRecorded](out)
	if err != nil || len(rs) != 1 {
		return TransitionRecorded{}, core.Errorf(core.CodeOutputInvalid, "bos workflow request-transition: want one row: %v", err)
	}
	return rs[0], nil
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
