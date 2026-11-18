// Package brief builds `flo brief show`: what flows found and what waits for
// a decision, grouped by flow. It reads published outputs (flo/output@1) and
// open approvals; it writes nothing. Outputs are kept core.OutputRetention
// (90 days): the brief never shows older ones, and the prune job deletes them.
package brief

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// SchemaVersion is the version of the JSON document Brief encodes to. Within
// a major version fields are only added.
const SchemaVersion = "1.0"

// ListLimit bounds the outputs and approvals one brief reads.
const ListLimit = 1000

// UnknownFlow groups what belongs to a run that is no longer stored.
const UnknownFlow = "(unknown)"

// Brief is the document of `flo brief show`.
type Brief struct {
	SchemaVersion string    `json:"schema_version" yaml:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at" yaml:"generated_at"`
	// Since is where the window starts: the --since value, never older than
	// the retention.
	Since         time.Time   `json:"since" yaml:"since"`
	Flow          string      `json:"flow,omitempty" yaml:"flow,omitempty"`
	RetentionDays int         `json:"retention_days" yaml:"retention_days"`
	Flows         []FlowBrief `json:"flows" yaml:"flows"`
	Totals        Totals      `json:"totals" yaml:"totals"`
	// Truncated says a list hit ListLimit.
	Truncated bool `json:"truncated,omitempty" yaml:"truncated,omitempty"`
}

// Totals counts what the brief holds.
type Totals struct {
	Flows     int `json:"flows" yaml:"flows"`
	Findings  int `json:"findings" yaml:"findings"`
	Briefs    int `json:"briefs" yaml:"briefs"`
	Approvals int `json:"approvals" yaml:"approvals"`
}

// FlowBrief is what one flow published and what of it waits.
type FlowBrief struct {
	Flow      string     `json:"flow" yaml:"flow"`
	Findings  []Findings `json:"findings" yaml:"findings"`
	Briefs    []Text     `json:"briefs" yaml:"briefs"`
	Approvals []Waiting  `json:"approvals" yaml:"approvals"`
}

// Findings is one flo/output@1 step of kind findings.
type Findings struct {
	Output    string    `json:"output" yaml:"output"`
	Run       string    `json:"run" yaml:"run"`
	Step      string    `json:"step" yaml:"step"`
	Title     string    `json:"title,omitempty" yaml:"title,omitempty"`
	CreatedAt time.Time `json:"created_at" yaml:"created_at"`
	Items     []Finding `json:"items" yaml:"items"`
}

// Finding is one finding: {title, label?, detail?, url?, refs?} (node.md §6.7).
type Finding struct {
	Title  string `json:"title" yaml:"title"`
	Label  string `json:"label,omitempty" yaml:"label,omitempty"`
	Detail string `json:"detail,omitempty" yaml:"detail,omitempty"`
	URL    string `json:"url,omitempty" yaml:"url,omitempty"`
	Refs   []any  `json:"refs,omitempty" yaml:"refs,omitempty"`
}

// Text is one flo/output@1 step of kind brief.
type Text struct {
	Output    string    `json:"output" yaml:"output"`
	Run       string    `json:"run" yaml:"run"`
	Step      string    `json:"step" yaml:"step"`
	Title     string    `json:"title,omitempty" yaml:"title,omitempty"`
	Body      string    `json:"body" yaml:"body"`
	CreatedAt time.Time `json:"created_at" yaml:"created_at"`
}

// Waiting is an open approval.
type Waiting struct {
	ID        string     `json:"id" yaml:"id"`
	Run       string     `json:"run" yaml:"run"`
	Step      string     `json:"step" yaml:"step"`
	Title     string     `json:"title,omitempty" yaml:"title,omitempty"`
	Items     int        `json:"items" yaml:"items"`
	Waiting   int        `json:"waiting" yaml:"waiting"`
	Task      string     `json:"task,omitempty" yaml:"task,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at" yaml:"created_at"`
	// Show and Set are the commands that inspect and decide it.
	Show string `json:"show" yaml:"show"`
	Set  string `json:"set" yaml:"set"`
}

// Options select what Build reads.
type Options struct {
	// Since keeps outputs created at or after it; zero keeps every retained
	// output. Waiting approvals are shown whatever their age.
	Since time.Time
	// Flow keeps one flow.
	Flow string
}

// Deps are the ports Build reads.
type Deps struct {
	Outputs   core.OutputStore
	Runs      core.RunStore
	Approvals interface {
		List(ctx context.Context, tenant core.TenantID, f core.ApprovalFilter) ([]core.Approval, error)
	}
	Clock core.Clock
}

// Build reads the tenant's outputs and open approvals and groups them by
// flow, flows in name order, newest first within a flow.
func Build(ctx context.Context, d Deps, tenant core.TenantID, o Options) (Brief, error) {
	now := d.Clock.Now()
	floor := now.Add(-core.OutputRetention)
	since := o.Since
	if since.Before(floor) {
		since = floor
	}
	b := Brief{
		SchemaVersion: SchemaVersion, GeneratedAt: now.UTC(), Since: since.UTC(), Flow: o.Flow,
		RetentionDays: int(core.OutputRetention / (24 * time.Hour)), Flows: []FlowBrief{},
	}
	outs, err := d.Outputs.ListOutputs(ctx, tenant, core.OutputFilter{Since: since, Limit: ListLimit})
	if err != nil {
		return Brief{}, err //nolint:wrapcheck // port error passes through
	}
	aps, err := d.Approvals.List(ctx, tenant, core.ApprovalFilter{Status: core.ApprovalOpen, Limit: ListLimit})
	if err != nil {
		return Brief{}, err //nolint:wrapcheck // port error passes through
	}
	b.Truncated = len(outs) >= ListLimit || len(aps) >= ListLimit

	g := &grouper{d: d, tenant: tenant, flow: o.Flow, by: map[string]*FlowBrief{}, names: map[core.RunID]string{}}
	for _, out := range outs {
		if out.CreatedAt.Before(since) {
			continue
		}
		if err := g.output(ctx, out); err != nil {
			return Brief{}, err
		}
	}
	for _, a := range aps {
		if err := g.approval(ctx, a); err != nil {
			return Brief{}, err
		}
	}
	for _, f := range g.by {
		b.Flows = append(b.Flows, *f)
		b.Totals.Findings += len(f.Findings)
		b.Totals.Briefs += len(f.Briefs)
		b.Totals.Approvals += len(f.Approvals)
	}
	slices.SortFunc(b.Flows, func(x, y FlowBrief) int { return cmp.Compare(x.Flow, y.Flow) })
	b.Totals.Flows = len(b.Flows)
	return b, nil
}

// grouper files outputs and approvals under the flow of their run.
type grouper struct {
	d      Deps
	tenant core.TenantID
	flow   string
	by     map[string]*FlowBrief
	names  map[core.RunID]string
}

// flowOf names the flow of a run; a run that is gone files under UnknownFlow.
func (g *grouper) flowOf(ctx context.Context, id core.RunID) (string, error) {
	if n, ok := g.names[id]; ok {
		return n, nil
	}
	name := UnknownFlow
	r, err := g.d.Runs.GetRun(ctx, g.tenant, id)
	switch {
	case err == nil:
		name = r.Flow.Name
	case core.IsCode(err, core.CodeRunNotFound):
	default:
		return "", err //nolint:wrapcheck // port error passes through
	}
	g.names[id] = name
	return name, nil
}

// slot returns the flow's group, or nil when --flow excludes it.
func (g *grouper) slot(ctx context.Context, run core.RunID) (*FlowBrief, error) {
	name, err := g.flowOf(ctx, run)
	if err != nil {
		return nil, err
	}
	if g.flow != "" && name != g.flow {
		return nil, nil //nolint:nilnil // excluded by --flow
	}
	f := g.by[name]
	if f == nil {
		f = &FlowBrief{Flow: name, Findings: []Findings{}, Briefs: []Text{}, Approvals: []Waiting{}}
		g.by[name] = f
	}
	return f, nil
}

func (g *grouper) output(ctx context.Context, o core.Output) error {
	f, err := g.slot(ctx, o.Run)
	if err != nil || f == nil {
		return err
	}
	switch o.Kind {
	case core.OutputFindings:
		f.Findings = append(f.Findings, Findings{
			Output: string(o.ID), Run: string(o.Run), Step: o.StepID, Title: o.Title, CreatedAt: o.CreatedAt.UTC(), Items: findings(o.Items),
		})
	case core.OutputBrief:
		f.Briefs = append(f.Briefs, Text{
			Output: string(o.ID), Run: string(o.Run), Step: o.StepID, Title: o.Title, Body: o.Body, CreatedAt: o.CreatedAt.UTC(),
		})
	}
	return nil
}

func (g *grouper) approval(ctx context.Context, a core.Approval) error {
	f, err := g.slot(ctx, a.Run)
	if err != nil || f == nil {
		return err
	}
	w := Waiting{
		ID: string(a.ID), Run: string(a.Run), Step: a.StepID, Title: a.Title, Items: len(a.Items), Waiting: len(a.Undecided()),
		Task: a.TaskRef, CreatedAt: a.CreatedAt.UTC(),
		Show: "flo approval show " + string(a.ID), Set: "flo approval set " + string(a.ID) + " --item N=approve|reject",
	}
	if a.ExpiresAt != nil {
		t := a.ExpiresAt.UTC()
		w.ExpiresAt = &t
	}
	f.Approvals = append(f.Approvals, w)
	return nil
}

// findings decodes a stored items list. An item is a JSON object; a field of
// another type is dropped rather than failing the brief.
func findings(items []any) []Finding {
	out := make([]Finding, 0, len(items))
	for _, it := range items {
		raw, err := json.Marshal(it)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		f := Finding{Title: str(m["title"]), Label: str(m["label"]), Detail: str(m["detail"]), URL: str(m["url"])}
		f.Refs, _ = m["refs"].([]any)
		out = append(out, f)
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
