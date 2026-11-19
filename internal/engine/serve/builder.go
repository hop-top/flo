package serve

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"hop.top/flo/internal/builder"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/prompts"
	"hop.top/flo/internal/runrec"
	"hop.top/flo/spec/cel"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

// DefaultBuilderModel is the model of the chat builder when neither the
// request nor `ai.builder.model` names one: local, through ollama.
const DefaultBuilderModel = "ollama://gemma4:e4b"

// builderPrompt is the shape the model client takes a prompt in: the builder
// builds its own text, so the template only passes the user message through.
// The system text of a prompt is sent as written, not rendered, so each call
// sets it.
var builderPrompt = mustPrompt()

func mustPrompt() *prompts.Prompt {
	doc, err := yaml.Marshal(map[string]string{"template": "{{ .user }}"})
	if err != nil {
		panic(err)
	}
	p, err := prompts.Parse(prompts.Ref{Name: "builder", Version: 1}, doc)
	if err != nil {
		panic(err)
	}
	return p
}

// DraftRequest asks the model for the next version of a flow.
type DraftRequest struct {
	Flow        string
	Instruction string
	// Model is the model URI; empty is Config.BuilderModel, then
	// DefaultBuilderModel.
	Model string
	// Caller is the profile that asks: the secrets of a cloud model are read
	// as it, and it is recorded on the draft.
	Caller string
}

// modelProposer is the model call of one build: flo's llm backend (the
// allow and deny lists, the privacy ceiling, the keys of the secret store, the
// engine's HTTP seam) under the ceiling of the flow being changed.
type modelProposer struct {
	gen       builtin.Generator
	model     string
	level     node.Privacy
	localOnly bool
	tenant    core.TenantID
	profile   string
}

func (m modelProposer) Propose(ctx context.Context, r builder.Request) (builder.Reply, error) {
	p := *builderPrompt
	p.System = r.System
	got, cost, err := m.gen.Generate(ctx, builtin.GenerateRequest{
		Prompt: &p, Context: map[string]any{"user": r.User},
		Model: m.model, LocalOnly: m.localOnly, Level: m.level, Tenant: m.tenant, Profile: m.profile,
	})
	if err != nil {
		return builder.Reply{Cost: cost}, err //nolint:wrapcheck // the backend's classified error
	}
	return builder.Reply{Text: got.Text, Model: got.Model, Cost: cost}, nil
}

// baseOf is the version a draft changes: the one `flo flow show` shows (the
// active version, else the newest) and the newest stored one.
func baseOf(vs []core.FlowVersion) (base core.FlowVersion, head string) {
	base = vs[0]
	for _, v := range vs {
		if v.State == core.FlowActive {
			base = v
			break
		}
	}
	return base, vs[0].Hash
}

// CreateDraft asks the model for the full next version of a flow, validates it,
// feeds the errors back (at most builder.MaxFixes times), replays the
// valid proposal on the flow's last recording and keeps it as a draft. Nothing
// is stored as a flow version: that is ApplyDraft.
func (e *Engine) CreateDraft(ctx context.Context, tenant core.TenantID, req DraftRequest) (builder.Draft, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // *core.Error
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return builder.Draft{}, core.New(core.CodeUsage, "the instruction is empty").
			WithSuggestion(`flo flow draft create <flow> "what to change"`)
	}
	vs, err := e.Services.Flows.ListVersions(ctx, tenant, req.Flow)
	if err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // *core.Error
	}
	if len(vs) == 0 {
		return builder.Draft{}, core.Errorf(core.CodeFlowNotFound, "flow %q not found", req.Flow).WithSuggestion("flo flow list")
	}
	base, head := baseOf(vs)
	f, err := cel.ParseFlow(bytes.NewReader(base.Source), base.Name+".yaml")
	if err != nil {
		return builder.Draft{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return builder.Draft{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	model := firstNonEmpty(req.Model, e.Config.BuilderModel, DefaultBuilderModel)
	prop, err := e.proposer(tenant, req.Caller, model, set.Privacy)
	if err != nil {
		return builder.Draft{}, err
	}
	loc, err := e.Nodes.Locator(base.Bundle)
	if err != nil {
		return builder.Draft{}, err
	}

	var last Checked
	validate := func(ctx context.Context, doc []byte) (builder.Check, error) {
		ck, err := e.Flows.Check(ctx, tenant, Source{File: base.Name + ".yaml", Bundle: base.Bundle, Data: doc})
		if core.IsCode(err, core.CodeInvalidFlow) { // a document that does not parse
			var ce *core.Error
			errors.As(err, &ce)
			return builder.Check{Errors: []builder.Finding{{Code: string(core.CodeInvalidFlow), Message: ce.Message}}}, nil
		}
		if err != nil {
			return builder.Check{}, err
		}
		last = ck
		out := builder.Check{Name: ck.Flow.Name, Version: ck.Flow.Version}
		for _, i := range ck.Report.Issues {
			fd := builder.Finding{Rule: i.Rule, Code: i.Code, Step: i.Step, Field: i.Field, Message: i.Message}
			if i.Severity == "error" {
				out.Errors = append(out.Errors, fd)
			} else {
				out.Warnings = append(out.Warnings, fd)
			}
		}
		return out, nil
	}
	res, err := builder.Run(ctx, builder.LoopInput{
		Flow: base.Name, Version: base.Version, Base: base.Source, Instruction: req.Instruction,
		Catalog: catalogOf(loc), Model: prop, Validate: validate,
	})
	if err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // the loop's and the model's classified errors
	}
	d := builder.Draft{
		ID: e.Services.IDs.New(core.PrefixDraft), Flow: base.Name, Instruction: req.Instruction,
		Base: builder.Ref{Version: base.Version, Hash: base.Hash}, Head: head,
		Version: last.Flow.Version, Hash: last.Flow.Hash, Source: string(res.Doc), Bundle: base.Bundle,
		Model: res.Model, Attempts: res.Attempts, Warnings: orNone(res.Warnings), Cost: res.Cost,
		CreatedAt: e.Config.Clock.Now().UTC(), CreatedBy: req.Caller,
	}
	if d.DryRun, err = e.dryRunDraft(ctx, tenant, req.Caller, d); err != nil {
		return builder.Draft{}, err
	}
	if err := e.DraftStore.Save(tenant, d); err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // *core.Error
	}
	return d, nil
}

func orNone(f []builder.Finding) []builder.Finding {
	if f == nil {
		return []builder.Finding{}
	}
	return f
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// proposer builds the model call under the privacy ceiling of the flow being
// changed: the document goes to the model, so a local-only flow's goes to a
// local model only. The check is the generate node's, made before any call.
func (e *Engine) proposer(tenant core.TenantID, caller, model string, ceiling specrun.Privacy) (builder.Proposer, error) {
	localOnly := ceiling != specrun.PrivacyCloudAllowed
	level, perr := node.ModelPrivacy(model, e.localHosts)
	switch {
	case errors.Is(perr, node.ErrNotRunnable):
		return nil, core.Errorf(core.CodePrivacyViolation, "model %q: %v", model, perr).
			WithSuggestion("write ollama://<model> for the local daemon or scheme://host:port/<model>")
	case perr != nil && !localOnly:
		return nil, core.Errorf(core.CodeInputInvalid, "model %q: %v", model, perr).
			WithSuggestion("write the model as scheme://<model>, e.g. ollama://gemma4:e4b")
	case localOnly && (perr != nil || level == node.PrivacyCloud):
		return nil, core.Errorf(core.CodePrivacyViolation, "model %q is not local: the flow is local-only and its document goes to the model", model).
			WithSuggestion("name a local model (ollama://<model>, a local scheme://host:port/<model>), list the host in privacy.local_hosts, or set privacy: cloud-allowed on the flow")
	}
	return modelProposer{gen: e.builderGen, model: model, level: level, localOnly: localOnly, tenant: tenant, profile: caller}, nil
}

// catalogOf lists the node types a flow may use, one per line: the type, what
// it does and its inputs (required ones starred).
func catalogOf(loc *node.Locator) string {
	var lines []string
	for _, en := range loc.List() {
		s := en.Spec
		var in []string
		for name, def := range s.Inputs {
			if def.Required.Flag {
				name += "*"
			}
			in = append(in, name)
		}
		sort.Strings(in)
		line := "- " + s.Node
		if d := strings.Join(strings.Fields(s.Description), " "); d != "" {
			line += ": " + d
		}
		if len(in) > 0 {
			line += " (inputs: " + strings.Join(in, ", ") + ")"
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// GetDraft returns a stored draft.
func (e *Engine) GetDraft(_ context.Context, tenant core.TenantID, id string) (builder.Draft, error) {
	return e.DraftStore.Load(tenant, id)
}

// ListDrafts lists the stored drafts of flow (all when empty), newest first.
func (e *Engine) ListDrafts(_ context.Context, tenant core.TenantID, flow string) ([]builder.Draft, error) {
	return e.DraftStore.List(tenant, flow)
}

// DiscardDraft deletes a draft.
func (e *Engine) DiscardDraft(_ context.Context, tenant core.TenantID, id string) error {
	return e.DraftStore.Delete(tenant, id)
}

// CheckDraft returns the draft when its base has not moved since it was made,
// else draft_stale: the flow's base version or its newest version is another
// one now. The draft is kept either way.
func (e *Engine) CheckDraft(ctx context.Context, tenant core.TenantID, id string) (builder.Draft, error) {
	d, err := e.DraftStore.Load(tenant, id)
	if err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // *core.Error
	}
	vs, err := e.Services.Flows.ListVersions(ctx, tenant, d.Flow)
	if err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // *core.Error
	}
	if len(vs) == 0 {
		return builder.Draft{}, core.Errorf(core.CodeFlowNotFound, "flow %q not found", d.Flow)
	}
	base, head := baseOf(vs)
	if base.Hash != d.Base.Hash || head != d.Head {
		return builder.Draft{}, core.Errorf(core.CodeDraftStale, "draft %s was made on %s@%s, and flow %s is at %s@%s now",
			d.ID, d.Flow, short(d.Base.Hash), d.Flow, base.Version, short(base.Hash)).
			WithDetail("draft_base", d.Base.Hash).WithDetail("base", base.Hash).WithDetail("newest", head).
			WithSuggestion("make a new draft on the current flow: flo flow draft create " + d.Flow + " <instruction>")
	}
	return d, nil
}

// DraftApplied is what ApplyDraft did.
type DraftApplied struct {
	Draft builder.Draft
	// Applied is the stored version, as `flo flow apply` reports it.
	Applied ApplyResult
	// DryRun is the proposal replayed on the last recording again, now.
	DryRun *builder.DryRun
}

// ApplyDraft stores the draft's document as the flow's next version, only when
// the base has not moved (draft_stale otherwise). The document is validated
// again and the caller's grants checked, exactly as `flo flow apply` does, and
// the proposal is replayed on the flow's last recording once more so the
// outcome is read at the moment of the apply. The version is a draft version:
// test it and activate it as any other. The draft is spent.
func (e *Engine) ApplyDraft(ctx context.Context, tenant core.TenantID, caller, id string) (DraftApplied, error) {
	d, err := e.CheckDraft(ctx, tenant, id)
	if err != nil {
		return DraftApplied{}, err
	}
	dry, err := e.dryRunDraft(ctx, tenant, caller, d)
	if err != nil {
		return DraftApplied{}, err
	}
	res, err := e.Flows.Apply(ctx, tenant, caller, Source{File: d.Flow + ".yaml", Bundle: d.Bundle, Data: []byte(d.Source)})
	if err != nil {
		return DraftApplied{Draft: d, DryRun: dry}, err
	}
	if err := e.DraftStore.Delete(tenant, d.ID); err != nil && !core.IsCode(err, core.CodeDraftNotFound) {
		return DraftApplied{}, err //nolint:wrapcheck // *core.Error
	}
	return DraftApplied{Draft: d, Applied: res, DryRun: dry}, nil
}

// dryRunDraft replays the draft's document on the flow's last recording, in
// the sandbox, offline: what would differ in the outcome of that run. A flow
// with no usable recording, or a recording that cannot be replayed for this
// caller, is a DryRun that did not run, with the reason; only a cancelled
// context is an error.
func (e *Engine) dryRunDraft(ctx context.Context, tenant core.TenantID, caller string, d builder.Draft) (*builder.DryRun, error) {
	run, meta, found, err := e.lastRecording(ctx, tenant, d.Flow)
	if err != nil {
		return nil, err
	}
	if !found {
		return &builder.DryRun{Reason: "the flow has no recording to replay on: record a run with `flo run create --flow " + d.Flow + " --record`",
			Diffs: []string{}, Problems: []string{}}, nil
	}
	v := core.FlowVersion{Tenant: tenant, Name: d.Flow, Version: d.Version, Hash: d.Hash, Source: []byte(d.Source), Bundle: d.Bundle}
	rep, err := e.replayOn(ctx, tenant, ReplayRequest{Run: run.ID, Caller: caller}, run, meta, v)
	var ce *core.Error
	if err != nil {
		if ctx.Err() != nil || !errors.As(err, &ce) {
			return nil, err
		}
		return &builder.DryRun{Run: string(run.ID), Recorded: builder.Ref{Version: meta.Version, Hash: meta.Hash},
			Reason: ce.Message, Diffs: []string{}, Problems: []string{}}, nil
	}
	out := &builder.DryRun{
		Ran: true, Run: string(run.ID), Recorded: builder.Ref{Version: meta.Version, Hash: meta.Hash},
		OriginalStatus: rep.OriginalStatus, Status: rep.Status, Matches: rep.Matches,
		Diffs: append([]string{}, rep.Diffs...), Problems: []string{}, Calls: rep.Calls, Replayed: rep.Replayed,
	}
	for _, p := range rep.Problems {
		out.Problems = append(out.Problems, p.Message)
	}
	return out, nil
}

// lastRecording is the newest run of the flow with a closed recording that can
// be replayed.
func (e *Engine) lastRecording(ctx context.Context, tenant core.TenantID, flow string) (core.Run, runrec.Meta, bool, error) {
	runs, err := e.Services.Runs.ListRuns(ctx, tenant, core.RunFilter{Flow: flow, Limit: 200})
	if err != nil {
		return core.Run{}, runrec.Meta{}, false, err //nolint:wrapcheck // *core.Error
	}
	for _, r := range runs { // newest first
		if !r.Status.Terminal() {
			continue
		}
		meta, found, err := e.loadClosed(ctx, tenant, r)
		if err != nil {
			return core.Run{}, runrec.Meta{}, false, err
		}
		if found && meta.Replayable() == nil {
			return r, meta, true, nil
		}
	}
	return core.Run{}, runrec.Meta{}, false, nil
}
