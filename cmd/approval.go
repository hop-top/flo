package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
)

const approvalSchemaVersion = "1.0"

// approvalRow is one approval as `flo approval list` prints it.
type approvalRow struct {
	ID      string `table:"ID" json:"id" yaml:"id"`
	Run     string `table:"RUN" json:"run" yaml:"run"`
	Step    string `table:"STEP" json:"step" yaml:"step"`
	Status  string `table:"STATUS" json:"status" yaml:"status"`
	Decided string `table:"DECIDED" json:"decided" yaml:"decided"`
	Title   string `table:"TITLE" json:"title" yaml:"title"`
	Task    string `table:"TASK" json:"task,omitempty" yaml:"task,omitempty"`
	Expires string `table:"EXPIRES" json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
}

// itemView is one item of an approval as `flo approval show` prints it. N is
// the position `--item N=...` also accepts.
type itemView struct {
	N          int      `json:"n" yaml:"n"`
	ID         string   `json:"id" yaml:"id"`
	Action     string   `json:"action,omitempty" yaml:"action,omitempty"`
	Why        string   `json:"why,omitempty" yaml:"why,omitempty"`
	Class      []string `json:"class,omitempty" yaml:"class,omitempty"`
	Capability string   `json:"capability,omitempty" yaml:"capability,omitempty"`
	Args       any      `json:"args,omitempty" yaml:"args,omitempty"`
	Digest     string   `json:"digest,omitempty" yaml:"digest,omitempty"`
	Verdict    string   `json:"verdict,omitempty" yaml:"verdict,omitempty"`
	By         string   `json:"by,omitempty" yaml:"by,omitempty"`
	At         string   `json:"at,omitempty" yaml:"at,omitempty"`
	Reason     string   `json:"reason,omitempty" yaml:"reason,omitempty"`
	Correction any      `json:"correction,omitempty" yaml:"correction,omitempty"`
}

// approvalView is an approval with its items, trust and decisions.
type approvalView struct {
	ID        string     `json:"id" yaml:"id"`
	Run       string     `json:"run" yaml:"run"`
	Step      string     `json:"step" yaml:"step"`
	Kind      string     `json:"kind" yaml:"kind"`
	Title     string     `json:"title,omitempty" yaml:"title,omitempty"`
	Status    string     `json:"status" yaml:"status"`
	Version   uint64     `json:"version" yaml:"version"`
	Trust     string     `json:"trust,omitempty" yaml:"trust,omitempty"`
	Verdicts  []string   `json:"verdicts" yaml:"verdicts"`
	Task      string     `json:"task,omitempty" yaml:"task,omitempty"`
	Assignee  string     `json:"assignee,omitempty" yaml:"assignee,omitempty"`
	ExpiresAt string     `json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
	Waiting   int        `json:"waiting" yaml:"waiting"`
	Items     []itemView `json:"items" yaml:"items"`
}

// approvalDecision is what `flo approval set` reports.
type approvalDecision struct {
	ID      string `table:"ID" json:"id" yaml:"id"`
	Status  string `table:"STATUS" json:"status" yaml:"status"`
	Decided int    `table:"DECIDED" json:"decided" yaml:"decided"`
	Waiting int    `table:"WAITING" json:"waiting" yaml:"waiting"`
	// Changed is false when every named item already had a decision: the
	// same request twice yields the same state.
	Changed bool `table:"CHANGED" json:"changed" yaml:"changed"`
	// Resumed is true when this call completed the approval and resumed the run.
	Resumed bool `table:"RESUMED" json:"resumed" yaml:"resumed"`
}

func stamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func decidedCount(a core.Approval) int { return len(a.Items) - len(a.Undecided()) }

func approvalRows(as []core.Approval) []approvalRow {
	rows := make([]approvalRow, len(as))
	for i, a := range as {
		rows[i] = approvalRow{
			ID: string(a.ID), Run: string(a.Run), Step: a.StepID, Status: string(a.Status),
			Decided: fmt.Sprintf("%d/%d", decidedCount(a), len(a.Items)), Title: a.Title, Task: a.TaskRef, Expires: stamp(a.ExpiresAt),
		}
	}
	return rows
}

func viewOf(a core.Approval) approvalView {
	v := approvalView{
		ID: string(a.ID), Run: string(a.Run), Step: a.StepID, Kind: string(a.Kind), Title: a.Title, Status: string(a.Status),
		Version: a.Version, Trust: a.Trust, Task: a.TaskRef, Assignee: a.Assignee, ExpiresAt: stamp(a.ExpiresAt),
		Waiting: len(a.Undecided()), Verdicts: []string{}, Items: make([]itemView, len(a.Items)),
	}
	for _, x := range a.Verdicts {
		v.Verdicts = append(v.Verdicts, string(x))
	}
	for i, it := range a.Items {
		iv := itemView{
			N: i + 1, ID: it.ID, Action: it.Action, Why: it.Why, Class: it.Class, Capability: it.Capability, Args: it.Args, Digest: it.Digest,
		}
		if d := it.Decision; d != nil {
			iv.Verdict, iv.By, iv.Reason, iv.Correction = string(d.Verdict), d.By, d.Reason, d.Correction
			iv.At = d.At.UTC().Format(time.RFC3339)
		}
		v.Items[i] = iv
	}
	return v
}

// addApproval registers the `flo approval` noun: list, show, set.
func addApproval(root *kitcli.Root, svc ServicesFunc) {
	ap := &cobra.Command{Use: "approval", Short: "approvals: what waits for a decision"}
	ap.AddCommand(approvalListCmd(root, svc), approvalShowCmd(root, svc), approvalSetCmd(root, svc))
	root.Cmd.AddCommand(ap)
}

func approvalListCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List approvals",
		Long: "List approvals, newest first: what a run parked on, how many items have a decision, " +
			"and the tlc task a person can close instead. --status keeps one state.",
		Args: cobra.NoArgs,
	}
	status := c.Flags().String("status", "", "only approvals in this state: open, decided or cancelled")
	run := c.Flags().String("run", "", "only approvals of this run")
	limit := c.Flags().Int("limit", 0, "at most this many; 0 is the store default")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		f := core.ApprovalFilter{Status: core.ApprovalStatus(*status), Run: core.RunID(*run), Limit: *limit}
		switch f.Status {
		case "", core.ApprovalOpen, core.ApprovalDecided, core.ApprovalCancelled:
		default:
			return core.Errorf(core.CodeUsage, "--status %q: want open, decided or cancelled", *status)
		}
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		as, err := s.ApprovalSvc.List(cmd.Context(), tenant, f)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, approvalRows(as))
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &[]approvalRow{}, Version: approvalSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "what waits for a decision", Command: "flo approval list --status open"},
		{Title: "one run, as JSON", Command: "flo approval list --run run_01 --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "an approval is open", Suggest: "flo approval show <id>", Reason: "see its items, reasons and trust"},
	})
	return c
}

func approvalShowCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "show <id>",
		Short: "Show an approval with its items",
		Long: "Show one approval: its items numbered from 1 (the N of `approval set --item N=...`), " +
			"the exact operation each would run, why it waits, its trust provider, and the decisions " +
			"made so far.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := core.ParseApprovalID(args[0])
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			s, tenant, err := accessServices(cmd, root, svc)
			if err != nil {
				return err
			}
			a, err := s.ApprovalSvc.Get(cmd.Context(), tenant, id)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return output.Dispatch(cmd, root.Viper, viewOf(a))
		},
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &approvalView{}, Version: approvalSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "show an approval", Command: "flo approval show ap_01"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "items wait", Suggest: "flo approval set <id> --all=approve --yes", Reason: "decide what is undecided"},
	})
	return c
}

// itemSpec is one `--item N=verdict`.
type itemSpec struct {
	ref     string
	verdict core.Verdict
}

// parseItemSpecs reads the --item values: N=approve|reject|correct. N is an
// item id, or its position from 1.
func parseItemSpecs(values []string) ([]itemSpec, error) {
	out := make([]itemSpec, 0, len(values))
	for _, v := range values {
		ref, verdict, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(ref) == "" {
			return nil, core.Errorf(core.CodeUsage, "--item %q: want N=approve|reject|correct", v)
		}
		out = append(out, itemSpec{ref: strings.TrimSpace(ref), verdict: core.Verdict(strings.TrimSpace(verdict))})
	}
	return out, nil
}

// settableVerdict reports a verdict a person may give.
func settableVerdict(v core.Verdict) bool {
	return v == core.VerdictApprove || v == core.VerdictReject || v == core.VerdictCorrect
}

// resolveItem maps N to an item id: an id first, else a position from 1.
func resolveItem(a core.Approval, ref string) (string, error) {
	if _, ok := a.Item(ref); ok {
		return ref, nil
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(a.Items) {
		return a.Items[n-1].ID, nil
	}
	return "", core.Errorf(core.CodeItemNotFound, "approval %s has no item %q", a.ID, ref).
		WithSuggestion("flo approval show " + string(a.ID))
}

func approvalSetCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "set <id>",
		Short: "Record decisions on an approval",
		Long: "Record item decisions: --item N=approve|reject|correct for a named item (N is its id or " +
			"its position from 1), --all=approve|reject for every item still undecided. --reason " +
			"applies to every decision of the call; --correction N=<json> gives the replacement of a " +
			"corrected item. An item that already has a decision keeps it, so the same request twice " +
			"yields the same state. The call that completes the approval closes its tlc task, reports " +
			"to trust through the flow and resumes the run. Without a terminal it needs --yes " +
			"(confirmation_required, exit 2); --dry-run shows what it would decide.",
		Args: cobra.ExactArgs(1),
	}
	items := c.Flags().StringArray("item", nil, "decide one item: N=approve|reject|correct (repeatable)")
	all := c.Flags().String("all", "", "decide every item still undecided: approve or reject")
	reason := c.Flags().String("reason", "", "why; recorded on every decision of this call")
	corrections := c.Flags().StringArray("correction", nil, "the replacement of a corrected item: N=<json> (repeatable)")
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		req, a, err := buildDecision(cmd, root, svc, args[0], *items, *all, *reason, *corrections)
		if err != nil {
			return err
		}
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		if kitcli.IsDryRun(cmd) {
			return kitcli.RenderPlan(cmd, decisionPlan(a, req))
		}
		if err := confirmWrite(*yes, fmt.Sprintf("record %s on approval %s?", describeDecision(req), a.ID)); err != nil {
			return err
		}
		res, err := s.ApprovalSvc.Decide(cmd.Context(), tenant, req)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, approvalDecision{
			ID: string(res.Approval.ID), Status: string(res.Approval.Status), Decided: decidedCount(res.Approval),
			Waiting: len(res.Approval.Undecided()), Changed: res.Changed, Resumed: res.Resumed,
		})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteShared)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &approvalDecision{}, Version: approvalSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "approve everything undecided", Command: "flo approval set ap_01 --all=approve --yes"},
		{Title: "decide items one by one", Command: "flo approval set ap_01 --item 1=approve --item 2=reject --reason spam --yes"},
		{Title: "preview", Command: "flo approval set ap_01 --all=approve --dry-run"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "items still wait", Suggest: "flo approval show <id>", Reason: "see what is left"},
		{When: "the run resumed", Suggest: "flo run show <run>", Reason: "follow the run"},
	})
	return c
}

// buildDecision turns the flags into a DecideRequest, reading the approval
// to resolve item positions.
func buildDecision(cmd *cobra.Command, root *kitcli.Root, svc ServicesFunc, idArg string, itemFlags []string, all, reason string, corrFlags []string) (core.DecideRequest, core.Approval, error) {
	id, err := core.ParseApprovalID(idArg)
	if err != nil {
		return core.DecideRequest{}, core.Approval{}, err //nolint:wrapcheck // *core.Error
	}
	specs, err := parseItemSpecs(itemFlags)
	if err != nil {
		return core.DecideRequest{}, core.Approval{}, err
	}
	if len(specs) == 0 && all == "" {
		return core.DecideRequest{}, core.Approval{}, core.New(core.CodeUsage, "nothing to decide").
			WithSuggestion("--item N=approve|reject|correct, or --all=approve")
	}
	for _, sp := range specs {
		if !settableVerdict(sp.verdict) {
			return core.DecideRequest{}, core.Approval{}, core.Errorf(core.CodeInvalidDecision, "--item %s=%s: want approve, reject or correct", sp.ref, sp.verdict)
		}
	}
	if all != "" && !settableVerdict(core.Verdict(all)) {
		return core.DecideRequest{}, core.Approval{}, core.Errorf(core.CodeInvalidDecision, "--all=%s: want approve or reject", all)
	}
	s, tenant, err := accessServices(cmd, root, svc)
	if err != nil {
		return core.DecideRequest{}, core.Approval{}, err
	}
	caller, err := accessCaller(root)
	if err != nil {
		return core.DecideRequest{}, core.Approval{}, err
	}
	a, err := s.ApprovalSvc.Get(cmd.Context(), tenant, id)
	if err != nil {
		return core.DecideRequest{}, core.Approval{}, err //nolint:wrapcheck // *core.Error
	}
	corrections, err := parseCorrections(a, corrFlags)
	if err != nil {
		return core.DecideRequest{}, a, err
	}
	req := core.DecideRequest{Approval: id, All: core.Verdict(all), AllReason: reason, By: caller}
	for _, sp := range specs {
		itemID, err := resolveItem(a, sp.ref)
		if err != nil {
			return core.DecideRequest{}, a, err
		}
		iv := core.ItemVerdict{Item: itemID, Verdict: sp.verdict, Reason: reason}
		if corr, ok := corrections[itemID]; ok {
			if sp.verdict != core.VerdictCorrect {
				return core.DecideRequest{}, a, core.Errorf(core.CodeUsage, "--correction for item %s, which is %s, not correct", itemID, sp.verdict)
			}
			iv.Correction = corr
		}
		req.Items = append(req.Items, iv)
	}
	for itemID := range corrections {
		if !slicesHas(req.Items, itemID) {
			return core.DecideRequest{}, a, core.Errorf(core.CodeUsage, "--correction for item %s, which --item does not decide", itemID)
		}
	}
	return req, a, nil
}

func slicesHas(items []core.ItemVerdict, id string) bool {
	for _, iv := range items {
		if iv.Item == id {
			return true
		}
	}
	return false
}

// parseCorrections reads --correction N=<json> by item id.
func parseCorrections(a core.Approval, values []string) (map[string]any, error) {
	out := map[string]any{}
	for _, v := range values {
		ref, raw, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(ref) == "" {
			return nil, core.Errorf(core.CodeUsage, "--correction %q: want N=<json>", v)
		}
		id, err := resolveItem(a, strings.TrimSpace(ref))
		if err != nil {
			return nil, err
		}
		val, err := core.DecodeJSONValue([]byte(raw))
		if err != nil {
			return nil, core.Errorf(core.CodeUsage, "--correction %s: not JSON: %w", ref, err)
		}
		out[id] = val
	}
	return out, nil
}

func describeDecision(req core.DecideRequest) string {
	var parts []string
	for _, iv := range req.Items {
		parts = append(parts, iv.Item+"="+string(iv.Verdict))
	}
	if req.All != "" {
		parts = append(parts, "all="+string(req.All))
	}
	return strings.Join(parts, " ")
}

// decisionPlan shows what a decision would do, without writing: the items it
// decides, the ones that keep their decision, and what completing the
// approval would set off.
func decisionPlan(a core.Approval, req core.DecideRequest) kitcli.Plan {
	plan := kitcli.Plan{
		Args:    map[string]any{"approval": string(a.ID), "by": req.By, "decision": describeDecision(req)},
		Effects: []kitcli.Effect{},
	}
	will := map[string]core.Verdict{}
	for _, iv := range req.Items {
		will[iv.Item] = iv.Verdict
	}
	if req.All != "" {
		for _, i := range a.Undecided() {
			if _, named := will[a.Items[i].ID]; !named {
				will[a.Items[i].ID] = req.All
			}
		}
	}
	waiting := 0
	for _, it := range a.Items {
		v, named := will[it.ID]
		switch {
		case !named && !it.Decided():
			waiting++
		case it.Decided() && named:
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("item %s keeps its decision (%s)", it.ID, it.Decision.Verdict))
		case named:
			plan.Effects = append(plan.Effects, kitcli.Effect{
				Kind: "decide", Target: "item:" + string(a.ID) + "/" + it.ID, Reversible: false, Detail: string(v),
			})
		}
	}
	if a.Status == core.ApprovalCancelled {
		plan.Warnings = append(plan.Warnings, "the approval was cancelled with its run: the write would be refused (approval_closed)")
	}
	if len(plan.Effects) > 0 && waiting == 0 {
		plan.Effects = append(plan.Effects, kitcli.Effect{Kind: "resume", Target: "run:" + string(a.Run), Reversible: false, Detail: "the last item completes the approval"})
		if a.TaskRef != "" {
			plan.Effects = append(plan.Effects, kitcli.Effect{Kind: "close", Target: "task:" + a.TaskRef, Reversible: true})
		}
	}
	return plan
}
