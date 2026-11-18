// Package command runs processes (node.md §4): the CommandRunner that
// spawns them through kit's sideeffect.Exec seam, and the executor of
// declared command nodes built on the CommandRunner port.
package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"

	"hop.top/kit/go/runtime/sideeffect"

	"hop.top/flo/internal/core"
)

// killDelay bounds how long Wait lingers for output pipes after the process
// group is killed.
const killDelay = 2 * time.Second

// Runner is the core.CommandRunner: a command, or `aps run <profile> --
// <command>` when the spec names a profile. Every process goes through the
// injected sideeffect.Exec, so a test substitutes one fake for all of them.
// Nothing is passed through a shell.
type Runner struct {
	// Exec starts the process.
	Exec sideeffect.Exec
	// Aps is the aps binary; empty is "aps".
	Aps string
}

// NewRunner returns a Runner over exec.
func NewRunner(exec sideeffect.Exec) *Runner { return &Runner{Exec: exec} }

// Argv returns the command line that will run spec: the argv, behind
// `aps run <profile> --` when a profile is set.
func (r *Runner) Argv(spec core.CommandSpec) []string {
	if spec.Profile == "" {
		return append([]string(nil), spec.Argv...)
	}
	bin := r.Aps
	if bin == "" {
		bin = "aps"
	}
	return append([]string{bin, "run", spec.Profile, "--"}, spec.Argv...)
}

// Run implements core.CommandRunner. An error is returned only when the
// process could not be started; a non-zero exit, a timeout and a truncated
// stream are reported in the result. On timeout the whole process group is
// killed and the output written so far is kept.
func (r *Runner) Run(ctx context.Context, spec core.CommandSpec) (core.CommandResult, error) {
	argv := r.Argv(spec)
	if len(spec.Argv) == 0 {
		return core.CommandResult{}, core.Errorf(core.CodeInternal, "command has no argv")
	}
	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	limit := spec.MaxOutput
	if limit <= 0 {
		limit = core.DefaultMaxOutput
	}
	var stdout, stderr capped
	stdout.max, stderr.max = limit, limit

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...) //nolint:gosec // argv is the declared command, run without a shell
	cmd.Dir = spec.Cwd
	cmd.Env = environ(spec.Env)
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = killDelay
	groupKill(cmd)

	start := time.Now()
	err := r.Exec.Run(cmd)
	res := core.CommandResult{
		Stdout:    stdout.buf.Bytes(),
		Stderr:    stderr.buf.Bytes(),
		Duration:  time.Since(start),
		Truncated: stdout.truncated || stderr.truncated,
	}
	timedOut := spec.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	var exit *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	case timedOut:
		res.ExitCode = -1
	default:
		return core.CommandResult{}, fmt.Errorf("start %s: %w", argv[0], err)
	}
	if timedOut {
		res.TimedOut = true
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
	}
	return res, nil
}

func environ(extra map[string]string) []string {
	env := os.Environ()
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// capped is a bounded buffer: it keeps the first max bytes, drops the rest
// and reports that it did. It never fails a write, so the child is not
// stopped by a full pipe.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

var _ core.CommandRunner = (*Runner)(nil)
