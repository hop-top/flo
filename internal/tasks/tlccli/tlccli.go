// Package tlccli is core.Tasks over the tlc command line: the human surface
// of approvals and the accountable task of a run (flow.md §7.8). It spawns
// nothing itself; every call goes through a core.CommandRunner.
//
// tlc statuses map onto what a person did: TODO and IN_PROGRESS (and any
// status this package does not know) are open, DONE is approved, SKIPPED is
// rejected. Closing goes the other way: approved is `task complete`,
// rejected is `task skip`, both past the state machine, because a decision
// is the task's whole work and the task is never claimed first.
package tlccli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// Config says how to reach tlc.
type Config struct {
	// Bin is the tlc executable; empty is "tlc".
	Bin string
	// Dir is the project directory, passed as -C; empty uses the working
	// directory of the engine.
	Dir string
	// Profile, when set, runs tlc as `aps run <profile> -- tlc ...`.
	Profile string
	// Timeout bounds each call; zero is 30s.
	Timeout time.Duration
}

// Adapter is the core.Tasks over tlc.
type Adapter struct {
	cfg Config
	run core.CommandRunner
}

var _ core.Tasks = (*Adapter)(nil)

// New returns the adapter over run.
func New(run core.CommandRunner, cfg Config) *Adapter {
	if cfg.Bin == "" {
		cfg.Bin = "tlc"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Adapter{cfg: cfg, run: run}
}

// tlc statuses.
const (
	statusDone    = "DONE"
	statusSkipped = "SKIPPED"
)

var taskIDRe = regexp.MustCompile(`\bT-\d+\b`)

func (a *Adapter) argv(args ...string) []string {
	out := []string{a.cfg.Bin}
	if a.cfg.Dir != "" {
		out = append(out, "-C", a.cfg.Dir)
	}
	return append(out, args...)
}

func (a *Adapter) exec(ctx context.Context, what string, args ...string) (core.CommandResult, error) {
	res, err := a.run.Run(ctx, core.CommandSpec{Argv: a.argv(args...), Profile: a.cfg.Profile, Timeout: a.cfg.Timeout})
	if err != nil {
		return res, core.Errorf(core.CodeEngineUnreachable, "tlc %s: %w", what, err)
	}
	if res.TimedOut {
		return res, core.Errorf(core.CodeTimeout, "tlc %s timed out after %s", what, a.cfg.Timeout)
	}
	if res.ExitCode != 0 {
		text := strings.TrimSpace(string(res.Stderr))
		if text == "" {
			text = strings.TrimSpace(string(res.Stdout))
		}
		low := strings.ToLower(text)
		if strings.Contains(low, "not found") || strings.Contains(low, "not_found") {
			return res, core.Errorf(core.CodeTaskNotFound, "tlc %s: %s", what, text)
		}
		return res, core.Errorf(core.CodeInternal, "tlc %s exited %d: %s", what, res.ExitCode, text).
			WithDetail("exit_code", res.ExitCode)
	}
	return res, nil
}

// Create implements core.Tasks. tlc prints human text for create, so the
// reference is the T-NNNN of its first line that has one. A parent is
// recorded as a `parent:<ref>` tag, the convention tlc tasks already use.
func (a *Adapter) Create(ctx context.Context, _ core.TenantID, spec core.TaskSpec) (core.TaskRef, error) {
	if strings.TrimSpace(spec.Title) == "" {
		return "", core.New(core.CodeUsage, "a task needs a title")
	}
	args := []string{"task", "create", spec.Title, "--no-prompt"}
	if spec.Description != "" {
		args = append(args, "--description", spec.Description)
	}
	if spec.Assignee != "" {
		args = append(args, "--assigned-to", spec.Assignee)
	}
	if spec.Due != nil {
		args = append(args, "--due", spec.Due.UTC().Format(time.DateOnly))
	}
	tags := append([]string{"flo"}, spec.Tags...)
	if spec.Run != "" {
		tags = append(tags, "run:"+string(spec.Run))
	}
	if spec.Approval != "" {
		tags = append(tags, "approval:"+string(spec.Approval))
	}
	if spec.Parent != "" {
		tags = append(tags, "parent:"+string(spec.Parent))
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	res, err := a.exec(ctx, "task create", args...)
	if err != nil {
		return "", err
	}
	id := taskIDRe.FindString(string(res.Stdout))
	if id == "" {
		return "", core.Errorf(core.CodeInternal, "tlc task create: no task id in its output: %q", strings.TrimSpace(string(res.Stdout)))
	}
	return core.TaskRef(id), nil
}

// shown is the part of `task show --format json` this package reads.
type shown struct {
	Task struct {
		Status     string `json:"status"`
		Title      string `json:"title"`
		AssignedTo string `json:"assigned_to"`
	} `json:"task"`
	Logs []struct {
		ID        int64  `json:"id"`
		Timestamp string `json:"timestamp"`
		By        string `json:"by"`
		Action    string `json:"action"`
		Note      string `json:"note"`
	} `json:"logs"`
}

// stateOf maps a tlc status onto what a person did.
func stateOf(status string) core.TaskState {
	switch strings.ToUpper(status) {
	case statusDone:
		return core.TaskApproved
	case statusSkipped:
		return core.TaskRejected
	}
	return core.TaskOpen
}

// statusNote is the prefix tlc puts before the note of a status change.
var statusNote = regexp.MustCompile(`^Status changed from [^\s:]+ to [^\s:]+(?:: )?`)

// Get implements core.Tasks.
func (a *Adapter) Get(ctx context.Context, _ core.TenantID, ref core.TaskRef) (core.TaskInfo, error) {
	if ref == "" {
		return core.TaskInfo{}, core.New(core.CodeUsage, "a task reference is empty")
	}
	res, err := a.exec(ctx, "task show", "task", "show", string(ref), "--format", "json", "--no-prompt")
	if err != nil {
		return core.TaskInfo{}, err
	}
	var s shown
	if err := json.Unmarshal(res.Stdout, &s); err != nil {
		return core.TaskInfo{}, core.Errorf(core.CodeInternal, "tlc task show %s: output is not JSON: %w", ref, err)
	}
	info := core.TaskInfo{Ref: ref, State: stateOf(s.Task.Status), Title: s.Task.Title}
	if info.State == core.TaskOpen {
		return info, nil
	}
	// the newest log of the closing action says who and when
	var best int64 = -1
	for _, l := range s.Logs {
		if !strings.EqualFold(l.Action, s.Task.Status) || l.ID <= best {
			continue
		}
		best = l.ID
		info.By, info.Note = l.By, statusNote.ReplaceAllString(l.Note, "")
		info.At = nil
		if t, err := time.Parse(time.RFC3339, l.Timestamp); err == nil {
			info.At = &t
		}
	}
	return info, nil
}

// Close implements core.Tasks. Closing a closed task is not an error and
// changes nothing.
func (a *Adapter) Close(ctx context.Context, tenant core.TenantID, ref core.TaskRef, outcome core.TaskState, note string) error {
	var verb string
	switch outcome {
	case core.TaskApproved:
		verb = "complete"
	case core.TaskRejected:
		verb = "skip"
	default:
		return core.Errorf(core.CodeUsage, "a task is closed as approved or rejected, not %q", outcome)
	}
	cur, err := a.Get(ctx, tenant, ref)
	if err != nil {
		return err
	}
	if cur.State != core.TaskOpen {
		return nil
	}
	args := []string{"task", verb, string(ref), "--no-verify", "--no-prompt"}
	if note != "" {
		args = append(args, "--note", note)
	}
	if _, err := a.exec(ctx, "task "+verb, args...); err != nil {
		return fmt.Errorf("close task %s: %w", ref, err)
	}
	return nil
}
