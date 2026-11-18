package serve

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
	"hop.top/flo/spec/flowcheck"
)

// Flows validates, stores and activates flow versions. It is what `flo flow
// apply|validate|set` runs.
type Flows struct {
	svc   *core.Services
	nodes *Nodes
}

// NewFlows returns the flow operations over the engine's services.
func NewFlows(svc *core.Services, nodes *Nodes) *Flows { return &Flows{svc: svc, nodes: nodes} }

// Source is a flow document read from disk.
type Source struct {
	// File is the document's path.
	File string
	// Bundle is the directory whose nodes/ and trusts/ the flow sees; empty
	// for a single file (flow.md §2.4).
	Bundle string
	Data   []byte
}

// bundleFiles are the names of a bundle's document.
var bundleFiles = []string{"flow.yaml", "flow.yml"}

// ReadSource reads the flow at path: a directory is a bundle and its document
// is flow.yaml; a file named flow.yaml also makes its directory the bundle;
// any other file is a single-file flow.
func ReadSource(path string) (Source, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Source{}, core.Wrap(core.CodeUsage, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Source{}, core.Errorf(core.CodeUsage, "flow %s: %v", path, err)
	}
	src := Source{File: abs}
	if fi.IsDir() {
		src.Bundle, src.File = abs, ""
		for _, n := range bundleFiles {
			if _, err := os.Stat(filepath.Join(abs, n)); err == nil {
				src.File = filepath.Join(abs, n)
				break
			}
		}
		if src.File == "" {
			return Source{}, core.Errorf(core.CodeUsage, "bundle %s has no flow.yaml", path).WithSuggestion("a flow bundle holds flow.yaml and, beside it, nodes/")
		}
	} else if b := filepath.Base(abs); b == bundleFiles[0] || b == bundleFiles[1] {
		src.Bundle = filepath.Dir(abs)
	}
	if src.Data, err = os.ReadFile(src.File); err != nil { //nolint:gosec // the flow document the caller named
		return Source{}, core.Errorf(core.CodeUsage, "flow %s: %v", path, err)
	}
	return src, nil
}

// Issue is one validation finding.
type Issue struct {
	Rule     string `json:"rule,omitempty" yaml:"rule,omitempty"`
	Code     string `json:"code" yaml:"code"`
	Severity string `json:"severity" yaml:"severity"`
	Step     string `json:"step,omitempty" yaml:"step,omitempty"`
	Field    string `json:"field,omitempty" yaml:"field,omitempty"`
	Message  string `json:"message" yaml:"message"`
}

// Report is what validating a flow found.
type Report struct {
	Flow     string  `json:"flow" yaml:"flow"`
	Version  string  `json:"version" yaml:"version"`
	Hash     string  `json:"hash" yaml:"hash"`
	Valid    bool    `json:"valid" yaml:"valid"`
	Errors   int     `json:"errors" yaml:"errors"`
	Warnings int     `json:"warnings" yaml:"warnings"`
	Issues   []Issue `json:"issues" yaml:"issues"`
}

// Err returns nil for a valid report, else the *core.Error of the first
// error, carrying every issue.
func (r Report) Err() error {
	if r.Valid {
		return nil
	}
	var first *Issue
	n := 0
	for i := range r.Issues {
		if r.Issues[i].Severity == string(flowcheck.SeverityError) {
			if first == nil {
				first = &r.Issues[i]
			}
			n++
		}
	}
	code := core.CodeInvalidFlow
	if c := core.Code(first.Code); c.Known() {
		code = c
	}
	msg := first.Message
	if first.Step != "" {
		msg = "step " + first.Step + ": " + msg
	}
	if n > 1 {
		msg += fmt.Sprintf(" (and %d more)", n-1)
	}
	return core.New(code, msg).WithDetail("issues", r.Issues).
		WithSuggestion("flo flow validate -f <flow> lists every finding")
}

// Checked is a validated flow document.
type Checked struct {
	Flow   *spec.Flow
	Report Report
	// Capabilities are every capability the flow's nodes declare, sorted.
	Capabilities []string
}

// Check parses src and applies every validation rule that needs the node
// layers and the grants on record (flow.md §8): a document that does not
// parse is an invalid_flow error; findings come back in the report.
func (f *Flows) Check(ctx context.Context, tenant core.TenantID, src Source) (Checked, error) {
	name := src.File
	if name == "" {
		name = "flow.yaml"
	}
	fl, err := cel.ParseFlow(bytes.NewReader(src.Data), filepath.Base(name))
	if err != nil {
		return Checked{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	loc, err := f.nodes.Locator(src.Bundle)
	if err != nil {
		return Checked{}, err
	}
	var flows spec.Locator
	if src.Bundle != "" {
		flows = &spec.DirLocator{Dirs: []string{src.Bundle}}
	}
	view, err := f.grantView(ctx, tenant)
	if err != nil {
		return Checked{}, err
	}
	rep := flowcheck.Check(fl, flowcheck.Options{Nodes: loc, Flows: flows, Grants: view})
	out := Checked{Flow: fl, Report: reportOf(fl, rep)}
	out.Capabilities = capabilitiesOf(fl, loc, flows)
	return out, nil
}

func reportOf(fl *spec.Flow, rep flowcheck.Report) Report {
	r := Report{Flow: fl.Name, Version: fl.Version, Hash: fl.Hash, Issues: []Issue{}}
	for _, i := range rep {
		r.Issues = append(r.Issues, Issue{
			Rule: i.Rule, Code: i.Code, Severity: string(i.Severity), Step: i.Step, Field: i.Field, Message: i.Msg,
		})
		if i.Severity == flowcheck.SeverityError {
			r.Errors++
		} else {
			r.Warnings++
		}
	}
	r.Valid = r.Errors == 0
	return r
}

// capabilitiesOf collects what the flow's node steps declare.
func capabilitiesOf(fl *spec.Flow, loc flowcheck.NodeResolver, flows spec.Locator) []string {
	steps := fl.Steps
	if flows != nil {
		if exp, err := spec.Expand(fl, flows); err == nil {
			steps = exp
		}
	}
	set := map[string]bool{}
	var walk func([]spec.Step)
	walk = func(ss []spec.Step) {
		for i := range ss {
			if ss[i].IsNode() {
				if e, err := loc.Locate(ss[i].Uses); err == nil {
					for _, c := range e.Spec.Capabilities {
						set[c] = true
					}
				}
			}
			walk(ss[i].Steps)
		}
	}
	walk(steps)
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// grantView is flowcheck's narrow view of the grants on record.
type grantView map[string][]string

func (g grantView) Capabilities(profile string) ([]string, bool) {
	c, ok := g[profile]
	return c, ok
}

func (f *Flows) grantView(ctx context.Context, tenant core.TenantID) (grantView, error) {
	gs, err := f.svc.Grants.List(ctx, tenant, "")
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	v := grantView{}
	for _, g := range gs {
		v[g.Profile] = append(v[g.Profile], g.Capability)
	}
	return v, nil
}

// ApplyResult is what Apply did.
type ApplyResult struct {
	Version core.FlowVersion
	// Created is false when the same content was already stored: the existing
	// version is returned and nothing changed.
	Created bool
	Report  Report
}

// Apply validates src, checks that caller may use the flow's run_as and that
// profile holds every capability the nodes declare (V18), and stores it as a
// new version when its content is new (draft). Same bytes is the same version.
func (f *Flows) Apply(ctx context.Context, tenant core.TenantID, caller string, src Source) (ApplyResult, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return ApplyResult{}, err //nolint:wrapcheck // *core.Error
	}
	ck, err := f.Check(ctx, tenant, src)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := ck.Report.Err(); err != nil {
		return ApplyResult{Report: ck.Report}, err
	}
	if err := f.svc.Grants.CheckFlow(ctx, core.GrantCheck{Tenant: tenant, Caller: caller, RunAs: ck.Flow.RunAs, Capabilities: ck.Capabilities}); err != nil {
		return ApplyResult{Report: ck.Report}, err //nolint:wrapcheck // *core.Error
	}
	v, created, err := f.svc.Flows.PutVersion(ctx, core.NewFlowVersion{
		Tenant: tenant, Name: ck.Flow.Name, Version: ck.Flow.Version, Hash: ck.Flow.Hash,
		Source: src.Data, Bundle: src.Bundle, At: f.svc.Clock.Now(),
	})
	if err != nil {
		return ApplyResult{}, err //nolint:wrapcheck // port error passes through
	}
	return ApplyResult{Version: v, Created: created, Report: ck.Report}, nil
}

// SetStateResult is what SetState did.
type SetStateResult struct {
	Version core.FlowVersion
	// Changed is false when the version already had the state.
	Changed bool
}

// SetState activates or pauses a flow. Activating checks the version again
// (validation, then the grants of the caller, then the ActivationGate), makes
// it the version new runs start on and syncs its triggers; ref names the
// version (hash or document version), empty is the newest. Pausing the live
// version stops its triggers; its runs continue. Setting the state a
// version has is a no-op, but an already active flow has its triggers synced
// again, which repairs a half-finished activation.
func (f *Flows) SetState(ctx context.Context, tenant core.TenantID, caller, flow, ref string, state core.FlowState) (SetStateResult, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return SetStateResult{}, err //nolint:wrapcheck // *core.Error
	}
	switch state {
	case core.FlowActive:
		return f.activate(ctx, tenant, caller, flow, ref)
	case core.FlowPaused:
		return f.pause(ctx, tenant, flow, ref)
	}
	return SetStateResult{}, core.Errorf(core.CodeUsage, "--state %q: want active or paused", state)
}

func (f *Flows) activate(ctx context.Context, tenant core.TenantID, caller, flow, ref string) (SetStateResult, error) {
	v, err := f.svc.Flows.GetVersion(ctx, tenant, flow, ref)
	if err != nil {
		return SetStateResult{}, err //nolint:wrapcheck // port error passes through
	}
	if v.State == core.FlowActive {
		return SetStateResult{Version: v}, f.svc.TrigSync.Sync(ctx, tenant, v) //nolint:wrapcheck // port error passes through
	}
	ck, err := f.Check(ctx, tenant, Source{File: v.Name + ".yaml", Bundle: v.Bundle, Data: v.Source})
	if err != nil {
		return SetStateResult{}, err
	}
	if err := ck.Report.Err(); err != nil {
		return SetStateResult{}, err
	}
	if err := f.svc.Grants.CheckFlow(ctx, core.GrantCheck{Tenant: tenant, Caller: caller, RunAs: ck.Flow.RunAs, Capabilities: ck.Capabilities}); err != nil {
		return SetStateResult{}, err //nolint:wrapcheck // *core.Error
	}
	if err := f.svc.Gate.Allow(ctx, tenant, v); err != nil {
		return SetStateResult{}, err //nolint:wrapcheck // *core.Error
	}
	nv, err := f.svc.Flows.SetState(ctx, tenant, flow, v.Hash, core.FlowActive, f.svc.Clock.Now())
	if err != nil {
		return SetStateResult{}, err //nolint:wrapcheck // port error passes through
	}
	if err := f.svc.TrigSync.Sync(ctx, tenant, nv); err != nil {
		return SetStateResult{Version: nv, Changed: true}, err //nolint:wrapcheck // port error passes through
	}
	return SetStateResult{Version: nv, Changed: true}, nil
}

func (f *Flows) pause(ctx context.Context, tenant core.TenantID, flow, ref string) (SetStateResult, error) {
	var v core.FlowVersion
	if ref != "" {
		got, err := f.svc.Flows.GetVersion(ctx, tenant, flow, ref)
		if err != nil {
			return SetStateResult{}, err //nolint:wrapcheck // port error passes through
		}
		v = got
	} else {
		vs, err := f.svc.Flows.ListVersions(ctx, tenant, flow)
		if err != nil {
			return SetStateResult{}, err //nolint:wrapcheck // port error passes through
		}
		for _, c := range vs { // newest first
			if c.State == core.FlowActive || c.State == core.FlowPaused {
				v = c
				break
			}
		}
		if v.Hash == "" {
			return SetStateResult{}, core.Errorf(core.CodeFlowNotActive, "flow %q has no active version to pause", flow).
				WithSuggestion("flo flow set " + flow + " --state active")
		}
	}
	nv, err := f.svc.Flows.SetState(ctx, tenant, flow, v.Hash, core.FlowPaused, f.svc.Clock.Now())
	if err != nil {
		return SetStateResult{}, err //nolint:wrapcheck // port error passes through
	}
	if err := f.svc.TrigSync.Unsync(ctx, tenant, flow); err != nil {
		return SetStateResult{Version: nv, Changed: nv.State != v.State}, err //nolint:wrapcheck // port error passes through
	}
	return SetStateResult{Version: nv, Changed: v.State != core.FlowPaused}, nil
}
