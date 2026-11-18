package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/brief"
)

// addBrief registers the `flo brief` noun: show.
func addBrief(root *kitcli.Root, svc ServicesFunc) {
	b := &cobra.Command{Use: "brief", Short: "brief: what flows found and what waits for you"}
	b.AddCommand(briefShowCmd(root, svc))
	root.Cmd.AddCommand(b)
}

func briefShowCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "show",
		Short: "Show findings and waiting approvals across flows",
		Long: "Show what flows published (findings and briefs from flo/output@1 steps) and the " +
			"approvals that wait for a decision, grouped by flow. Outputs are kept 90 days; " +
			"--since narrows the window (a duration such as 36h or 7d, an RFC 3339 time or a " +
			"date) and --flow keeps one flow. Open approvals always show, whatever their age. " +
			"--format json is a stable document with schema_version.",
		Args: cobra.NoArgs,
	}
	since := c.Flags().String("since", "", "only outputs from then on: 36h, 7d, 2026-11-01 or an RFC 3339 time (default: all retained)")
	flow := c.Flags().String("flow", "", "only this flow")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		from, err := brief.ParseSince(*since, s.Clock.Now())
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		b, err := brief.Build(cmd.Context(), brief.Deps{Outputs: s.Outputs, Runs: s.Runs, Approvals: s.ApprovalSvc, Clock: s.Clock},
			tenant, brief.Options{Since: from, Flow: *flow})
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if humanOutput(cmd) {
			return brief.WriteText(cmd.OutOrStdout(), b)
		}
		return output.Dispatch(cmd, root.Viper, b)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &brief.Brief{}, Version: brief.SchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "what ran, what was found, what waits", Command: "flo brief show"},
		{Title: "since yesterday, one flow, as JSON", Command: "flo brief show --since 24h --flow dependency-watch --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "approvals wait", Suggest: "flo approval show <id>", Reason: "see their items, reasons and trust"},
		{When: "a brief is ready", Suggest: "flo run show <run>", Reason: "read the run that published it"},
	})
	return c
}

// humanOutput reports that the caller wants the default, human rendering: no
// --format (or table) and no --output file.
func humanOutput(cmd *cobra.Command) bool {
	if f := cmd.Flag("output"); f != nil && f.Value.String() != "" && f.Value.String() != "-" {
		return false
	}
	f := cmd.Flag("format")
	return f == nil || f.Value.String() == "" || f.Value.String() == output.Table
}
