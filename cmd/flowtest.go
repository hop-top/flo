package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

func flowTestCmd(root *kitcli.Root, svc ServicesFunc, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "test <flow>",
		Short: "Test a stored flow version on recordings, or record one",
		Long: "Run a stored version of a flow once in a sandbox and say whether it passes: the run completes " +
			"with no step error, every output matches its node spec, every check: passes (eva must be " +
			"reachable, or have a recording) and every expect: block of tests/ holds. Nothing reaches the " +
			"real stores, and every node that writes shared state or cannot be undone is not executed: its " +
			"plan is recorded instead. An approval is auto-approved.\n\n" +
			"By default (--replay) processes and requests are answered from the cassettes in record/ of the " +
			"flow's bundle, so the test is deterministic and offline; a call with no cassette fails the test. " +
			"--record makes the calls for real (reads and pure nodes only) and refuses when a declared command, http or plugin node with side_effect read or write-local would reach real systems, unless --allow-live (point the node's profile env at temp dirs first; --dry-run lists those nodes). It writes the cassettes, the trigger " +
			"payload and the plans of the skipped writes to record/, scrubbed as a run recording is (node redact paths, secrets, credential headers, env values), and records the result. Re-recording " +
			"overwrites the cassettes of the same calls.\n\n" +
			"--state picks the flow state the run starts from: default (declared defaults), current (a " +
			"read-only copy of the engine's committed state; nothing is written back) or a JSON/YAML seed file " +
			"(keys must be declared, values of the default's kind). A tests/ document with a `state:` map runs " +
			"the flow on that state and wins. --record writes the seed used to record/state.json, which a " +
			"replay uses unless --state is given. --dry-run prints the plan (seed, cassettes, planned writes) " +
			"and creates nothing.\n\n" +
			"--from-run <run> makes the test from a run recorded with `flo run create --record`: its " +
			"cassettes, trigger payload and starting state are copied into record/ of the flow's bundle " +
			"(--write-expect also writes tests/from-<run>.yaml, an expect: entry per step with the status " +
			"it ended with), then the bundle is replayed. It writes the bundle, so it asks for confirmation " +
			"(--yes, or a terminal) and --dry-run lists what would be written; the bundle's digest changes, " +
			"so the version is tested again before it is activated. It refuses a recording that is " +
			"unfinished or partial, of a run that did not succeed, of another version than the one to " +
			"write (--version names the recorded one), and of a local-only flow unless --allow-local-data " +
			"(a bundle may be committed or shared). Inputs a node's redact paths dropped are never copied: " +
			"supply them again with --trigger-file, --var/--var-file or --state <seed file>.\n\n" +
			"The latest result of a version is what `flo flow set --state active` looks at: activation needs a " +
			"passing test of that exact version, bundle files included. Apply the version first; the test " +
			"takes the newest stored version, or --version. Exits 1 when the test fails.",
		Args: cobra.ExactArgs(1),
	}
	ver := c.Flags().String("version", "", "a version: its hash or its document version; default the newest")
	record := c.Flags().Bool("record", false, "make the reads for real and record them (cassettes, trigger payload, plans) in record/ of the bundle")
	c.Flags().Bool("replay", false, "answer calls from the recordings (the default)")
	vars := c.Flags().StringArray("var", nil, "bind a flow var: name=value (repeatable)")
	varFile := c.Flags().String("var-file", "", "a JSON object of vars; --var wins over it")
	trigFile := c.Flags().String("trigger-file", "", "a JSON object read as the trigger payload (trigger.<key> in expressions); - reads standard input; default the recorded one")
	state := c.Flags().String("state", "", "flow state to start from: default (declared defaults), current (read-only copy of the engine's state) or a JSON/YAML seed file; default: record/state.json on replay, else the declared defaults")
	timeout := c.Flags().Duration("timeout", serve.DefaultTestTimeout, "give up after this long")
	yes := c.Flags().Bool("yes", false, "with --record or --from-run, confirm without asking")
	allowLive := c.Flags().Bool("allow-live", false, "with --record, allow declared command/http/plugin nodes (side_effect read or write-local) to run against real systems")
	fromRun := addFromRunFlags(c)
	c.MarkFlagsMutuallyExclusive("record", "replay")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		mode := flowtest.ModeReplay
		if *record {
			mode = flowtest.ModeRecord
		}
		provided, err := parseVars(*vars, *varFile)
		if err != nil {
			return err
		}
		payload, err := readTriggerFile(cmd.InOrStdin(), *trigFile)
		if err != nil {
			return err
		}
		if *fromRun.run != "" {
			return runFromRun(cmd, root, eng, fromRun, fromRunArgs{
				flow: args[0], version: *ver, state: *state, trigger: payload, vars: provided, timeout: *timeout, yes: *yes,
			})
		}
		if *fromRun.writeExpect || *fromRun.allowLocalData {
			return core.New(core.CodeUsage, "--write-expect and --allow-local-data go with --from-run")
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
		treq := serve.TestRequest{
			Flow: args[0], Ref: *ver, Mode: mode, Caller: caller, Trigger: payload, Vars: provided, State: *state, Timeout: *timeout, AllowLive: *allowLive,
		}
		if kitcli.IsDryRun(cmd) {
			plan, err := e.PlanTest(cmd.Context(), tenant, treq)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return kitcli.RenderPlan(cmd, testPlan(plan, caller))
		}
		if mode == flowtest.ModeRecord {
			if !*allowLive { // refuse before asking: nothing has run yet
				plan, err := e.PlanTest(cmd.Context(), tenant, treq)
				if err != nil {
					return err //nolint:wrapcheck // *core.Error
				}
				if err := serve.LiveRefusal(plan.Live); err != nil {
					return err //nolint:wrapcheck // *core.Error
				}
			}
			if err := confirmWrite(*yes, fmt.Sprintf("record a test of flow %s (reads run for real; record/ of its bundle is written)?", args[0])); err != nil {
				return err
			}
		}
		res, err := e.TestFlow(cmd.Context(), tenant, treq)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if err := output.Dispatch(cmd, root.Viper, res); err != nil {
			return err //nolint:wrapcheck // kit output error
		}
		return testFailure(res)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteLocal)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &flowtest.Result{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "test on the recordings", Command: "flo flow test dependency-watch"},
		{Title: "record the reads, then test", Command: "flo flow test dependency-watch --record --yes"},
		{Title: "turn a recorded run into a test", Command: "flo flow test dependency-watch --from-run run_01 --write-expect --yes"},
		{Title: "see what that would write", Command: "flo flow test dependency-watch --from-run run_01 --dry-run"},
		{Title: "feed a trigger payload", Command: "flo flow test meeting-brief --record --trigger-file event.json --yes"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the test passed", Suggest: "flo flow set <flow> --state active", Reason: "activation needs a passing test of this version"},
		{When: "a call has no recording", Suggest: "flo flow test <flow> --record", Reason: "record the reads for real"},
	})
	return c
}

// testPlan is the Plan a --dry-run prints: nothing is run or written.
func testPlan(p serve.TestPlan, caller string) kitcli.Plan {
	effects := []kitcli.Effect{{Kind: "store", Target: "test-result:" + p.Flow + "@" + p.Hash, Reversible: true, Detail: "the result of this test, which activation reads"}}
	for _, w := range p.Writes {
		effects = append(effects, kitcli.Effect{Kind: "plan", Target: "step:" + w.Step, Reversible: true, Detail: w.Node + " (" + w.SideEffect + ") is planned, not run"})
	}
	for _, n := range p.Live {
		effects = append(effects, kitcli.Effect{Kind: "live", Target: "step:" + n.Step, Reversible: false, Detail: n.Node + " (" + n.Kind + ", " + n.SideEffect + ") runs for real: refused without --allow-live"})
	}
	if p.CassetteAction == "record" {
		effects = append(effects, kitcli.Effect{Kind: "write", Target: "record/ of the flow's bundle", Reversible: true, Detail: "cassettes, trigger payload, state seed and the plans of skipped writes"})
	}
	return kitcli.Plan{
		Args: map[string]any{
			"flow": p.Flow, "version": p.Version, "hash": p.Hash, "mode": p.Mode, "by": caller,
			"state_source": p.StateSource, "seed": p.Seed, "cassette_action": p.CassetteAction, "cassette_dir": p.CassetteDir,
			"cassettes": p.Cassettes, "planned_writes": p.Writes, "live_nodes": p.Live, "cases": p.Cases,
		},
		Effects: effects,
	}
}

// testFailure is the error of a test that did not pass, nil for one that did.
// The first problem decides the code, so a failing check exits as one.
func testFailure(res flowtest.Result) error {
	if res.Passed {
		return nil
	}
	code, msg := core.CodeRunFailed, "no reason recorded"
	if len(res.Problems) > 0 {
		p := res.Problems[0]
		msg = p.Message
		if c := core.Code(p.Code); c.Known() {
			code = c
		}
		if n := len(res.Problems) - 1; n > 0 {
			msg += fmt.Sprintf(" (and %d more)", n)
		}
	}
	return core.Errorf(code, "flow %s@%s failed its test: %s", res.Flow, res.Version, msg).
		WithDetail("problems", res.Problems).
		WithSuggestion("fix the flow, or `flo flow test " + res.Flow + " --record` after a change in what it reads")
}
