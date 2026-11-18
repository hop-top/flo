package main

import (
	"os"

	"hop.top/flo/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(cmd.ExitCode(err))
	}
}
