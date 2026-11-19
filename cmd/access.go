package cmd

import (
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/access/provision"
	"hop.top/flo/internal/bos"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/store/tenants"
	"hop.top/flo/internal/version"
)

const accessSchemaVersion = "1.0"

// accessStatePath is where the set flo holds is kept: next to the tenant's
// database. Tests replace it.
var accessStatePath = func(tenant core.TenantID) (string, error) {
	root, err := tenants.DefaultRoot()
	if err != nil {
		return "", err //nolint:wrapcheck // *core.Error
	}
	return filepath.Join(root, "tenants", string(tenant), "access-set.json"), nil
}

// maxSetBytes bounds the set read from a file or standard input.
const maxSetBytes = 32 << 20

// accessConfig is the config of the applier: the bound scope, the profile to
// principal mapping and the age of the applied set past which flo denies what
// it provisioned (bos.max_staleness, default 24h).
func accessConfig(root *kitcli.Root) (d provision.Config, err error) {
	bc, err := bosSettings(root)
	if err != nil {
		return provision.Config{}, err
	}
	d = provision.Config{Scope: bc.Scope, Principals: bc.Principals}
	d.MaxStaleness, err = durationSetting(root, EnvBosMaxStaleness, "bos.max_staleness")
	return d, err
}

// addAccess registers the `flo access` noun: apply, state. They are the peer
// verbs of bos.access/v1 for a peer with no server: bos pushes a set and flo
// writes the operations it grants into its own grants.
func addAccess(root *kitcli.Root, svc ServicesFunc) {
	a := &cobra.Command{Use: "access", Short: "access: the operation grants bos provisions"}
	a.AddCommand(accessApplyCmd(root, svc), accessStateCmd(root, svc))
	root.Cmd.AddCommand(a)
}

func accessApplier(root *kitcli.Root, svc ServicesFunc, cmd *cobra.Command) (*provision.Applier, core.TenantID, error) {
	s, tenant, err := accessServices(cmd, root, svc)
	if err != nil {
		return nil, "", err
	}
	cfg, err := accessConfig(root)
	if err != nil {
		return nil, "", err
	}
	path, err := accessStatePath(tenant)
	if err != nil {
		return nil, "", err
	}
	return provision.New(s.GrantRows, s.Clock, provision.FileState{Path: path}, cfg), tenant, nil
}

func accessApplyCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "apply",
		Short: "Apply a bos access set to the grants",
		Long: "Read a bos.access/v1 set (full, or a delta over the one held) from --file or standard input " +
			"and write the operations it grants into flo's grants: each operation a principal may run becomes " +
			"a provisioned capability of the profile mapped to it (config bos.principals), ending at the earlier " +
			"of its own end and the staleness limit (bos.max_staleness, shorter if the set says so). A grant flo " +
			"cannot express (a record read, a narrower scope, a subject no profile maps to) is listed in " +
			"unrepresentable and denied. Replaying the epoch held with its digest only refreshes its age. " +
			"Refusals carry the contract's codes: epoch_stale, base_epoch_mismatch, digest_mismatch, " +
			"set_invalid, scope_unknown, unsupported_contract. Without a terminal it needs --yes.",
		Args: cobra.NoArgs,
	}
	file := c.Flags().String("file", "-", "the set: a path, or - for standard input")
	yes := c.Flags().Bool("yes", false, "confirm without asking")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		raw, err := readSet(cmd.InOrStdin(), *file)
		if err != nil {
			return err
		}
		ap, tenant, err := accessApplier(root, svc, cmd)
		if err != nil {
			return err
		}
		if err := confirmWrite(*yes, "apply this access set to the grants?"); err != nil {
			return err
		}
		ack, err := ap.Apply(cmd.Context(), tenant, raw)
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, ack)
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteShared)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &provision.Ack{}, Version: accessSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "apply a set from a file", Command: "flo access apply --file set.json --yes --format json"},
		{Title: "apply the set on standard input", Command: "flo access apply --yes < set.json"},
	})
	return c
}

func accessStateCmd(root *kitcli.Root, svc ServicesFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "state",
		Short: "Show the access set flo holds",
		Long: "Show the epoch, digest and time of the set flo holds, the staleness limit in force and the " +
			"grants it cannot express (bos.access/v1 state). With no set applied flo denies every provisioned operation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ap, tenant, err := accessApplier(root, svc, cmd)
			if err != nil {
				return err
			}
			st, err := ap.State(cmd.Context(), tenant)
			if err != nil {
				return err //nolint:wrapcheck // *core.Error
			}
			return output.Dispatch(cmd, root.Viper, st)
		},
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &provision.StateDoc{}, Version: accessSchemaVersion})
	return c
}

func readSet(in io.Reader, file string) ([]byte, error) {
	r := in
	if file != "" && file != "-" {
		f, err := os.Open(file) //nolint:gosec // the path is the caller's argument
		if err != nil {
			return nil, core.Errorf(core.CodeUsage, "--file: %v", err)
		}
		defer f.Close() //nolint:errcheck // read-only
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxSetBytes+1))
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "read the set: %v", err)
	}
	if len(b) > maxSetBytes {
		return nil, core.Errorf(core.CodeSetInvalid, "the set is over %d bytes", maxSetBytes)
	}
	return b, nil
}

// addCapabilities registers `flo capabilities`: the peer profile of
// bos.peer/v1 (the verb a peer with no server prints).
func addCapabilities(root *kitcli.Root) {
	c := &cobra.Command{
		Use:   "capabilities",
		Short: "Print the peer profile: contracts, grant shapes, limits",
		Long: "Print what flo declares about itself to a governance peer (bos.peer/v1): the contracts it " +
			"implements (bos.access/v1 partially: operation grants and their end; bos.flo/v1), the grant " +
			"shapes it expresses (operation_grant, expiry) and the staleness limit it enforces. " +
			"--format json is the document bos reads.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := accessConfig(root)
			if err != nil {
				return err
			}
			limit := cfg.MaxStaleness
			if limit <= 0 {
				limit = provision.DefaultMaxStaleness
			}
			return output.Dispatch(cmd, root.Viper, bos.Profile(version.Version(), limit))
		},
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectRead)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &bos.PeerProfile{}, Version: accessSchemaVersion})
	kitcli.SetTopLevelVerb(c)
	root.Cmd.AddCommand(c)
}
