//go:build !windows

package command

import (
	"os"
	"os/exec"
	"syscall"
)

// groupKill puts the child in its own process group and makes a context
// cancellation kill the whole group, so a wrapper script's children do not
// outlive a timeout (node.md §4.1).
func groupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
