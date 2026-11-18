package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
)

// Environment variables and config keys of caller identity.
const (
	// EnvProfile names the profile the caller acts as; it overrides the
	// config key.
	EnvProfile = "FLO_PROFILE"
	// ConfigOwnerProfile is the config key of the owner's profile.
	ConfigOwnerProfile = "owner_profile"
	// EnvOwnerProfile is the environment form of owner_profile.
	EnvOwnerProfile = EnvPrefix + "_OWNER_PROFILE"
	// EnvTenant and ConfigTenant name the tenant a command works in.
	EnvTenant    = "FLO_TENANT"
	ConfigTenant = "tenant"
)

// accessCaller resolves the profile the command acts as: FLO_PROFILE, else
// the owner_profile of the config, else the login name ($USER).
func accessCaller(root *kitcli.Root) (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvProfile)); p != "" {
		return p, nil
	}
	if root != nil && root.Viper != nil {
		if p := strings.TrimSpace(root.Viper.GetString(ConfigOwnerProfile)); p != "" {
			return p, nil
		}
	}
	if p := strings.TrimSpace(os.Getenv("USER")); p != "" {
		return p, nil
	}
	return "", core.New(core.CodeUsage, "no caller profile").
		WithSuggestion("set " + EnvProfile + " or owner_profile in the config")
}

// accessTenant resolves the tenant: FLO_TENANT, else the config, else the
// default tenant of a single-user install.
func accessTenant(root *kitcli.Root) (core.TenantID, error) {
	s := setting(root, EnvTenant, ConfigTenant)
	return core.ParseTenant(core.TenantID(s).OrDefault().String())
}

// promptSource is where a write asks for its confirmation: the controlling
// terminal. Tests replace it.
var promptSource = kitcli.ControllingTTY

// confirmWrite is the gate of every write (12-factor F6): --yes, or an
// answer at the terminal. Without a terminal and without --yes the write
// fails with confirmation_required.
func confirmWrite(yes bool, question string) error {
	if yes {
		return nil
	}
	tty := promptSource()
	if tty == nil {
		return core.New(core.CodeConfirmationRequired, "this write needs confirmation and there is no terminal").
			WithSuggestion("rerun with --yes, or preview it with --dry-run")
	}
	_, _ = fmt.Fprintf(tty, "%s [y/N] ", question)
	ans, err := tty.ReadLine()
	if err != nil && ans == "" {
		return core.New(core.CodeConfirmationRequired, "no answer at the terminal")
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "yes":
		return nil
	}
	return core.New(core.CodeConfirmationRequired, "not confirmed")
}

// grantRow is one grant as `flo grant list` prints it.
type grantRow struct {
	Profile    string `table:"PROFILE" json:"profile" yaml:"profile"`
	Capability string `table:"CAPABILITY" json:"capability" yaml:"capability"`
	Source     string `table:"SOURCE" json:"source" yaml:"source"`
	CreatedAt  string `table:"CREATED" json:"created_at" yaml:"created_at"`
}

// grantChange is what `flo grant set|unset` reports.
type grantChange struct {
	Profile    string `table:"PROFILE" json:"profile" yaml:"profile"`
	Capability string `table:"CAPABILITY" json:"capability" yaml:"capability"`
	Action     string `table:"ACTION" json:"action" yaml:"action"`
	// Changed is false when the grant already was (set) or was not (unset)
	// there: repeating a write is a no-op.
	Changed bool `table:"CHANGED" json:"changed" yaml:"changed"`
}

const grantSchemaVersion = "1.0"

func grantRows(gs []core.Grant) []grantRow {
	rows := make([]grantRow, len(gs))
	for i, g := range gs {
		rows[i] = grantRow{Profile: g.Profile, Capability: g.Capability, Source: string(g.Source), CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339)}
	}
	return rows
}

// addGrant registers the `flo grant` noun: list, set, unset.
func addGrant(root *kitcli.Root, svc ServicesFunc) {
	grant := &cobra.Command{Use: "grant", Short: "grants: what a profile may do"}
	grant.AddCommand(grantListCmd(root, svc), grantWriteCmd(root, svc, true), grantWriteCmd(root, svc, false))
	root.Cmd.AddCommand(grant)
}

func grantListCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "list [profile]",
		Short: "List grants",
		Long: "List the grants of a profile, or of every profile. A grant is a capability " +
			"(<domain>.<verb>), the wildcard * (every capability), or profile.use:<profile> " +
			"(may name that profile as run_as). Access is deny by default. Source says who owns " +
			"a grant: local, or provisioned by the governance backend (refused by set and unset).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, tenant, err := accessServices(cmd, root, svc)
			if err != nil {
				return err
			}
			profile := ""
			if len(args) == 1 {
				profile = args[0]
			}
			gs, err := s.Grants.List(cmd.Context(), tenant, profile)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return output.Dispatch(cmd, root.Viper, grantRows(gs))
		},
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &[]grantRow{}, Version: grantSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "every grant", Command: "flo grant list"},
		{Title: "one profile, as JSON", Command: "flo grant list noor --format json"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "a profile lacks a capability", Suggest: "flo grant set <profile> <capability> --yes", Reason: "grants are deny by default"},
	})
	return c
}

func grantWriteCmd(root *kitcli.Root, svc ServicesFunc, set bool) *cobra.Command {
	verb, short, effect := "unset", "Revoke a grant", "delete"
	if set {
		verb, short, effect = "set", "Grant a capability to a profile", "create"
	}
	c := &cobra.Command{
		Use:   verb + " <profile> <capability>",
		Short: short,
		Long: short + ". Local grants only: a grant the governance backend provisions is refused " +
			"(grant_provisioned, exit 4). The caller must hold the wildcard grant. Repeating the " +
			"write changes nothing. Without a terminal it needs --yes (confirmation_required, " +
			"exit 2); --dry-run previews it.",
		Args: cobra.ExactArgs(2),
	}
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		profile, capability := args[0], args[1]
		s, tenant, err := accessServices(cmd, root, svc)
		if err != nil {
			return err
		}
		caller, err := accessCaller(root)
		if err != nil {
			return err
		}
		if ok, err := s.Grants.Covers(cmd.Context(), tenant, caller, core.CapabilityAny); err != nil {
			return err //nolint:wrapcheck // *core.Error
		} else if !ok {
			return core.Errorf(core.CodeGrantDenied, "%s may not change grants", caller).
				WithSuggestion("act as a profile that holds the wildcard grant (" + EnvProfile + ")")
		}
		if kitcli.IsDryRun(cmd) {
			return grantPlan(cmd, s, tenant, verb, effect, profile, capability)
		}
		if err := confirmWrite(*yes, fmt.Sprintf("%s %s %s?", verb, profile, capability)); err != nil {
			return err
		}
		change := s.Grants.Unset
		if set {
			change = s.Grants.Set
		}
		changed, err := change(cmd.Context(), tenant, profile, capability)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, grantChange{Profile: profile, Capability: capability, Action: verb, Changed: changed})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteShared)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &grantChange{}, Version: grantSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: verb + " a grant", Command: "flo grant " + verb + " noor mail.read --yes"},
		{Title: "preview", Command: "flo grant " + verb + " noor mail.read --dry-run"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "on success", Suggest: "flo grant list <profile>", Reason: "check what the profile holds"},
	})
	return c
}

func grantPlan(cmd *cobra.Command, s *core.Services, tenant core.TenantID, verb, effect, profile, capability string) error {
	plan := kitcli.Plan{Args: map[string]any{"profile": profile, "capability": capability, "tenant": string(tenant)}, Effects: []kitcli.Effect{}}
	held, err := s.Grants.List(cmd.Context(), tenant, profile)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	var cur *core.Grant
	for i := range held {
		if held[i].Capability == capability {
			cur = &held[i]
		}
	}
	switch {
	case cur != nil && cur.Source == core.GrantProvisioned:
		plan.Warnings = []string{"provisioned by the governance backend: the write would be refused (grant_provisioned)"}
	case verb == "set" && cur != nil:
		plan.Warnings = []string{profile + " already holds " + capability + ": a no-op"}
	case verb == "unset" && cur == nil:
		plan.Warnings = []string{profile + " does not hold " + capability + ": a no-op"}
	default:
		plan.Effects = append(plan.Effects, kitcli.Effect{Kind: effect, Target: "grant:" + profile + "/" + capability, Reversible: true})
	}
	return kitcli.RenderPlan(cmd, plan)
}

// accessServices opens the ports and the tenant of an access command.
func accessServices(cmd *cobra.Command, root *kitcli.Root, svc ServicesFunc) (*core.Services, core.TenantID, error) {
	tenant, err := accessTenant(root)
	if err != nil {
		return nil, "", err //nolint:wrapcheck // *core.Error
	}
	s, err := svc(cmd.Context())
	if err != nil {
		return nil, "", err
	}
	return s, tenant, nil
}
