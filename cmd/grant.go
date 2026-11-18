package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
)

// addGrant registers the `flo grant` noun. Its verbs are added by the piece of
// work that owns them; svc opens the ports they run against.
func addGrant(root *kitcli.Root, svc ServicesFunc) {
	_ = svc
	root.Cmd.AddCommand(&cobra.Command{
		Use:   "grant",
		Short: "grant commands",
	})
}
