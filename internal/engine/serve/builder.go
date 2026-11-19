package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
const DefaultBuilderModel = "ollama://qwen3:4b-instruct-2507-q4_K_M"

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
	// New drafts a flow that does not exist yet: Flow is its name.
	New bool
	// Privacy is, with New, the privacy ceiling of the model call: there is
	// no base flow to read it from. Empty is local-only.
	Privacy string
	// From refines the draft it names (multi-turn); Flow is not needed and
	// the instruction is on top of that draft's.
	From string
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

// basis is what a build starts from: the base the model is shown, or for a new
// flow the name alone.
type basis struct {
	flow, version string
	doc           []byte
	base          builder.Ref
	head          string
	bundle        string
	file, hash    string
	ceiling       specrun.Privacy
	isNew         bool
	parent        *builder.Draft
}

var newFlowName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// existingBasis reads the flow's base version, and the bundle document on disk
// it came from.
func (e *Engine) existingBasis(ctx context.Context, tenant core.TenantID, flow string) (basis, error) {
	vs, err := e.Services.Flows.ListVersions(ctx, tenant, flow)
	if err != nil {
		return basis{}, err //nolint:wrapcheck // *core.Error
	}
	if len(vs) == 0 {
		return basis{}, core.Errorf(core.CodeFlowNotFound, "flow %q not found", flow).WithSuggestion("flo flow list")
	}
	base, head := baseOf(vs)
	f, err := cel.ParseFlow(bytes.NewReader(base.Source), base.Name+".yaml")
	if err != nil {
		return basis{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return basis{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	b := basis{flow: base.Name, version: base.Version, doc: base.Source, base: builder.Ref{Version: base.Version, Hash: base.Hash},
		head: head, bundle: base.Bundle, ceiling: set.Privacy}
	b.file, b.hash = bundleDocument(base.Bundle)
	return b, nil
}

// newBasis is the start of a flow that does not exist: its name must be free.
func (e *Engine) newBasis(ctx context.Context, tenant core.TenantID, name, privacy string) (basis, error) {
	if !newFlowName.MatchString(name) {
		return basis{}, core.Errorf(core.CodeUsage, "flow name %q must match %s", name, newFlowName).
			WithSuggestion(`flo flow draft create --new <name> "what the flow does"`)
	}
	if err := e.requireFree(ctx, tenant, name); err != nil {
		return basis{}, err
	}
	ceiling := specrun.PrivacyLocalOnly
	switch privacy {
	case "", string(specrun.PrivacyLocalOnly):
	case string(specrun.PrivacyCloudAllowed):
		ceiling = specrun.PrivacyCloudAllowed
	default:
		return basis{}, core.Errorf(core.CodeUsage, "privacy %q is not local-only or cloud-allowed", privacy)
	}
	return basis{flow: name, isNew: true, ceiling: ceiling}, nil
}

// requireFree fails with duplicate when a flow of that name is stored.
func (e *Engine) requireFree(ctx context.Context, tenant core.TenantID, name string) error {
	vs, err := e.Services.Flows.ListVersions(ctx, tenant, name)
	if err != nil && !core.IsCode(err, core.CodeFlowNotFound) {
		return err //nolint:wrapcheck // *core.Error
	}
	if len(vs) > 0 {
		return core.Errorf(core.CodeDuplicate, "flow %q exists already", name).
			WithSuggestion("flo flow draft create " + name + ` "what to change"`)
	}
	return nil
}

// bundleDocument is the flow document of a bundle directory and its hash; both
// empty when there is no bundle or no readable document.
func bundleDocument(bundle string) (path, hash string) {
	if bundle == "" {
		return "", ""
	}
	for _, n := range bundleFiles {
		p := filepath.Join(bundle, n)
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // the bundle the flow was applied from
			return p, hashOf(b)
		}
	}
	return "", ""
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateDraft asks the model for the full next version of a flow, validates it,
// feeds the errors back (at most builder.MaxFixes times), replays the
// valid proposal on the flow's last recording and keeps it as a draft. Nothing
// is stored as a flow version: that is ApplyDraft.
//
// With req.New the flow does not exist yet (nothing to read a privacy ceiling
// from, nothing to replay on); with req.From the build refines a draft: the
// model gets the original base, the draft's source, the instructions so far and
// the new one. The refined draft keeps the base of the one it refines and links
// to it; the earlier draft is kept.
func (e *Engine) CreateDraft(ctx context.Context, tenant core.TenantID, req DraftRequest) (builder.Draft, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // *core.Error
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return builder.Draft{}, core.New(core.CodeUsage, "the instruction is empty").
			WithSuggestion(`flo flow draft create <flow> "what to change"`)
	}
	var (
		b   basis
		err error
	)
	switch {
	case req.From != "":
		if req.New || req.Privacy != "" {
			return builder.Draft{}, core.New(core.CodeUsage, "--from refines a draft: --new and --privacy do not apply")
		}
		parent, err := e.CheckDraft(ctx, tenant, req.From) // a moved base is draft_stale here too
		if err != nil {
			return builder.Draft{}, err
		}
		if parent.New {
			b = basis{flow: parent.Flow, isNew: true, ceiling: specrun.Privacy(parent.Ceiling)}
		} else if b, err = e.existingBasis(ctx, tenant, parent.Flow); err != nil {
			return builder.Draft{}, err
		} else {
			b.file, b.hash = parent.File, parent.FileHash // the file the first draft was made on
		}
		b.parent = &parent
	case req.New:
		if b, err = e.newBasis(ctx, tenant, req.Flow, req.Privacy); err != nil {
			return builder.Draft{}, err
		}
	default:
		if req.Privacy != "" {
			return builder.Draft{}, core.New(core.CodeUsage, "--privacy is for a new flow: an existing flow has its own")
		}
		if b, err = e.existingBasis(ctx, tenant, req.Flow); err != nil {
			return builder.Draft{}, err
		}
	}
	model := firstNonEmpty(req.Model, e.Config.BuilderModel, DefaultBuilderModel)
	prop, err := e.proposer(tenant, req.Caller, model, b.ceiling)
	if err != nil {
		return builder.Draft{}, err
	}
	loc, err := e.Nodes.Locator(b.bundle)
	if err != nil {
		return builder.Draft{}, err
	}

	var last Checked
	validate := func(ctx context.Context, doc []byte) (builder.Check, error) {
		ck, err := e.Flows.Check(ctx, tenant, Source{File: b.flow + ".yaml", Bundle: b.bundle, Data: doc})
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
	in := builder.LoopInput{
		Flow: b.flow, Version: b.version, Base: b.doc, Instruction: req.Instruction,
		Catalog: catalogOf(loc), Model: prop, Validate: validate, New: b.isNew,
	}
	var history []string
	if b.parent != nil {
		history = append(append(history, b.parent.History...), b.parent.Instruction)
		in.Previous, in.History = b.parent.Source, history
	}
	res, err := builder.Run(ctx, in)
	if err != nil {
		return builder.Draft{}, err //nolint:wrapcheck // the loop's and the model's classified errors
	}
	d := builder.Draft{
		ID: e.Services.IDs.New(core.PrefixDraft), Flow: b.flow, Instruction: req.Instruction, New: b.isNew, History: history,
		Ceiling: string(b.ceiling), Base: b.base, Head: b.head,
		Version: last.Flow.Version, Hash: last.Flow.Hash, Source: string(res.Doc), Bundle: b.bundle, File: b.file, FileHash: b.hash,
		Model: res.Model, Attempts: res.Attempts, Warnings: orNone(res.Warnings), Cost: res.Cost,
		CreatedAt: e.Config.Clock.Now().UTC(), CreatedBy: req.Caller,
	}
	if d.Ceiling == "" {
		d.Ceiling = string(specrun.PrivacyLocalOnly)
	}
	if b.parent != nil {
		d.Parent = b.parent.ID
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
			WithSuggestion("write the model as scheme://<model>, e.g. ollama://qwen3:4b-instruct-2507-q4_K_M")
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
	if d.New { // stale when the name is taken meanwhile
		if err := e.requireFree(ctx, tenant, d.Flow); err != nil {
			return builder.Draft{}, core.Errorf(core.CodeDraftStale, "draft %s is a new flow %s, and a flow of that name exists now", d.ID, d.Flow).
				WithSuggestion("name the new flow otherwise: flo flow draft create --new <name> <instruction>")
		}
		return d, nil
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
	// Overridden are the blocking differences --force went over.
	Overridden []string
	// Written is the file written with Write: the bundle's flow.yaml.
	Written string
}

// ApplyDraftOptions are the choices of an apply.
type ApplyDraftOptions struct {
	// Force applies over a dry run that shows a step failing or ending
	// differently, or a call with no recording.
	Force bool
	// Write also writes the document to disk: over the flow.yaml of the
	// bundle the flow was applied from, if it is the file the draft was made
	// on; for a new flow, a new bundle <Dir>/<name>/flow.yaml that must not
	// exist.
	Write bool
	// Dir is where the bundle of a new flow goes, with Write; absolute.
	Dir string
}

// ApplyDraft applies a draft without writing a file and without forcing.
func (e *Engine) ApplyDraft(ctx context.Context, tenant core.TenantID, caller, id string) (DraftApplied, error) {
	return e.ApplyDraftWith(ctx, tenant, caller, id, ApplyDraftOptions{})
}

// ApplyDraftWith stores the draft's document as the flow's next version (the
// first one, for a new flow), only when the base has not moved (draft_stale
// otherwise). The document is validated again and the caller's grants checked,
// exactly as `flo flow apply` does, and the proposal is replayed on the flow's
// last recording once more so the outcome is read at the moment of the apply.
// A dry run that shows a step failing or ending with another status, or calls
// with no recording, refuses the apply with draft_dry_run_failed unless
// opts.Force; a different output only reports, and no recording at all is
// nothing to compare, so it is allowed (the report says why). With opts.Write
// the document is also written to disk, atomically, only when the file is the
// one the draft was made on (draft_stale otherwise, before anything is
// stored). The version is a draft version: test it and activate it as any
// other. The draft is spent.
func (e *Engine) ApplyDraftWith(ctx context.Context, tenant core.TenantID, caller, id string, opts ApplyDraftOptions) (DraftApplied, error) {
	d, err := e.CheckDraft(ctx, tenant, id)
	if err != nil {
		return DraftApplied{}, err
	}
	var target string // the file to write
	if opts.Write {
		if target, err = writeTarget(d, opts); err != nil {
			return DraftApplied{}, err
		}
	}
	dry, err := e.dryRunDraft(ctx, tenant, caller, d)
	if err != nil {
		return DraftApplied{}, err
	}
	out := DraftApplied{Draft: d, DryRun: dry}
	if len(dry.Blocking) > 0 {
		if !opts.Force {
			return out, core.Errorf(core.CodeDraftDryRunFailed, "the dry run of draft %s on its flow's last recording shows %s", d.ID, listed(dry.Blocking)).
				WithDetail("blocking", dry.Blocking).
				WithSuggestion("flo flow draft show " + d.ID + " reads the dry run; --force applies anyway")
		}
		out.Overridden = dry.Blocking
	}
	bundle := d.Bundle
	created := ""
	if d.New && opts.Write {
		dir := filepath.Dir(target)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return out, core.Wrap(core.CodeInternal, err)
		}
		created = dir
		if err := writeFileAtomic(target, []byte(d.Source), 0o644); err != nil { //nolint:gosec // a flow document, meant to be read and committed
			_ = os.RemoveAll(created)
			return out, err
		}
		bundle = dir
	}
	res, err := e.Flows.Apply(ctx, tenant, caller, Source{File: d.Flow + ".yaml", Bundle: bundle, Data: []byte(d.Source)})
	if err != nil {
		if created != "" {
			_ = os.RemoveAll(created)
		}
		return out, err
	}
	out.Applied = res
	if opts.Write {
		if !d.New {
			if err := checkFileUnchanged(d, target); err != nil { // again, as late as possible
				return out, err
			}
			if err := writeFileAtomic(target, []byte(d.Source), 0); err != nil {
				return out, err
			}
		}
		out.Written = target
	}
	if err := e.DraftStore.Delete(tenant, d.ID); err != nil && !core.IsCode(err, core.CodeDraftNotFound) {
		return DraftApplied{}, err //nolint:wrapcheck // *core.Error
	}
	return out, nil
}

func listed(items []string) string {
	if len(items) <= 3 {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s (and %d more)", strings.Join(items[:3], "; "), len(items)-3)
}

// writeTarget is the file --write writes, checked before anything is stored.
func writeTarget(d builder.Draft, opts ApplyDraftOptions) (string, error) {
	if d.New {
		if !filepath.IsAbs(opts.Dir) {
			return "", core.New(core.CodeUsage, "writing a new flow needs the directory its bundle goes in")
		}
		dir := filepath.Join(opts.Dir, d.Flow)
		if _, err := os.Lstat(dir); err == nil {
			return "", core.Errorf(core.CodeDuplicate, "%s exists already: not overwriting it", dir).
				WithSuggestion("apply without --write, or write the flow elsewhere with --dir")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", core.Wrap(core.CodeInternal, err)
		}
		return filepath.Join(dir, "flow.yaml"), nil
	}
	if d.File == "" || d.FileHash == "" {
		return "", core.Errorf(core.CodeUsage, "flow %s was not applied from a bundle directory: there is no flow.yaml to write", d.Flow).
			WithSuggestion("apply without --write")
	}
	if err := checkFileUnchanged(d, d.File); err != nil {
		return "", err
	}
	return d.File, nil
}

// checkFileUnchanged is draft_stale when the file is not the one the draft was
// made on.
func checkFileUnchanged(d builder.Draft, path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // the bundle document recorded with the draft
	if err != nil || hashOf(b) != d.FileHash {
		what := "changed"
		if err != nil {
			what = "gone"
		}
		return core.Errorf(core.CodeDraftStale, "%s %s since draft %s was made: not overwriting it", path, what, d.ID).
			WithDetail("file", path).
			WithSuggestion("make a new draft on the file as it is: flo flow draft create " + d.Flow + " <instruction>")
	}
	return nil
}

// writeFileAtomic writes data beside path and renames it over: a reader sees
// the old file or the new one. An existing file keeps its mode; mode is for a
// new one (0 is 0o644).
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	} else if mode == 0 {
		mode = 0o644
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".flow-*")
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return core.Wrap(core.CodeInternal, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return core.Wrap(core.CodeInternal, err)
	}
	if err := tmp.Close(); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	return nil
}

// dryRunDraft replays the draft's document on the flow's last recording, in
// the sandbox, offline: what would differ in the outcome of that run. A flow
// with no usable recording, or a recording that cannot be replayed for this
// caller, is a DryRun that did not run, with the reason; only a cancelled
// context is an error.
func (e *Engine) dryRunDraft(ctx context.Context, tenant core.TenantID, caller string, d builder.Draft) (*builder.DryRun, error) {
	if d.New {
		return &builder.DryRun{Reason: "a new flow has no recording to replay on: test it with `flo flow test " + d.Flow + " --record` once applied",
			Diffs: []string{}, Problems: []string{}, Blocking: []string{}}, nil
	}
	run, meta, found, err := e.lastRecording(ctx, tenant, d.Flow)
	if err != nil {
		return nil, err
	}
	if !found {
		return &builder.DryRun{Reason: "the flow has no recording to replay on: record a run with `flo run create --flow " + d.Flow + " --record`",
			Diffs: []string{}, Problems: []string{}, Blocking: []string{}}, nil
	}
	v := core.FlowVersion{Tenant: tenant, Name: d.Flow, Version: d.Version, Hash: d.Hash, Source: []byte(d.Source), Bundle: d.Bundle}
	rep, err := e.replayOn(ctx, tenant, ReplayRequest{Run: run.ID, Caller: caller}, run, meta, v)
	var ce *core.Error
	if err != nil {
		if ctx.Err() != nil || !errors.As(err, &ce) {
			return nil, err
		}
		return &builder.DryRun{Run: string(run.ID), Recorded: builder.Ref{Version: meta.Version, Hash: meta.Hash},
			Reason: ce.Message, Diffs: []string{}, Problems: []string{}, Blocking: []string{}}, nil
	}
	out := &builder.DryRun{
		Ran: true, Run: string(run.ID), Recorded: builder.Ref{Version: meta.Version, Hash: meta.Hash},
		OriginalStatus: rep.OriginalStatus, Status: rep.Status, Matches: rep.Matches,
		Diffs: append([]string{}, rep.Diffs...), Problems: []string{}, Blocking: append([]string{}, rep.blocking...), Calls: rep.Calls, Replayed: rep.Replayed,
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
