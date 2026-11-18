package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
)

// addRun registers the `flo run` noun. Its verbs are added by the piece of
// work that owns them; svc opens the ports they run against.
func addRun(root *kitcli.Root, svc ServicesFunc) {
	_ = svc
	root.Cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "run commands",
	})
}
