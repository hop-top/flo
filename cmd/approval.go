package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
)

// addApproval registers the `flo approval` noun. Its verbs are added by the piece of
// work that owns them; svc opens the ports they run against.
func addApproval(root *kitcli.Root, svc ServicesFunc) {
	_ = svc
	root.Cmd.AddCommand(&cobra.Command{
		Use:   "approval",
		Short: "approval commands",
	})
}
