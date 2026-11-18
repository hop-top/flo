//go:build !windows

package serve_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/runrec"
)

// startRecorded makes a run of the version with --record and has the engine
// run it, through the handler the worker runs.
func (r *rig) startRecorded(t *testing.T, flow, hash string) core.Run {
	t.Helper()
	res, err := r.e.Services.Starter.Start(bg, core.StartRequest{
		Tenant: core.DefaultTenant, Flow: flow, Version: hash, StartedBy: "jad", Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Run.Record {
		t.Fatal("the run does not say it is recorded")
	}
	if err := r.e.Advance(bg, core.DefaultTenant, res.Run.ID); err != nil {
		t.Fatal(err)
	}
	run, err := r.e.Services.Runs.GetRun(bg, core.DefaultTenant, res.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (r *rig) replay(t *testing.T, run core.RunID) (serve.ReplayResult, error) {
	t.Helper()
	return r.e.ReplayRun(bg, core.DefaultTenant, serve.ReplayRequest{Run: run, Caller: "jad"})
}

// liveFootprint is what a replay must leave as it found it.
type liveFootprint struct {
	runs, outputs, approvals int
	state                    map[string]any
	recording                map[string]string
}

func footprint(t *testing.T, r *rig, run core.RunID) liveFootprint {
	t.Helper()
	var f liveFootprint
	runs, _ := r.e.Services.Runs.ListRuns(bg, core.DefaultTenant, core.RunFilter{})
	f.runs = len(runs)
	outs, _ := r.e.Services.Outputs.ListOutputs(bg, core.DefaultTenant, core.OutputFilter{})
	f.outputs = len(outs)
	aps, _ := r.e.Services.ApprovalSvc.List(bg, core.DefaultTenant, core.ApprovalFilter{})
	f.approvals = len(aps)
	f.state, _ = r.e.Services.State.Snapshot(bg, core.DefaultTenant, "dependency-watch")
	f.recording = map[string]string{}
	dir, _ := r.e.Recordings.Dir(core.DefaultTenant, run)
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p) //nolint:gosec // a temp recording
			f.recording[p] = string(b)
		}
		return nil
	})
	return f
}

func TestARecordedRunReplaysOfflineFromItsRecordedStart(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	run := r.startRecorded(t, "dependency-watch", v.Hash)
	if run.Status != core.RunSucceeded || r.live.count() != 2 {
		t.Fatalf("the recorded run: %s, %d live calls", run.Status, r.live.count())
	}

	meta, found, err := r.e.Recordings.Load(core.DefaultTenant, run.ID)
	if err != nil || !found {
		t.Fatalf("meta: %v %v", found, err)
	}
	if meta.Status != runrec.StatusComplete || meta.Calls != 2 || len(meta.Partial) != 0 || meta.Hash != v.Hash || meta.RunStatus != "succeeded" {
		t.Fatalf("meta = %+v", meta)
	}
	if err := meta.Replayable(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{runrec.TriggerFile, runrec.VarsFile, runrec.StateFile, runrec.MetaFile} {
		if !r.e.Recordings.Exists(core.DefaultTenant, run.ID, name) {
			t.Errorf("recording lacks %s", name)
		}
	}

	// The live run committed state; the replay must start from the state the
	// run started from (empty), not from what the engine holds now.
	if st, _ := r.e.Services.State.Snapshot(bg, core.DefaultTenant, "dependency-watch"); len(st["seen_adv"].([]any)) != 2 {
		t.Fatalf("the live run committed no state: %v", st)
	}
	before := footprint(t, r, run.ID)
	res, err := r.replay(t, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Matches || len(res.Diffs) != 0 || len(res.Problems) != 0 || res.Status != "succeeded" || res.Calls != 2 || res.Replayed != 2 {
		t.Fatalf("replay = %+v", res)
	}
	if r.live.count() != 2 {
		t.Errorf("the replay reached the real world: %d live calls", r.live.count())
	}
	after := footprint(t, r, run.ID)
	if before.runs != after.runs || before.outputs != after.outputs || before.approvals != after.approvals {
		t.Errorf("the replay wrote live data: %+v -> %+v", before, after)
	}
	if !equalJSON(before.state, after.state) {
		t.Errorf("the replay changed live state: %v -> %v", before.state, after.state)
	}
	if !sameFiles(before.recording, after.recording) {
		t.Errorf("the replay changed the recording")
	}
}

func equalJSON(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestARecordingIsRunDataKeptOnTheMachine(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read") // privacy: cloud-allowed
	run := r.startRecorded(t, "dependency-watch", v.Hash)
	dir, err := r.e.Recordings.Dir(core.DefaultTenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(r.root, "tenants", "default", "runs")+string(filepath.Separator)) {
		t.Errorf("the recording %s is not beside the run log", dir)
	}
	if _, err := os.Stat(filepath.Join(v.Bundle, "record")); err == nil {
		t.Error("a recorded run wrote into the flow's bundle")
	}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		if d.IsDir() && info.Mode().Perm()&0o077 != 0 || !d.IsDir() && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", p, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, _, _ := r.e.Recordings.Load(core.DefaultTenant, run.ID)
	if meta.Privacy != "cloud-allowed" || !meta.MayLeave() {
		t.Errorf("a cloud-allowed flow: %+v", meta)
	}
}

func TestReplayRefusesWhatIsNotWhole(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")

	// a run that was not recorded
	plain, err := r.e.Services.Starter.Start(bg, core.StartRequest{Tenant: core.DefaultTenant, Flow: "dependency-watch", Version: v.Hash, StartedBy: "jad"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.Advance(bg, core.DefaultTenant, plain.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.replay(t, plain.Run.ID); !core.IsCode(err, core.CodeNotFound) || !strings.Contains(err.Error(), "no recording") {
		t.Errorf("a run without a recording: %v", err)
	}
	if _, err := r.replay(t, "run_01NOSUCHRUN0000000000000"); err == nil {
		t.Error("a run that does not exist")
	}

	run := r.startRecorded(t, "dependency-watch", v.Hash)
	meta, _, _ := r.e.Recordings.Load(core.DefaultTenant, run.ID)

	open := meta
	open.Status = runrec.StatusRecording
	if err := r.e.Recordings.Save(core.DefaultTenant, open); err != nil {
		t.Fatal(err)
	}
	if _, err := r.replay(t, run.ID); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "not finished") {
		t.Errorf("an unfinished recording: %v", err)
	}

	part := meta
	part.Partial = []string{"gh api: the response is over the size a recording keeps"}
	if err := r.e.Recordings.Save(core.DefaultTenant, part); err != nil {
		t.Fatal(err)
	}
	if _, err := r.replay(t, run.ID); !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "partial") {
		t.Errorf("a partial recording: %v", err)
	}

	if err := r.e.Recordings.Save(core.DefaultTenant, meta); err != nil {
		t.Fatal(err)
	}
	cass, _ := r.e.Recordings.Cassettes(core.DefaultTenant, run.ID)
	if err := os.RemoveAll(cass); err != nil {
		t.Fatal(err)
	}
	if _, err := r.replay(t, run.ID); !core.IsCode(err, core.CodeNotFound) || !strings.Contains(err.Error(), "cassettes") {
		t.Errorf("missing cassettes: %v", err)
	}
	if r.live.count() != 4 { // the two runs, one each of two calls
		t.Errorf("a refused replay reached the real world: %d live calls", r.live.count())
	}
}

func TestRecordingsAreKeptAsLongAsOutputs(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	old := r.startRecorded(t, "dependency-watch", v.Hash)
	fresh := r.startRecorded(t, "dependency-watch", v.Hash)
	meta, _, _ := r.e.Recordings.Load(core.DefaultTenant, old.ID)
	meta.CreatedAt = time.Now().Add(-core.OutputRetention - time.Hour)
	if err := r.e.Recordings.Save(core.DefaultTenant, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Pruner.Prune(bg, core.DefaultTenant); err != nil {
		t.Fatal(err)
	}
	if r.e.Recordings.Exists(core.DefaultTenant, old.ID, runrec.MetaFile) {
		t.Error("a recording older than the retention is kept")
	}
	if !r.e.Recordings.Exists(core.DefaultTenant, fresh.ID, runrec.MetaFile) {
		t.Error("a recent recording was pruned")
	}
}

// ---- redaction, secrets, env ----

// echoExec is the real world of a flow with a secret input: it answers `lookup`
// with a document that holds a redacted output field, and echoes what the
// recording must not keep.
type echoExec struct {
	mu     sync.Mutex
	stdins []string
}

const (
	secretToken = "tok-4f9c1d7a-input"  //nolint:gosec // a fake: a redacted input
	secretOut   = "out-8b2e6a30-field"  //nolint:gosec // a fake: a redacted output field
	varSecret   = "pass-3c7d1e90-var"   //nolint:gosec // a fake: a secret-looking var
	stateSecret = "pw-default-9z8y7x"   //nolint:gosec // a fake: a state value
	envSecret   = "env-secret-5d3b9e21" //nolint:gosec // a fake: a secret of the environment
)

func (e *echoExec) Run(cmd *exec.Cmd) error {
	args := cmd.Args
	if i := slices.Index(args, "--"); i >= 0 && args[0] == "aps" {
		args = args[i+1:]
	}
	if args[0] != "lookup" {
		return os.ErrInvalid
	}
	var in strings.Builder
	if cmd.Stdin != nil {
		b := make([]byte, 4096)
		n, _ := cmd.Stdin.Read(b)
		in.Write(b[:n])
	}
	e.mu.Lock()
	e.stdins = append(e.stdins, in.String())
	e.mu.Unlock()
	_, _ = cmd.Stdout.Write([]byte(`{"name":"alpha","secret":"` + secretOut + `","note":"` + envSecret + `","items":[{"id":1,"secret":"` + secretOut + `"}]}`))
	_, _ = cmd.Stderr.Write([]byte("looked up " + args[1] + " with " + secretToken + "\n"))
	return nil
}

func (e *echoExec) Output(cmd *exec.Cmd) ([]byte, error) { return nil, e.Run(cmd) }

const lookupFlow = `flow: lookup
version: 0.1.0
description: One read with a secret in and a secret out.
on:
  - manual: {}
run_as: automation
privacy: local-only
tasks: none
vars:
  query: { default: alpha, type: string }
  passphrase: { default: "pass-3c7d1e90-var", type: string }
state:
  cursor: { default: "" }
  password: { default: "pw-default-9z8y7x" }
steps:
  - id: find
    uses: demo/lookup@1
    with: { query: "{{ vars.query }}", token: "{{ trigger.token }}" }
`

const lookupNode = `node: demo/lookup@1
kind: command
description: Look something up.
argv: [lookup, "{{ in.query }}"]
stdin: { json: { query: "{{ in.query }}", token: "{{ in.token }}" } }
inputs:
  query: { type: string, required: true }
  token: { type: string, required: true }
outputs:
  from: stdout
  format: json
  schema: { type: object, required: [name] }
side_effect: read
privacy: local
capabilities: []
redact: [in.token, out.secret, passphrase, password]
timeout: 30s
`

func TestRedactAndSecretsNeverReachTheRecording(t *testing.T) {
	t.Setenv("FLO_SECRET_DEFAULT_AUTOMATION_KEY", envSecret)
	live := &echoExec{}
	root := t.TempDir()
	e, err := serve.Open(bg, serve.Config{Root: root, Owner: "jad", NoSync: true, Exec: live})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	bundle := filepath.Join(t.TempDir(), "lookup")
	for name, body := range map[string]string{"flow.yaml": lookupFlow, "nodes/lookup.yaml": lookupNode} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(bundle, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := serve.ReadSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Services.Starter.Start(bg, core.StartRequest{
		Tenant: core.DefaultTenant, Flow: "lookup", Version: a.Version.Hash, StartedBy: "jad", Record: true,
		Vars:    map[string]any{"query": "alpha", "passphrase": varSecret},
		Trigger: core.TriggerInfo{Payload: map[string]any{"token": secretToken, "note": "kept"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(bg, core.DefaultTenant, res.Run.ID); err != nil {
		t.Fatal(err)
	}
	run, _ := e.Services.Runs.GetRun(bg, core.DefaultTenant, res.Run.ID)
	if run.Status != core.RunSucceeded {
		t.Fatalf("the run: %s %+v", run.Status, run.Failure)
	}
	if len(live.stdins) != 1 || !strings.Contains(live.stdins[0], secretToken) {
		t.Fatalf("the live call did not get the real token: %q", live.stdins)
	}

	dir, _ := e.Recordings.Dir(core.DefaultTenant, run.ID) // every file: cassettes, trigger, vars, state, meta
	var all strings.Builder
	files := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p) //nolint:gosec // a temp recording
			all.Write(b)
			files++
		}
		return nil
	})
	if files < 6 {
		t.Fatalf("only %d recording files", files)
	}
	text := all.String()
	for what, secret := range map[string]string{"a trigger field under a redact path": secretToken, "a secret-looking var": varSecret, "a state key named by a redact path": stateSecret, "a redacted input": secretToken, "a redacted output field": secretOut, "a secret of the environment": envSecret} {
		if strings.Contains(text, secret) {
			t.Errorf("the recording keeps %s (%s)", what, secret)
		}
	}
	for _, kept := range []string{"alpha", "name"} {
		if !strings.Contains(text, kept) {
			t.Errorf("the recording lost %q, which is not redacted", kept)
		}
	}
	trig, _, _ := e.Recordings.ReadDoc(core.DefaultTenant, run.ID, runrec.TriggerFile)
	state, _, _ := e.Recordings.ReadDoc(core.DefaultTenant, run.ID, runrec.StateFile)
	if _, ok := trig["token"]; ok || state["cursor"] != "" {
		t.Errorf("recorded trigger %v, state %v", trig, state)
	}
	meta, _, _ := e.Recordings.Load(core.DefaultTenant, run.ID)
	if meta.Privacy != "local-only" || meta.MayLeave() || !slices.Contains(meta.Redacted, "in.token") {
		t.Errorf("a local-only flow: %+v", meta)
	}

	// what was dropped is asked for again, never guessed
	_, err = e.ReplayRun(bg, core.DefaultTenant, serve.ReplayRequest{Run: run.ID, Caller: "jad"})
	if !core.IsCode(err, core.CodeUsage) || !strings.Contains(err.Error(), "recorded input redacted: ") {
		t.Fatalf("a replay without the dropped input: %v", err)
	}
	rep, err := e.ReplayRun(bg, core.DefaultTenant, serve.ReplayRequest{
		Run: run.ID, Caller: "jad",
		Trigger: map[string]any{"token": secretToken}, Vars: map[string]any{"query": "alpha", "passphrase": "x"},
		State: map[string]any{"cursor": "", "password": "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "succeeded" || rep.Calls != 1 || rep.Replayed != 1 || len(live.stdins) != 1 {
		t.Errorf("replay = %+v, live calls %d", rep, len(live.stdins))
	}
	if rep.Matches || len(rep.Diffs) != 1 || !strings.Contains(rep.Diffs[0], "find") {
		t.Errorf("a redacted output is not in the replay, which says so: %v", rep.Diffs)
	}
}

// A replay starts from the state the run started from, not the declared
// defaults and not what the engine holds by now.
func TestReplayStartsFromTheRecordedState(t *testing.T) {
	r := newRig(t)
	v := r.apply(t, "dependency-watch", "vcs.read")
	first := r.startRecorded(t, "dependency-watch", v.Hash) // commits seen_adv and seen_rel
	second := r.startRecorded(t, "dependency-watch", v.Hash)
	state, _, _ := r.e.Recordings.ReadDoc(core.DefaultTenant, second.ID, runrec.StateFile)
	if seen, _ := state["seen_adv"].([]any); len(seen) != 2 {
		t.Fatalf("the second run did not start from the first one's state: %v", state)
	}
	if st, _, _ := r.e.Recordings.ReadDoc(core.DefaultTenant, first.ID, runrec.StateFile); len(st["seen_adv"].([]any)) != 0 {
		t.Fatalf("the first run started from %v", st)
	}
	for _, id := range []core.RunID{first.ID, second.ID} {
		res, err := r.replay(t, id)
		if err != nil || !res.Matches {
			t.Errorf("replay of %s: %v %+v", id, err, res.Diffs)
		}
	}
}
