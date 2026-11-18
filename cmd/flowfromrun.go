package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

// fromRunFlags are the flags of `flo flow test --from-run`.
type fromRunFlags struct {
	run            *string
	writeExpect    *bool
	allowLocalData *bool
}

func addFromRunFlags(c *cobra.Command) fromRunFlags {
	f := fromRunFlags{
		run:            c.Flags().String("from-run", "", "make the test from a recorded run (`flo run create --record`): copy its cassettes, trigger payload and starting state into record/ of the bundle, then replay"),
		writeExpect:    c.Flags().Bool("write-expect", false, "with --from-run, also write tests/from-<run>.yaml: an expect: entry for each step, with the status it ended with"),
		allowLocalData: c.Flags().Bool("allow-local-data", false, "with --from-run, copy the recording of a local-only flow into the bundle (a bundle may be committed or shared)"),
	}
	c.MarkFlagsMutuallyExclusive("from-run", "record")
	c.MarkFlagsMutuallyExclusive("from-run", "replay")
	return f
}

// fromRunArgs is what runFromRun needs of the command that parsed the flags.
type fromRunArgs struct {
	flow, version, state string
	trigger              map[string]any
	vars                 map[string]string
	timeout              time.Duration
	yes                  bool
}

// runFromRun is `flo flow test <flow> --from-run <run>`.
func runFromRun(cmd *cobra.Command, root *kitcli.Root, eng EngineFunc, f fromRunFlags, a fromRunArgs) error {
	id, err := core.ParseRunID(*f.run)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	var seed map[string]any
	switch a.state {
	case "":
	case flowtest.StateDefault, "current":
		return core.Errorf(core.CodeUsage, "--state %s: with --from-run the starting state is the run's; give a seed file to supply it again", a.state)
	default:
		if seed, err = flowtest.ReadSeedFile(a.state); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
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
	if len(a.vars) == 0 {
		a.vars = nil
	}
	req := serve.FromRunRequest{
		Flow: a.flow, Run: id, Ref: a.version, Caller: caller, Trigger: a.trigger, Vars: a.vars, State: seed,
		WriteExpect: *f.writeExpect, AllowLocalData: *f.allowLocalData, Timeout: a.timeout,
	}
	plan, err := e.PlanFromRun(cmd.Context(), tenant, req)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if kitcli.IsDryRun(cmd) {
		return kitcli.RenderPlan(cmd, fromRunPlan(plan, caller))
	}
	q := fmt.Sprintf("copy run %s into %s (%d cassette files, trigger payload, starting state%s); the bundle's digest changes?",
		id, plan.Bundle, plan.Cassettes, map[bool]string{true: ", tests/ skeleton", false: ""}[req.WriteExpect])
	if len(plan.Warnings) > 0 {
		q += "\n  - " + strings.Join(plan.Warnings, "\n  - ") + "\n"
	}
	if err := confirmWrite(a.yes, q); err != nil {
		return err
	}
	res, err := e.FromRun(cmd.Context(), tenant, req)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if err := output.Dispatch(cmd, root.Viper, res); err != nil {
		return err //nolint:wrapcheck // kit output error
	}
	return testFailure(res.Test)
}

// fromRunPlan is the Plan a --dry-run prints: nothing is copied or run.
func fromRunPlan(p serve.FromRunPlan, caller string) kitcli.Plan {
	effects := []kitcli.Effect{}
	cassettes := 0
	for _, f := range p.Files {
		if strings.HasPrefix(f.Path, "record/cassettes/") {
			cassettes++
			continue
		}
		effects = append(effects, kitcli.Effect{Kind: "write", Target: f.Path + " of the flow's bundle", Reversible: true, Detail: f.Action})
	}
	effects = append([]kitcli.Effect{{
		Kind: "write", Target: "record/cassettes/ of the flow's bundle", Reversible: true, Detail: fmt.Sprintf("%d files", cassettes),
	}}, effects...)
	effects = append(effects, kitcli.Effect{Kind: "store", Target: "test-result:" + p.Flow + "@" + p.Hash, Reversible: true, Detail: "the replay of the bundle, which activation reads"})
	return kitcli.Plan{
		Args: map[string]any{
			"flow": p.Flow, "version": p.Version, "hash": p.Hash, "run": p.Run, "bundle": p.Bundle, "privacy": p.Privacy, "by": caller,
			"files": p.Files, "warnings": p.Warnings,
		},
		Effects: effects,
	}
}
