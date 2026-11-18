package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
)

const triggerSchemaVersion = "1.0"

// triggerRow is one trigger registration as `flo trigger list` prints it.
type triggerRow struct {
	ID      string `table:"ID" json:"id" yaml:"id"`
	Flow    string `table:"FLOW" json:"flow" yaml:"flow"`
	Version string `table:"VERSION" json:"version" yaml:"version"`
	Kind    string `table:"KIND" json:"kind" yaml:"kind"`
	On      string `table:"ON" json:"on" yaml:"on"`
	State   string `table:"STATE" json:"state" yaml:"state"`
	Next    string `table:"NEXT" json:"next_at,omitempty" yaml:"next_at,omitempty"`
}

// triggerOn summarizes what a trigger fires on: the recurrence, or the
// source node of a calendar or event trigger.
func triggerOn(t core.TriggerRecord) string {
	switch {
	case t.Spec.RRule != "":
		if t.Spec.TZ != "" {
			return t.Spec.RRule + " " + t.Spec.TZ
		}
		return t.Spec.RRule
	case t.Spec.Source != "":
		return t.Spec.Source
	}
	return ""
}

func triggerRows(ts []core.TriggerRecord) []triggerRow {
	rows := make([]triggerRow, len(ts))
	for i, t := range ts {
		rows[i] = triggerRow{
			ID: string(t.ID), Flow: t.Flow, Version: t.Version, Kind: string(t.Spec.Kind), On: triggerOn(t),
			State: string(t.State), Next: stamp(t.NextAt),
		}
	}
	return rows
}

// addTrigger registers the `flo trigger` noun: list.
func addTrigger(root *kitcli.Root, svc ServicesFunc) {
	t := &cobra.Command{Use: "trigger", Short: "triggers: what starts runs, and when next"}
	t.AddCommand(triggerListCmd(root, svc))
	root.Cmd.AddCommand(t)
}

func triggerListCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List triggers with the next fire of each",
		Long: "List the trigger registrations of the active flows, ordered by flow: the declaration, its " +
			"state (active or paused) and, for a schedule, the next fire. --flow keeps one flow.",
		Args: cobra.NoArgs,
	}
	flow := c.Flags().String("flow", "", "only the triggers of this flow")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		ts, err := s.Triggers.ListTriggers(cmd.Context(), tenant, *flow)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, triggerRows(ts))
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &[]triggerRow{}, Version: triggerSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "next fires", Command: "flo trigger list"},
		{Title: "one flow, as JSON", Command: "flo trigger list --flow dependency-watch --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "a trigger is listed", Suggest: "flo run list --flow <flow>", Reason: "see the runs it started"},
	})
	return c
}
