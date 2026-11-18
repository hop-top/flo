//go:build windows

package command

import "os/exec"

// groupKill: the context kills the process; process groups are a Unix
// notion and phase 1 runs on Unix hosts.
func groupKill(*exec.Cmd) {}
