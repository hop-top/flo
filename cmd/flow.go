package cmd

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/spec/cel"
)

const flowSchemaVersion = "1.0"

// flowRow is one flow as `flo flow list` prints it: the version new runs
// start on, else the newest stored.
type flowRow struct {
	Flow    string `table:"FLOW" json:"flow" yaml:"flow"`
	Version string `table:"VERSION" json:"version" yaml:"version"`
	State   string `table:"STATE" json:"state" yaml:"state"`
	Hash    string `table:"HASH" json:"hash" yaml:"hash"`
	Updated string `table:"UPDATED" json:"updated" yaml:"updated"`
}

// flowVersionView is a stored flow version.
type flowVersionView struct {
	Flow        string `json:"flow" yaml:"flow"`
	Version     string `json:"version" yaml:"version"`
	Hash        string `json:"hash" yaml:"hash"`
	Seq         int    `json:"seq" yaml:"seq"`
	State       string `json:"state" yaml:"state"`
	Bundle      string `json:"bundle,omitempty" yaml:"bundle,omitempty"`
	CreatedAt   string `json:"created_at" yaml:"created_at"`
	ActivatedAt string `json:"activated_at,omitempty" yaml:"activated_at,omitempty"`
}

// flowShow is `flo flow show`: the version and its document.
type flowShow struct {
	flowVersionView `yaml:",inline"`
	Versions        []flowVersionView `json:"versions" yaml:"versions"`
	Source          string            `json:"source" yaml:"source"`
}

// flowApplied is what `flo flow apply` reports.
type flowApplied struct {
	flowVersionView `yaml:",inline"`
	// Changed is false when the same content was already stored: the same
	// bytes are the same version.
	Changed  bool          `json:"changed" yaml:"changed"`
	Warnings []serve.Issue `json:"warnings" yaml:"warnings"`
}

// flowSet is what `flo flow set` reports.
type flowSet struct {
	flowVersionView `yaml:",inline"`
	Changed         bool `json:"changed" yaml:"changed"`
}

func stampTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func flowVersionOf(v core.FlowVersion) flowVersionView {
	out := flowVersionView{
		Flow: v.Name, Version: v.Version, Hash: v.Hash, Seq: v.Seq, State: string(v.State), Bundle: v.Bundle,
		CreatedAt: stampTime(v.CreatedAt),
	}
	if v.ActivatedAt != nil {
		out.ActivatedAt = stampTime(*v.ActivatedAt)
	}
	return out
}

func flowRows(fs []core.FlowSummary) []flowRow {
	rows := make([]flowRow, len(fs))
	for i, s := range fs {
		v, state := s.Latest, string(s.Latest.State)
		switch {
		case s.Active != nil:
			v, state = *s.Active, "active"
		case s.Paused:
			state = "paused"
		}
		rows[i] = flowRow{Flow: s.Name, Version: v.Version, State: state, Hash: v.Hash, Updated: stampTime(v.CreatedAt)}
	}
	return rows
}

// addFlow registers the `flo flow` noun: apply, list, show, validate, test, set.
func addFlow(root *kitcli.Root, svc ServicesFunc, eng EngineFunc) {
	f := &cobra.Command{Use: "flow", Short: "flows: documents, versions, activation"}
	f.AddCommand(
		flowApplyCmd(root, svc, eng), flowListCmd(root, svc), flowShowCmd(root, svc),
		flowValidateCmd(root, svc, eng), flowTestCmd(root, svc, eng), flowSetCmd(root, svc, eng),
	)
	root.Cmd.AddCommand(f)
}

// flowOps opens the engine's flow operations.
func flowOps(cmd *cobra.Command, eng EngineFunc) (*serve.Engine, error) {
	if eng == nil {
		return nil, core.New(core.CodeEngineUnreachable, "the engine is not wired in this build")
	}
	return eng(cmd.Context())
}

func flowFileFlag(c *cobra.Command) *string {
	f := c.Flags().StringP("file", "f", "", "the flow: a file, or a bundle directory holding flow.yaml and nodes/")
	_ = c.MarkFlagRequired("file")
	return f
}

func flowApplyCmd(root *kitcli.Root, svc ServicesFunc, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "apply",
		Short: "Validate a flow and store it as a new version",
		Long: "Read the flow, validate it (document, expressions, node specs, privacy, irreversible " +
			"guards) and check that you may use its run_as and that profile holds every capability its " +
			"nodes declare. A new version is stored, as a draft, only when the content changed: " +
			"applying the same bytes again changes nothing. Without --yes it asks for confirmation at a " +
			"terminal, and fails with confirmation_required without one. Test it with `flo flow test`, " +
			"then activate it with `flo flow set`.",
		Args: cobra.NoArgs,
	}
	file := flowFileFlag(c)
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		src, err := serve.ReadSource(*file)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		caller, err := accessCaller(root)
		if err != nil {
			return err
		}
		if kitcli.IsDryRun(cmd) {
			ck, err := e.Flows.Check(cmd.Context(), tenant, src)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			if err := ck.Report.Err(); err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return kitcli.RenderPlan(cmd, kitcli.Plan{
				Args: map[string]any{"flow": ck.Flow.Name, "version": ck.Flow.Version, "hash": ck.Flow.Hash, "run_as": ck.Flow.RunAs},
				Effects: []kitcli.Effect{{
					Kind: "store", Target: "flow:" + ck.Flow.Name + "@" + ck.Flow.Hash, Reversible: true, Detail: "a new draft version when the content is new",
				}},
			})
		}
		if err := confirmWrite(*yes, fmt.Sprintf("store flow %s as a new version?", flowNameOf(src))); err != nil {
			return err
		}
		res, err := e.Flows.Apply(cmd.Context(), tenant, caller, src)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		warn := []serve.Issue{}
		for _, i := range res.Report.Issues {
			if i.Severity != "error" {
				warn = append(warn, i)
			}
		}
		return output.Dispatch(cmd, root.Viper, flowApplied{flowVersionView: flowVersionOf(res.Version), Changed: res.Created, Warnings: warn})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteLocal)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &flowApplied{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "apply a bundle", Command: "flo flow apply -f flows/dependency-watch --yes"},
		{Title: "preview", Command: "flo flow apply -f flows/dependency-watch/flow.yaml --dry-run"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the version is a draft", Suggest: "flo flow test <flow>", Reason: "activation needs a passing test of this version"},
	})
	return c
}

// flowNameOf is the name a source's document declares, for a question to the
// person; the path when it does not parse (the apply that follows reports why).
func flowNameOf(src serve.Source) string {
	if f, err := cel.ParseFlow(bytes.NewReader(src.Data), filepath.Base(src.File)); err == nil {
		return f.Name
	}
	return src.File
}

func flowValidateCmd(root *kitcli.Root, svc ServicesFunc, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate a flow without storing it",
		Long: "Report every finding of the validation rules: document and expression errors, node specs " +
			"the flow cannot resolve, privacy, irreversible steps without an approval, and warnings. " +
			"Exits 2 (invalid_flow) when there is an error; the report is printed either way.",
		Args: cobra.NoArgs,
	}
	file := flowFileFlag(c)
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		src, err := serve.ReadSource(*file)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		ck, err := e.Flows.Check(cmd.Context(), tenant, src)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if err := output.Dispatch(cmd, root.Viper, ck.Report); err != nil {
			return err //nolint:wrapcheck // kit output error
		}
		return ck.Report.Err()
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &serve.Report{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "validate a flow", Command: "flo flow validate -f flows/dependency-watch --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the flow is valid", Suggest: "flo flow apply -f <flow>", Reason: "store it as a version"},
	})
	return c
}

func flowListCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List flows",
		Long:  "List flows by name with the version new runs start on (else the newest stored) and its state: active, paused or draft.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, tenant, err := accessServices(cmd, root, svc)
			if err != nil {
				return err
			}
			fs, err := s.Flows.ListFlows(cmd.Context(), tenant)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return output.Dispatch(cmd, root.Viper, flowRows(fs))
		},
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &[]flowRow{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "list flows as JSON", Command: "flo flow list --format json"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "a flow is listed", Suggest: "flo flow show <flow>", Reason: "see its versions and document"}})
	return c
}

func flowShowCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "show <flow>",
		Short: "Show a flow: a version, its document and the version history",
		Long: "Show one version of a flow (the active one, else the newest; --version names a hash or " +
			"document version), its raw document and every stored version, newest first.",
		Args: cobra.ExactArgs(1),
	}
	ver := c.Flags().String("version", "", "a version: its hash or its document version; default the active, else the newest")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		vs, err := s.Flows.ListVersions(cmd.Context(), tenant, args[0])
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if len(vs) == 0 {
			return core.Errorf(core.CodeFlowNotFound, "flow %q not found", args[0]).WithSuggestion("flo flow list")
		}
		pick := vs[0]
		if *ver == "" {
			for _, v := range vs {
				if v.State == core.FlowActive {
					pick = v
					break
				}
			}
		} else if pick, err = s.Flows.GetVersion(cmd.Context(), tenant, args[0], *ver); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		out := flowShow{flowVersionView: flowVersionOf(pick), Source: string(pick.Source), Versions: make([]flowVersionView, len(vs))}
		for i, v := range vs {
			out.Versions[i] = flowVersionOf(v)
		}
		return output.Dispatch(cmd, root.Viper, out)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &flowShow{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "show the active version", Command: "flo flow show dependency-watch --format json"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "the flow is active", Suggest: "flo run create --flow <flow>", Reason: "start a run"}})
	return c
}

func flowSetCmd(root *kitcli.Root, svc ServicesFunc, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "set <flow>",
		Short: "Activate or pause a flow",
		Long: "Set the state of a flow: active makes a version the one new runs start on (the newest, or " +
			"--version) and registers its triggers; paused stops its triggers while its runs continue. " +
			"Activating validates the version again, checks that you may use its run_as and that profile " +
			"holds the capabilities its nodes declare, and runs the activation gate: the version must have " +
			"a passing `flo flow test` of that exact version, bundle files included (flow_not_tested " +
			"otherwise). Setting the state a version has is a no-op.",
		Args: cobra.ExactArgs(1),
	}
	state := c.Flags().String("state", "", "active or paused")
	ver := c.Flags().String("version", "", "the version to set: its hash or its document version; default the newest (active) or the live one (paused)")
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	_ = c.MarkFlagRequired("state")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		st := core.FlowState(*state)
		if st != core.FlowActive && st != core.FlowPaused {
			return core.Errorf(core.CodeUsage, "--state %q: want active or paused", *state)
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		caller, err := accessCaller(root)
		if err != nil {
			return err
		}
		if kitcli.IsDryRun(cmd) {
			return kitcli.RenderPlan(cmd, kitcli.Plan{
				Args:    map[string]any{"flow": args[0], "state": string(st), "version": *ver, "by": caller},
				Effects: []kitcli.Effect{{Kind: "set-state", Target: "flow:" + args[0], Reversible: true, Detail: string(st)}},
			})
		}
		if err := confirmWrite(*yes, fmt.Sprintf("set flow %s %s?", args[0], st)); err != nil {
			return err
		}
		res, err := e.Flows.SetState(cmd.Context(), tenant, caller, args[0], strings.TrimSpace(*ver), st)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, flowSet{flowVersionView: flowVersionOf(res.Version), Changed: res.Changed})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteShared)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &flowSet{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "activate a tested version", Command: "flo flow set dependency-watch --state active --yes"},
		{Title: "pause", Command: "flo flow set dependency-watch --state paused --yes"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the flow is active", Suggest: "flo trigger list", Reason: "see when it fires next"},
	})
	return c
}
