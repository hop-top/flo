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

// EnvAgentToken is the variable bos reads the agent's credential from: a bearer
// token of the profile's bos agent principal, which also fixes who the agent
// acts for. It lives in the environment of the aps profile the call runs
// under, so flo never sets, reads or logs it, and passes no actor or
// acting-for of its own (bos refuses another principal than the token's).
const EnvAgentToken = "BOS_AGENT_TOKEN" //nolint:gosec // the name of a variable, not a credential

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
// profile's own environment carries its credential (BOS_AGENT_TOKEN).
type Gateway struct {
	run core.CommandRunner
	cfg Config
}

// NewGateway returns the gateway over run.
func NewGateway(run core.CommandRunner, cfg Config) *Gateway { return &Gateway{run: run, cfg: cfg} }

// Config returns the gateway's configuration.
func (g *Gateway) Config() Config { return g.cfg }

// call runs one bos verb as profile and returns its standard output.
func (g *Gateway) call(ctx context.Context, profile string, stdin []byte, argv ...string) ([]byte, error) {
	full := append([]string{g.cfg.bin()}, argv...)
	full = append(full, "--format", "json")
	if g.cfg.Scope != "" {
		full = append(full, "--scope", g.cfg.Scope)
	}
	spec := core.CommandSpec{Argv: full, Profile: profile, Stdin: stdin, Timeout: g.cfg.timeout()}
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

// proposeRow is the row `bos agent propose` prints (one in a list).
type proposeRow struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Digest  string `json:"request_digest"`
	Expires string `json:"expires_at"`
}

// Propose opens the proposal for the request, as the agent: idempotent by
// digest, so the same request returns the same proposal. Only its id and
// status are read here; the state flo decides on is the proposal-state
// document, read by digest.
func (g *Gateway) Propose(ctx context.Context, r Request) (ProposalState, error) {
	argv := []string{"agent", "propose", "--operation", r.Operation, "--input", "-", "--purpose", r.Purpose}
	out, err := g.call(ctx, r.Profile, r.Input, argv...)
	if err != nil {
		return ProposalState{}, err
	}
	rs, err := rows[proposeRow](out)
	if err != nil || len(rs) != 1 || rs[0].ID == "" || rs[0].Status == "" || rs[0].Digest == "" {
		return ProposalState{}, core.Errorf(core.CodeOutputInvalid, "bos agent propose: want one row with id, status and request_digest: %v", err)
	}
	return ProposalState{ID: rs[0].ID, Digest: rs[0].Digest, Status: rs[0].Status, ExpiresAt: rs[0].Expires}, nil
}

// ProposalByDigest reads the newest proposal for a request digest: the
// bos.flo/v1 proposal-state document, one object.
func (g *Gateway) ProposalByDigest(ctx context.Context, r Request) (ProposalState, error) {
	out, err := g.call(ctx, r.Profile, nil, "agent", "proposal", "show", "--digest", r.Digest)
	if err != nil {
		return ProposalState{}, err
	}
	var st ProposalState
	if err := json.Unmarshal(out, &st); err != nil {
		return ProposalState{}, core.Errorf(core.CodeOutputInvalid, "bos agent proposal show: want one proposal-state document: %w", err)
	}
	if st.ID == "" || st.Status == "" || st.Digest == "" {
		return ProposalState{}, core.New(core.CodeOutputInvalid, "bos agent proposal show: the document needs id, digest and status")
	}
	return st, nil
}

// rows decodes what a bos verb prints as a list.
func rows[T any](out []byte) ([]T, error) {
	var list []T
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("not a JSON list: %w", err)
	}
	return list, nil
}

// TrustLevels reads the agent's level in each class, as the agent. Classes bos
// does not know come back ask.
func (g *Gateway) TrustLevels(ctx context.Context, profile, agent string, classes []string) ([]core.TrustLevel, error) {
	argv := []string{"agent", "trust", "--agent", agent}
	for _, c := range classes {
		argv = append(argv, "--class", c)
	}
	out, err := g.call(ctx, profile, nil, argv...)
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

// Stage reads a subject's workflow instance (`bos workflow status`): the
// bos.flo/v1 stage document, one object, as the profile that reads it.
func (g *Gateway) Stage(ctx context.Context, profile, instance string) (Stage, error) {
	out, err := g.call(ctx, profile, nil, "workflow", "status", instance)
	if err != nil {
		return Stage{}, err
	}
	var st Stage
	if err := json.Unmarshal(out, &st); err != nil {
		return Stage{}, core.Errorf(core.CodeOutputInvalid, "bos workflow status: want one stage document: %w", err)
	}
	if st.InstanceID == "" || st.State == "" || st.StateVersion < 1 {
		return Stage{}, core.New(core.CodeOutputInvalid, "bos workflow status: the document needs instance_id, state and state_version")
	}
	return st, nil
}

// RequestTransition asks for the transition out of the instance's current
// state. It is the one write flo makes to a stage and never a decision: bos
// records the request and the people the definition names decide.
func (g *Gateway) RequestTransition(ctx context.Context, profile string, t TransitionRequest) (TransitionRecorded, error) {
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
	out, err := g.call(ctx, profile, nil, argv...)
	if err != nil {
		return TransitionRecorded{}, err
	}
	rs, err := rows[TransitionRecorded](out)
	if err != nil || len(rs) != 1 {
		return TransitionRecorded{}, core.Errorf(core.CodeOutputInvalid, "bos workflow request-transition: want one row: %v", err)
	}
	return rs[0], nil
}
