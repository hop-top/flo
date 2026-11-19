package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/builder"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// draftRow is one draft as `flo flow draft list` prints it.
type draftRow struct {
	ID      string `table:"DRAFT" json:"draft" yaml:"draft"`
	Flow    string `table:"FLOW" json:"flow" yaml:"flow"`
	Version string `table:"VERSION" json:"version" yaml:"version"`
	Base    string `table:"BASE" json:"base" yaml:"base"`
	Model   string `table:"MODEL" json:"model" yaml:"model"`
	DryRun  string `table:"DRY RUN" json:"dry_run" yaml:"dry_run"`
	Created string `table:"CREATED" json:"created_at" yaml:"created_at"`
	Request string `table:"INSTRUCTION" json:"instruction" yaml:"instruction"`
}

func dryRunWord(d *builder.DryRun) string {
	switch {
	case d == nil || (!d.Ran && d.Run == ""):
		return "none"
	case !d.Ran:
		return "skipped"
	case d.Matches:
		return "matches"
	}
	return "differs"
}

func draftRows(ds []builder.Draft) []draftRow {
	rows := make([]draftRow, len(ds))
	for i, d := range ds {
		rows[i] = draftRow{
			ID: d.ID, Flow: d.Flow, Version: d.Version, Base: d.Base.Version, Model: d.Model,
			DryRun: dryRunWord(d.DryRun), Created: stampTime(d.CreatedAt), Request: d.Instruction,
		}
	}
	return rows
}

// draftApplied is what `flo flow draft apply` reports: the stored version, and
// the proposal replayed on the last recording once more.
type draftApplied struct {
	Draft       string `json:"draft" yaml:"draft"`
	flowApplied `yaml:",inline"`
	DryRun      *builder.DryRun `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
	// Overridden are the blocking differences --force went over.
	Overridden []string `json:"overridden,omitempty" yaml:"overridden,omitempty"`
	// Written is the file --write wrote.
	Written string `json:"written,omitempty" yaml:"written,omitempty"`
}

// addFlowDraft registers `flo flow draft`: create, show, list, apply, discard.
func addFlowDraft(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "draft",
		Short: "chat builder: a model proposes the next version of a flow",
		Long: "The chat builder. `create` gives a model an instruction and the current flow and gets the full " +
			"next version back; validation errors go back to the model up to three times; a valid proposal is " +
			"replayed on the flow's last recording, offline, and the difference in outcome is reported. The " +
			"result is a draft, kept until it is applied or discarded; it is not a flow version. `apply` stores " +
			"it as the next version, only if the flow has not moved since (draft_stale otherwise), then test " +
			"and activate it as any version.",
	}
	kitcli.SetHierarchical(c)
	c.AddCommand(
		draftCreateCmd(root, eng), draftShowCmd(root, eng), draftListCmd(root, eng),
		draftApplyCmd(root, eng), draftDiscardCmd(root, eng),
	)
	return c
}

func draftCreateCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "create <flow> [instruction...] | --new <name> <instruction...> | --from <draft> <instruction...>",
		Short: "Ask a model for the next version of a flow, as a draft",
		Long: "Ask a model for the full next version of a flow. The instruction is the rest of the arguments, " +
			"or standard input when it is `-` or absent and input is piped. The model gets the flow's active " +
			"version (else the newest) and the nodes it may use; the reply is validated, and errors go back to " +
			"the model up to three times, then the command fails with invalid_flow and the last errors. The model " +
			"call is flo's own: the ai.models allow and deny lists apply, the flow's privacy ceiling holds (a " +
			"local-only flow's document goes to a local model only) and a cloud model's key is read from the " +
			"secret store. The model is --model, else ai.builder.model, else a local ollama model. A valid " +
			"proposal is replayed on the flow's last recording (offline, nothing reaches real systems) and the " +
			"outcome differences are stored with the draft. Nothing is stored as a flow version. " +
			"--new <name> drafts a flow that does not exist yet (the name must be free; the model sets flow: and a " +
			"version); there is no recording, so no dry run, and with no flow to read a privacy ceiling from the " +
			"model call is local-only unless --privacy cloud-allowed. --from <draft> refines a draft: the model " +
			"gets the original base, that draft's document, the instructions so far and the new one; the new draft " +
			"keeps the original base, links to its parent (which is kept) and is stale when the base moves.",
		Args: cobra.MinimumNArgs(1),
	}
	model := c.Flags().String("model", "", "the model: scheme://<model>; default ai.builder.model, else a local ollama model")
	newName := c.Flags().String("new", "", "draft a flow that does not exist yet, with this name; the arguments are the instruction")
	from := c.Flags().String("from", "", "refine this draft (multi-turn); the arguments are the new instruction")
	privacy := c.Flags().String("privacy", "", "with --new, the ceiling of the model call: local-only (default) or cloud-allowed")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if *newName != "" && *from != "" {
			return core.New(core.CodeUsage, "--new and --from do not go together").
				WithSuggestion("refine a new flow's draft with --from alone: it stays a new flow")
		}
		rest := args
		flow := ""
		if *newName == "" && *from == "" {
			if len(args) == 0 {
				return core.New(core.CodeUsage, "no flow").WithSuggestion(`flo flow draft create <flow> "what to change"`)
			}
			flow, rest = args[0], args[1:]
		} else if *newName != "" {
			flow = *newName
		}
		if *from != "" {
			if err := core.CheckID(core.PrefixDraft, *from); err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
		}
		instruction := strings.Join(rest, " ")
		if instruction == "" || instruction == "-" {
			b, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return core.Wrap(core.CodeUsage, err)
			}
			instruction = strings.TrimSpace(string(b))
		}
		if instruction == "" {
			return core.New(core.CodeUsage, "no instruction").
				WithSuggestion(`flo flow draft create <flow> "what to change"`)
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
		req := serve.DraftRequest{Flow: flow, Instruction: instruction, Model: *model, Caller: caller, New: *newName != "", Privacy: *privacy, From: *from}
		target := flow
		if *from != "" {
			target = "draft:" + *from
		}
		if kitcli.IsDryRun(cmd) {
			return kitcli.RenderPlan(cmd, kitcli.Plan{
				Args: map[string]any{"flow": flow, "new": *newName != "", "from": *from, "privacy": *privacy, "instruction": instruction, "model": *model, "by": caller},
				Effects: []kitcli.Effect{
					{Kind: "model-call", Target: "flow:" + target, Reversible: true, Detail: "the flow's document and the instruction go to the model, up to four times"},
					{Kind: "store", Target: "draft of flow:" + target, Reversible: true, Detail: "a draft, not a flow version"},
				},
			})
		}
		d, err := e.CreateDraft(cmd.Context(), tenant, req)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, d)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteLocal)
	kitcli.SetIdempotency(c, kitcli.IdempotencyNo)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &builder.Draft{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "ask for a change", Command: `flo flow draft create dependency-watch "also watch cli/cli releases"`},
		{Title: "name the model", Command: `flo flow draft create dependency-watch "run at 6" --model ollama://qwen3:4b`},
		{Title: "a flow that does not exist yet", Command: `flo flow draft create --new pr-digest "every morning list my open PRs"`},
		{Title: "refine a draft", Command: `flo flow draft create --from drf_01 "also run at 6"`},
		{Title: "preview", Command: `flo flow draft create dependency-watch "run at 6" --dry-run`},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the draft is made", Suggest: "flo flow draft show <draft>", Reason: "read the proposal and its dry run"},
		{When: "the draft is right", Suggest: "flo flow draft apply <draft>", Reason: "store it as the next version"},
	})
	return c
}

func draftArg(args []string) (string, error) {
	if err := core.CheckID(core.PrefixDraft, args[0]); err != nil {
		return "", err //nolint:wrapcheck // *core.Error
	}
	return args[0], nil
}

func draftShowCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "show <draft>",
		Short: "Show a draft: the proposal, what the model got wrong, the dry run",
		Long: "Show a draft: the proposed document, the base it was made on, each attempt with the errors that went " +
			"back to the model, the model and its spend, and the dry run on the flow's last recording " +
			"(where the outcome differs, or why there was none).",
		Args: cobra.ExactArgs(1),
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := draftArg(args)
		if err != nil {
			return err
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		d, err := e.GetDraft(cmd.Context(), tenant, id)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, d)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &builder.Draft{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "show a draft as JSON", Command: "flo flow draft show drf_01 --format json"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "the draft is right", Suggest: "flo flow draft apply <draft>", Reason: "store it as the next version"}})
	return c
}

func draftListCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "list [flow]",
		Short: "List drafts, newest first",
		Long:  "List the drafts of one flow, or of every flow, newest first, with the version each proposes and how its dry run went: matches, differs, skipped or none.",
		Args:  cobra.MaximumNArgs(1),
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		flow := ""
		if len(args) > 0 {
			flow = args[0]
		}
		ds, err := e.ListDrafts(cmd.Context(), tenant, flow)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, draftRows(ds))
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &[]draftRow{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "the drafts of a flow", Command: "flo flow draft list dependency-watch --format json"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "a draft is listed", Suggest: "flo flow draft show <draft>", Reason: "read it"}})
	return c
}

func draftApplyCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "apply <draft>",
		Short: "Store a draft as the flow's next version, if the flow has not moved",
		Long: "Store the draft's document as the flow's next version (a draft version, as `flo flow apply` stores it), " +
			"only when the flow's base version and newest version are the ones the draft was made on; otherwise " +
			"it fails with draft_stale (exit 4) and keeps the draft. The document is validated again and your " +
			"grants checked as for `flo flow apply`, and the proposal is replayed on the flow's last recording " +
			"once more: the report carries the outcome differences. A dry run that shows a step that failed or " +
			"ended with another status, or a call with no recording, refuses the apply with " +
			"draft_dry_run_failed (exit 4) and keeps the draft, unless --force; a different output only " +
			"reports, and a flow with no recording has nothing to compare, so it applies (the report says why). " +
			"--write also rewrites the bundle's flow.yaml with the draft's document, atomically and keeping its " +
			"mode, and refuses with draft_stale when the file is not the one the draft was made on; a new " +
			"flow's bundle is written to <dir>/<name>/flow.yaml, which must not exist (--dir, else the flows.dir setting, default flows). " +
			"Without --yes it asks for confirmation at " +
			"a terminal, and fails with confirmation_required without one. The draft is spent. Test the version " +
			"with `flo flow test`, then activate it with `flo flow set`.",
		Args: cobra.ExactArgs(1),
	}
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	force := c.Flags().Bool("force", false, "apply although the dry run shows a step that failed or ended differently, or a call with no recording")
	write := c.Flags().Bool("write", false, "also write the document to the bundle's flow.yaml (a new flow: <dir>/<name>/flow.yaml); refused when the file changed since the draft")
	dir := c.Flags().String("dir", "", "with --write on a new flow, the directory its bundle goes in (default: flows.dir of the config, else flows)")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := draftArg(args)
		if err != nil {
			return err
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		opts := serve.ApplyDraftOptions{Force: *force, Write: *write}
		if *write {
			if *dir != "" {
				abs, aerr := filepath.Abs(*dir)
				if aerr != nil {
					return core.Wrap(core.CodeUsage, aerr)
				}
				opts.Dir = abs
			} else {
				opts.Dir, _ = flowsDir(root)
			}
		}
		caller, err := accessCaller(root)
		if err != nil {
			return err
		}
		if kitcli.IsDryRun(cmd) {
			d, err := e.CheckDraft(cmd.Context(), tenant, id)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			effects := []kitcli.Effect{{
				Kind: "store", Target: "flow:" + d.Flow + "@" + d.Hash, Reversible: true, Detail: "a new draft version; the draft is spent",
			}}
			if *write {
				effects = append(effects, kitcli.Effect{Kind: "write", Target: "the flow.yaml of the flow's bundle", Reversible: true, Detail: "rewritten with the draft's document, if it has not changed since the draft"})
			}
			return kitcli.RenderPlan(cmd, kitcli.Plan{
				Args:    map[string]any{"draft": d.ID, "flow": d.Flow, "version": d.Version, "hash": d.Hash, "base": d.Base.Hash, "dry_run": d.DryRun, "force": *force, "write": *write},
				Effects: effects,
			})
		}
		d, err := e.CheckDraft(cmd.Context(), tenant, id) // fail before asking
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if err := confirmWrite(*yes, fmt.Sprintf("store draft %s as the next version of flow %s?", d.ID, d.Flow)); err != nil {
			return err
		}
		res, err := e.ApplyDraftWith(cmd.Context(), tenant, caller, id, opts)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		warn := []serve.Issue{}
		for _, i := range res.Applied.Report.Issues {
			if i.Severity != "error" {
				warn = append(warn, i)
			}
		}
		return output.Dispatch(cmd, root.Viper, draftApplied{
			Draft:       res.Draft.ID,
			flowApplied: flowApplied{flowVersionView: flowVersionOf(res.Applied.Version), Changed: res.Applied.Created, Warnings: warn},
			DryRun:      res.DryRun, Overridden: res.Overridden, Written: res.Written,
		})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteLocal)
	kitcli.SetIdempotency(c, kitcli.IdempotencyNo)
	kitcli.SetRetryable(c, false)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &draftApplied{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "apply a draft", Command: "flo flow draft apply drf_01 --yes"},
		{Title: "apply and rewrite the bundle's flow.yaml", Command: "flo flow draft apply drf_01 --yes --write"},
		{Title: "apply over a dry run that shows a failing step", Command: "flo flow draft apply drf_01 --yes --force"},
		{Title: "preview", Command: "flo flow draft apply drf_01 --dry-run"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the version is stored", Suggest: "flo flow test <flow>", Reason: "activation needs a passing test of this version"},
		{When: "the draft is stale", Suggest: "flo flow draft create <flow> <instruction>", Reason: "make a new draft on the current flow"},
	})
	return c
}

func draftDiscardCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "discard <draft>",
		Short: "Delete a draft",
		Long: "Delete a draft that is not wanted: a draft is not a flow version, so nothing else changes. " +
			"Without --yes it asks for confirmation at a terminal, and fails with confirmation_required without one. " +
			"--dry-run checks that the draft exists and deletes nothing.",
		Args: cobra.ExactArgs(1),
	}
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := draftArg(args)
		if err != nil {
			return err
		}
		e, err := flowOps(cmd, eng)
		if err != nil {
			return err
		}
		tenant, err := accessTenant(root)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if kitcli.IsDryRun(cmd) {
			if _, err := e.GetDraft(cmd.Context(), tenant, id); err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return kitcli.RenderPlan(cmd, kitcli.Plan{
				Args:    map[string]any{"draft": id},
				Effects: []kitcli.Effect{{Kind: "delete", Target: "draft:" + id, Reversible: false, Detail: "the draft is deleted"}},
			})
		}
		if err := confirmWrite(*yes, fmt.Sprintf("discard draft %s?", id)); err != nil {
			return err
		}
		if err := e.DiscardDraft(cmd.Context(), tenant, id); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, draftDiscarded{Draft: id, Discarded: true})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteLocal)
	kitcli.SetIdempotency(c, kitcli.IdempotencyNo)
	kitcli.SetRetryable(c, false)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &draftDiscarded{}, Version: flowSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{{Title: "discard", Command: "flo flow draft discard drf_01 --yes"}})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{{When: "a draft is discarded", Suggest: "flo flow draft create <flow> <instruction>", Reason: "ask again"}})
	return c
}

type draftDiscarded struct {
	Draft     string `json:"draft" yaml:"draft"`
	Discarded bool   `json:"discarded" yaml:"discarded"`
}
