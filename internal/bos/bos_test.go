package bos_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/flo/internal/bos"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/spec/node"
)

const (
	scope  = "business_scopes_1"
	agent  = "principals_1"
	person = "principals_2"
	// The canonical request of the admission below, written by hand: bos
	// digests the same bytes (autonomy.Request, and its independent twin
	// conformance.FloDigest).
	canonical = `{"acting_for":"principals_2","actor":"principals_1","input":{"args":["bdoc_1"],"flags":{"amount":"100.00"}},"operation":"billing.pay","scope":"business_scopes_1","v":1}`
)

var bg = context.Background()

func wantDigest() string {
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cfg() bos.Config {
	return bos.Config{Scope: scope, Principals: map[string]string{"noor": agent, "jad": person}}
}

// fake is a bos CLI that answers from recorded outputs and remembers what it
// was asked, in place of the real executable.
type fake struct {
	t  *testing.T
	fx *coretest.Fixture

	mu    sync.Mutex
	calls []core.CommandSpec
}

func newFake(t *testing.T) *fake {
	t.Helper()
	return &fake{t: t, fx: coretest.New()}
}

// reply answers `bos <verb...>` with a recorded file (its @DIGEST@ replaced) and an exit code.
func (f *fake) reply(prefix []string, file string, exit int) {
	f.t.Helper()
	raw, err := os.ReadFile("testdata/" + file) //nolint:gosec // a fixture of this package
	if err != nil {
		f.t.Fatal(err)
	}
	body := []byte(strings.ReplaceAll(string(raw), "@DIGEST@", wantDigest()))
	f.fx.Commands.On(append([]string{"bos"}, prefix...), func(spec core.CommandSpec) (core.CommandResult, error) {
		f.mu.Lock()
		f.calls = append(f.calls, spec)
		f.mu.Unlock()
		if exit != 0 {
			return core.CommandResult{ExitCode: exit, Stderr: body}, nil
		}
		return core.CommandResult{Stdout: body}, nil
	})
}

func (f *fake) last() core.CommandSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		f.t.Fatal("bos was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fake) gateway() *bos.Gateway { return bos.NewGateway(f.fx.Commands, cfg()) }

func (f *fake) admission() *bos.Admission {
	return bos.NewAdmission(bos.AdmissionDeps{
		Gateway: f.gateway(), Receipts: f.fx.DB.Receipts, Jobs: f.fx.Jobs, IDs: f.fx.IDs, Clock: f.fx.Clock, PollEvery: time.Minute,
	})
}

func request() core.AdmissionRequest {
	return core.AdmissionRequest{
		Tenant: core.DefaultTenant, Agent: "noor", Principal: "jad",
		Purpose: core.Purpose{Flow: "pay", Version: "1", StepID: "pay"},
		Run:     "run_1", StepRun: "run_1/pay", Operation: "billing/pay@1", Capabilities: []string{"billing.pay"},
		SideEffect: node.SideEffectWriteShared,
		Input:      map[string]any{"flags": map[string]any{"amount": "100.00"}, "args": []any{"bdoc_1"}},
	}
}

func hasArg(argv []string, want ...string) bool {
	for i := range argv {
		if i+len(want) <= len(argv) && strings.Join(argv[i:i+len(want)], "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}

// Flo renders the request with bos ids, digests it to the bytes bos does, and
// opens the proposal as the agent with the request on stdin, never on argv.
func TestProposeRendersBosIdsAndOpensAsTheAgent(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	adm := f.admission()
	d, err := adm.Admit(bg, request())
	if err != nil {
		t.Fatal(err)
	}
	if d.Result != core.AdmissionDeferred || d.Receipt.Proposal == "" || d.Receipt.Backend != bos.Backend {
		t.Fatalf("first answer = %+v", d)
	}
	if d.Receipt.Digest != wantDigest() {
		t.Fatalf("digest = %s, want %s", d.Receipt.Digest, wantDigest())
	}
	c := f.last()
	if c.Profile != "noor" {
		t.Errorf("ran as %q, want the acting profile", c.Profile)
	}
	// who the call is, and for whom, is the profile's credential (BOS_AGENT_TOKEN
	// in its own environment): flo sets neither an actor nor an acting-for
	if len(c.Env) != 0 {
		t.Errorf("flo set an environment: %v", c.Env)
	}
	if hasArg(c.Argv, "--acting-for") || hasArg(c.Argv, "--actor") {
		t.Errorf("flo names a principal on the command line: %v", c.Argv)
	}
	if string(c.Stdin) != `{"args":["bdoc_1"],"flags":{"amount":"100.00"}}` {
		t.Errorf("stdin = %s", c.Stdin)
	}
	for _, want := range [][]string{{"--operation", "billing.pay"}, {"--input", "-"}, {"--purpose", "flow pay@1 step pay"},
		{"--scope", scope}, {"--format", "json"}} {
		if !hasArg(c.Argv, want...) {
			t.Errorf("argv lacks %v: %v", want, c.Argv)
		}
	}
	for _, a := range c.Argv {
		if strings.Contains(a, "100.00") {
			t.Errorf("the input is on argv: %v", c.Argv)
		}
	}
}

// Asking again for the same request opens nothing new and wakes the run later.
func TestDeferredIsPolledByDigestAndWakesTheRun(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	f.reply([]string{"agent", "proposal", "show"}, "show-proposed.json", 0)
	adm := f.admission()
	first, err := adm.Admit(bg, request())
	if err != nil {
		t.Fatal(err)
	}
	again, err := adm.Admit(bg, request())
	if err != nil || again.Result != core.AdmissionDeferred || again.Receipt.ID != first.Receipt.ID {
		t.Fatalf("second ask = %+v %v", again, err)
	}
	if !hasArg(f.last().Argv, "proposal", "show", "--digest", wantDigest()) {
		t.Errorf("polled with %v", f.last().Argv)
	}
	jobs, err := f.fx.Jobs.List(bg, core.JobFilter{})
	if err != nil || len(jobs) == 0 {
		t.Fatalf("no wake-up queued: %v %v", jobs, err)
	}
	for _, j := range jobs {
		if j.Type != core.JobRunAdvance {
			t.Errorf("job %s", j.Type)
		}
	}
}

func scripted(t *testing.T, show string) (core.AdmissionDecision, *fake) {
	t.Helper()
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	f.reply([]string{"agent", "proposal", "show"}, show, 0)
	adm := f.admission()
	if _, err := adm.Admit(bg, request()); err != nil {
		t.Fatal(err)
	}
	d, err := adm.Admit(bg, request())
	if err != nil {
		t.Fatal(err)
	}
	return d, f
}

func TestApprovedAllowsWithProposalAndExecutionKey(t *testing.T) {
	d, _ := scripted(t, "show-approved.json")
	if d.Result != core.AdmissionAllow || d.Receipt.ExecutionKey != "exec_01h455vb4pex5vsknk084sn02q" ||
		d.Receipt.Proposal != "agent_proposals_01h455vb4pex5vsknk084sn02q" || d.Receipt.CorrectedInput != nil {
		t.Fatalf("decision = %+v", d)
	}
}

// A correction changes what runs: the approver's input, never the step's own.
func TestApprovedCorrectedAllowsTheCorrectedInput(t *testing.T) {
	d, _ := scripted(t, "show-approved-corrected.json")
	if d.Result != core.AdmissionAllow || d.Receipt.ExecutionKey == "" {
		t.Fatalf("decision = %+v", d)
	}
	flags, _ := d.Receipt.CorrectedInput["flags"].(map[string]any)
	if flags["amount"] != "90.00" {
		t.Fatalf("corrected input = %v", d.Receipt.CorrectedInput)
	}
}

func TestFinalStatusesDenyAndStaySettled(t *testing.T) {
	for _, status := range []string{"rejected", "expired", "withdrawn", "failed", "executed", "executing"} {
		t.Run(status, func(t *testing.T) {
			d, f := scripted(t, "show-"+status+".json")
			if d.Result != core.AdmissionDeny || d.Receipt.ExecutionKey != "" {
				t.Fatalf("decision = %+v", d)
			}
			if used := status == "executed" || status == "executing"; used != strings.Contains(d.Reason, "runs once") {
				t.Fatalf("reason = %q: a used proposal says it runs once, and no other does", d.Reason)
			}
			n := f.count()
			again, err := f.admission().Admit(bg, request())
			if err != nil || again.Result != core.AdmissionDeny || f.count() != n {
				t.Fatalf("a settled receipt asked bos again: %+v %v (calls %d -> %d)", again, err, n, f.count())
			}
		})
	}
}

// An answer for another digest is an error, never a decision.
func TestAnotherDigestIsNotAnApproval(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	// a document for another digest than the one asked is no approval
	other := "sha256:" + strings.Repeat("e", 64)
	f.fx.Commands.On([]string{"bos", "agent", "proposal", "show"}, func(core.CommandSpec) (core.CommandResult, error) {
		return core.CommandResult{Stdout: []byte(`{"id":"p","status":"approved","digest":"` + other + `","approved_digest":"` + other + `","execution_key":"k"}`)}, nil
	})
	adm := f.admission()
	if _, err := adm.Admit(bg, request()); err != nil {
		t.Fatal(err)
	}
	d, err := adm.Admit(bg, request())
	if err != nil {
		t.Fatal(err)
	}
	if d.Result != core.AdmissionDeny || !strings.Contains(d.Reason, "another request") {
		t.Fatalf("decision = %+v", d)
	}
}

// bos refusing the proposal is a denial with bos's reason; bos down keeps the
// step parked and never allows.
func TestRefusedDeniesAndOutageDefers(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "error-refused.json", 1)
	d, err := f.admission().Admit(bg, request())
	if err != nil || d.Result != core.AdmissionDeny || !strings.Contains(d.Reason, "may not run billing.pay") {
		t.Fatalf("refused = %+v %v", d, err)
	}

	g := newFake(t)
	g.fx.Commands.On([]string{"bos", "agent", "propose"}, func(core.CommandSpec) (core.CommandResult, error) {
		return core.CommandResult{}, errors.New("exec: bos: not found")
	})
	adm := g.admission()
	d, err = adm.Admit(bg, request())
	if err != nil || d.Result != core.AdmissionDeferred || d.Receipt.Proposal != "" {
		t.Fatalf("outage = %+v %v", d, err)
	}
	// bos is back: the same receipt opens the proposal and moves on
	g.fx.Commands = &coretest.Commands{}
	g2 := bos.NewAdmission(bos.AdmissionDeps{
		Gateway:  bos.NewGateway(g.fx.Commands, cfg()),
		Receipts: g.fx.DB.Receipts, Jobs: g.fx.Jobs, IDs: g.fx.IDs, Clock: g.fx.Clock,
	})
	g.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	d2, err := g2.Admit(bg, request())
	if err != nil || d2.Result != core.AdmissionDeferred || d2.Receipt.Proposal == "" || d2.Receipt.ID != d.Receipt.ID {
		t.Fatalf("after the outage = %+v %v", d2, err)
	}
}

// A profile bos does not know cannot ask; a read needs no proposal.
func TestUnmappedProfileIsDeniedAndReadNeedsNoProposal(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	req := request()
	req.Agent = "stranger"
	d, err := f.admission().Admit(bg, req)
	if err != nil || d.Result != core.AdmissionDeny || !strings.Contains(d.Reason, "no bos principal") || f.count() != 0 {
		t.Fatalf("unmapped = %+v %v (calls %d)", d, err, f.count())
	}
	read := request()
	read.SideEffect = node.SideEffectRead
	d, err = f.admission().Admit(bg, read)
	if err != nil || d.Result != core.AdmissionAllow || d.Receipt.Proposal != "" || f.count() != 0 {
		t.Fatalf("read = %+v %v (calls %d)", d, err, f.count())
	}
}

// A run started by the engine's profile acts for the configured person.
func TestActingForFallsBackToTheConfiguredPerson(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-proposed.json", 0)
	c := cfg()
	c.ActingFor = person
	adm := bos.NewAdmission(bos.AdmissionDeps{Gateway: bos.NewGateway(f.fx.Commands, c), Receipts: f.fx.DB.Receipts, IDs: f.fx.IDs, Clock: f.fx.Clock})
	req := request()
	req.Principal = "flo"
	d, err := adm.Admit(bg, req)
	if err != nil || d.Receipt.Digest != wantDigest() {
		t.Fatalf("decision = %+v %v", d, err)
	}
}

// Trust: read is `bos agent trust`, decide acknowledges every item and counts
// nothing, set is refused.
func TestTrustProvider(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "trust"}, "trust.json", 0)
	tr := bos.NewTrust(f.gateway(), nil)
	lv, err := tr.Levels(bg, core.DefaultTenant, "bos", map[string]any{"agent": "noor", "classes": []any{"draft_document", "customer_record"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(lv) != 2 || lv[0].Level != core.TrustAuto || lv[0].Streak != 31 || lv[1].Level != core.TrustAsk || lv[1].LastReset == nil {
		t.Fatalf("levels = %+v", lv)
	}
	c := f.last()
	if !hasArg(c.Argv, "agent", "trust", "--agent", agent, "--class", "draft_document", "--class", "customer_record") || c.Profile != "noor" {
		t.Errorf("call = %+v", c)
	}

	got, err := tr.Decide(bg, core.DefaultTenant, "bos", nil, []core.TrustDecision{{ID: "a"}, {ID: "b", Verdict: core.VerdictReject}})
	if err != nil || len(got.Applied) != 2 || len(got.Skipped) != 0 {
		t.Fatalf("decide = %+v %v", got, err)
	}
	if err := tr.Set(bg, core.DefaultTenant, "bos", "draft_document", core.TrustAuto, "because"); !core.IsCode(err, core.CodeTrustCapped) {
		t.Fatalf("set = %v", err)
	}
	if _, err := tr.Levels(bg, core.DefaultTenant, "bos", map[string]any{"classes": []any{"x"}}); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("a read with no agent = %v", err)
	}
	// another provider goes to the port behind it
	if _, err := tr.Levels(bg, core.DefaultTenant, "inbox", nil); !core.IsCode(err, core.CodeNotFound) {
		t.Fatalf("other provider = %v", err)
	}
}

func TestTrustRowsAreCheckedAgainstTheContract(t *testing.T) {
	for _, file := range []string{"trust-unknown-level.json", "trust-missing-counter.json"} {
		f := newFake(t)
		f.reply([]string{"agent", "trust"}, file, 0)
		_, err := f.gateway().TrustLevels(bg, "noor", agent, []string{"c"})
		if !core.IsCode(err, core.CodeOutputInvalid) {
			t.Errorf("%s: %v", file, err)
		}
	}
}

func TestStageAndTransitionRequest(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"workflow", "status"}, "status.json", 0)
	f.reply([]string{"workflow", "request-transition"}, "request.json", 0)
	st, err := f.gateway().Stage(bg, "noor", "workflow_instances_01h455vb4pex5vsknk084sn02q")
	if err != nil || st.State != "pending" || st.StateVersion != 1 || len(st.Transitions) != 1 ||
		st.Transitions[0].Action != "approve" || st.Transitions[0].Required != 2 || st.Transitions[0].Accepted != 1 {
		t.Fatalf("stage = %+v %v", st, err)
	}
	rec, err := f.gateway().RequestTransition(bg, "noor", bos.TransitionRequest{
		Instance: "workflow_instances_01h455vb4pex5vsknk084sn02q", Reason: "checks passed", StateVersion: st.StateVersion, Key: "rt-1"})
	if err != nil || rec.To != "approved" || rec.StateVersion != 1 {
		t.Fatalf("recorded = %+v %v", rec, err)
	}
	c := f.last()
	if !hasArg(c.Argv, "--state-version", "1") || !hasArg(c.Argv, "--reason", "checks passed") || !hasArg(c.Argv, "--idempotency-key", "rt-1") {
		t.Errorf("argv = %v", c.Argv)
	}
}

// A stale state version is bos's refusal, surfaced with its code.
func TestStaleStateVersionIsAConflictWithBosCode(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"workflow", "request-transition"}, "error-stale.json", 1)
	_, err := f.gateway().RequestTransition(bg, "noor", bos.TransitionRequest{Instance: "i", Reason: "r", StateVersion: 1})
	if !core.IsCode(err, core.CodeRunStateConflict) || bos.BosCode(err) != bos.CodeStateVersionMismatch {
		t.Fatalf("err = %v (bos code %q)", err, bos.BosCode(err))
	}
}

func TestDigestUnknownIsNotFound(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "proposal", "show"}, "error-digest-unknown.json", 1)
	_, err := f.gateway().ProposalByDigest(bg, bos.Request{Digest: wantDigest(), Profile: "noor", Actor: agent})
	if !core.IsCode(err, core.CodeNotFound) || bos.BosCode(err) != bos.CodeProposalDigestUnknown {
		t.Fatalf("err = %v", err)
	}
}

// The peer profile lists what flo expresses and the limit it enforces.
func TestProfileDeclaresOperationGrantsAndExpiry(t *testing.T) {
	p := bos.Profile("1.2.3", 36*time.Hour)
	if p.Tool != "flo" || p.Limits.MaxStalenessSeconds != 129600 {
		t.Fatalf("profile = %+v", p)
	}
	have := map[string]string{}
	for _, c := range p.Contracts {
		have[c.ID] = c.Level
	}
	if have[bos.ContractAccess] != "partial" || have[bos.ContractFlo] != "complete" || have[bos.ContractPeer] != "complete" {
		t.Fatalf("contracts = %v", have)
	}
	if len(p.Represents) != 2 || p.Represents[0] != "operation_grant" || p.Represents[1] != "expiry" {
		t.Fatalf("represents = %v", p.Represents)
	}
}

// Opening a request bos already decided answers with its proposal: flo then
// reads the state document, which carries the approved digest and the key.
func TestOpeningADecidedRequestReadsTheStateDocument(t *testing.T) {
	f := newFake(t)
	f.reply([]string{"agent", "propose"}, "propose-approved.json", 0)
	f.reply([]string{"agent", "proposal", "show"}, "show-approved.json", 0)
	d, err := f.admission().Admit(bg, request())
	if err != nil || d.Result != core.AdmissionAllow || d.Receipt.ExecutionKey == "" {
		t.Fatalf("decision = %+v %v", d, err)
	}
}

// bos prints one document, not a list, for the state flo reads.
func TestProposalAndStageMustBeOneDocument(t *testing.T) {
	f := newFake(t)
	f.fx.Commands.On([]string{"bos", "agent", "proposal", "show"}, func(core.CommandSpec) (core.CommandResult, error) {
		return core.CommandResult{Stdout: []byte(`[{"id":"p","status":"proposed","digest":"x"}]`)}, nil
	})
	f.fx.Commands.On([]string{"bos", "workflow", "status"}, func(core.CommandSpec) (core.CommandResult, error) {
		return core.CommandResult{Stdout: []byte(`[{"id":"i","state":"s","state_version":1}]`)}, nil
	})
	if _, err := f.gateway().ProposalByDigest(bg, bos.Request{Digest: wantDigest(), Profile: "noor"}); !core.IsCode(err, core.CodeOutputInvalid) {
		t.Errorf("a list for a proposal = %v", err)
	}
	if _, err := f.gateway().Stage(bg, "noor", "i"); !core.IsCode(err, core.CodeOutputInvalid) {
		t.Errorf("a list for a stage = %v", err)
	}
}
