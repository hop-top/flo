package serve_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

var bg = context.Background()

func testdata(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "testdata"}, parts...)...)
}

// open assembles an engine over a temporary root with the inbox pack
// installed and jad as owner.
func open(t *testing.T) *serve.Engine {
	t.Helper()
	root := t.TempDir()
	packs := filepath.Join(root, "packs")
	if err := os.MkdirAll(packs, 0o750); err != nil {
		t.Fatal(err)
	}
	copyTree(t, testdata("packs", "inbox"), filepath.Join(packs, "inbox"))
	e, err := serve.Open(bg, serve.Config{Root: root, Owner: "jad", NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	return e
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		b, err := os.ReadFile(p) //nolint:gosec // testdata
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600) //nolint:gosec // a temp directory
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenSeedsTheOwnerAndTheEngineProfile(t *testing.T) {
	e := open(t)
	if ok, _ := e.Grants.Covers(bg, core.DefaultTenant, "jad", "anything.at.all"); !ok {
		t.Error("the owner holds the wildcard")
	}
	for _, c := range []string{"mail.read", "mail.write"} { // what the inbox pack declares
		if ok, _ := e.Grants.Covers(bg, core.DefaultTenant, serve.DefaultEngineProfile, c); !ok {
			t.Errorf("the engine profile lacks %s", c)
		}
	}
	if ok, _ := e.Grants.Covers(bg, core.DefaultTenant, serve.DefaultEngineProfile, "vcs.read"); ok {
		t.Error("the engine profile holds what no pack declares")
	}
}

func TestPacksLoadAndDeclareCapabilities(t *testing.T) {
	e := open(t)
	if got := strings.Join(e.Nodes.Packs.Capabilities(), ","); got != "mail.read,mail.write" {
		t.Errorf("capabilities = %s", got)
	}
	info, err := e.Services.Nodes.Resolve(bg, core.NodeScope{Tenant: core.DefaultTenant}, "inbox/apply@1")
	if err != nil || info.Source.Layer != core.LayerPack || info.Source.Pack != "inbox" || info.Source.PackVersion != "0.1.0" {
		t.Errorf("inbox/apply@1 = %+v, %v", info.Source, err)
	}
	if _, err := e.Services.Nodes.Resolve(bg, core.NodeScope{Tenant: core.DefaultTenant}, "flo/approval@1"); err != nil {
		t.Errorf("the approval node is a built-in: %v", err)
	}
	if p, err := serve.LoadPacks(filepath.Join(t.TempDir(), "absent")); err != nil || len(p.Loaded) != 0 {
		t.Errorf("no packs directory is no packs: %v %v", p, err)
	}
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "broken"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "broken", serve.ManifestFile), []byte("pack: Broken\nversion: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serve.LoadPacks(bad); !core.IsCode(err, core.CodeInvalidNode) {
		t.Errorf("a broken pack: %v", err)
	}
}

func TestReadSource(t *testing.T) {
	dir := t.TempDir()
	flow := "flow: f\nversion: 0.1.0\nsteps:\n  - id: a\n    kind: exec\n    exec: { argv: [true] }\n"
	file := filepath.Join(dir, "f.yaml")
	bundleFlow := filepath.Join(dir, "b", "flow.yaml")
	if err := os.MkdirAll(filepath.Dir(bundleFlow), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, bundleFlow} {
		if err := os.WriteFile(p, []byte(flow), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if s, err := serve.ReadSource(file); err != nil || s.Bundle != "" || s.File != file {
		t.Errorf("a single file has no bundle: %+v %v", s, err)
	}
	if s, err := serve.ReadSource(filepath.Dir(bundleFlow)); err != nil || s.Bundle != filepath.Dir(bundleFlow) || s.File != bundleFlow {
		t.Errorf("a directory is a bundle: %+v %v", s, err)
	}
	if s, err := serve.ReadSource(bundleFlow); err != nil || s.Bundle != filepath.Dir(bundleFlow) {
		t.Errorf("flow.yaml makes its directory the bundle: %+v %v", s, err)
	}
	if _, err := serve.ReadSource(dir); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("a directory without flow.yaml: %v", err)
	}
	if _, err := serve.ReadSource(filepath.Join(dir, "nope.yaml")); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("a missing file: %v", err)
	}
}

func TestApplyChecksGrantsThenStoresOnlyNewContent(t *testing.T) {
	e := open(t)
	src, err := serve.ReadSource(testdata("flows", "inbox-triage"))
	if err != nil {
		t.Fatal(err)
	}
	// automation holds nothing: V18
	if _, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src); !core.IsCode(err, core.CodeGrantDenied) {
		t.Fatalf("apply without grants: %v", err)
	}
	for _, c := range []string{"mail.read", "mail.write"} {
		if _, err := e.Grants.Set(bg, core.DefaultTenant, "automation", c); err != nil {
			t.Fatal(err)
		}
	}
	a, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil || !a.Created || a.Version.State != core.FlowDraft || a.Version.Bundle == "" {
		t.Fatalf("apply = %+v %v", a, err)
	}
	b, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil || b.Created || b.Version.Hash != a.Version.Hash {
		t.Errorf("the same bytes are the same version: %+v %v", b, err)
	}
	// a caller who may not act as the flow's profile
	if _, err := e.Flows.Apply(bg, core.DefaultTenant, "noor", src); !core.IsCode(err, core.CodeGrantDenied) {
		t.Errorf("apply as someone who may not use automation: %v", err)
	}
}

func TestApplyRefusesAFlowThatDoesNotValidate(t *testing.T) {
	e := open(t)
	cases := map[string]struct {
		doc  string
		code core.Code
	}{
		"not a flow":        {"flow: f\nversion: 0.1.0\n", core.CodeInvalidFlow},
		"a node nobody has": {"flow: f\nversion: 0.1.0\nsteps:\n  - id: a\n    uses: nobody/home@1\n", core.CodeNodeNotFound},
		"an agent step":     {"flow: f\nversion: 0.1.0\nsteps:\n  - id: a\n    kind: agent\n", core.CodeEngineFeature},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", serve.Source{File: "f.yaml", Data: []byte(c.doc)})
			if !core.IsCode(err, c.code) {
				t.Errorf("err = %v, want %s", err, c.code)
			}
			if fs, _ := e.Services.Flows.ListFlows(bg, core.DefaultTenant); len(fs) != 0 {
				t.Errorf("a refused flow was stored: %+v", fs)
			}
		})
	}
}

func TestSetStateActivatesPausesAndSyncsTriggers(t *testing.T) {
	e := open(t)
	if _, err := e.Grants.Set(bg, core.DefaultTenant, "automation", "vcs.read"); err != nil {
		t.Fatal(err)
	}
	src, err := serve.ReadSource(testdata("flows", "dependency-watch"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src)
	if err != nil {
		t.Fatal(err)
	}
	triggers := func() int {
		ts, _ := e.Services.Triggers.ListTriggers(bg, core.DefaultTenant, "dependency-watch")
		return len(ts)
	}
	if triggers() != 0 {
		t.Fatal("a draft registers no triggers")
	}
	if _, err := e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowPaused); !core.IsCode(err, core.CodeFlowNotActive) {
		t.Errorf("pausing a flow that was never active: %v", err)
	}
	// the gate: a caller without a grant to act as the flow's profile
	if _, err := e.Flows.SetState(bg, core.DefaultTenant, "noor", "dependency-watch", "", core.FlowActive); !core.IsCode(err, core.CodeGrantDenied) {
		t.Errorf("activate as someone who may not: %v", err)
	}
	r, err := e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowActive)
	if err != nil || !r.Changed || r.Version.State != core.FlowActive || r.Version.Hash != a.Version.Hash {
		t.Fatalf("activate = %+v %v", r, err)
	}
	if triggers() != 2 {
		t.Errorf("activation registers the triggers: %d", triggers())
	}
	if r, err = e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowActive); err != nil || r.Changed {
		t.Errorf("activating the active version is a no-op: %+v %v", r, err)
	}
	if r, err = e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowPaused); err != nil || !r.Changed || r.Version.State != core.FlowPaused {
		t.Fatalf("pause = %+v %v", r, err)
	}
	ts, _ := e.Services.Triggers.ListTriggers(bg, core.DefaultTenant, "dependency-watch")
	for _, tr := range ts {
		if tr.State != core.TriggerPaused {
			t.Errorf("a paused flow's trigger %s is %s", tr.ID, tr.State)
		}
	}
	if _, err := e.Services.Starter.Start(bg, core.StartRequest{Tenant: core.DefaultTenant, Flow: "dependency-watch", StartedBy: "jad"}); !core.IsCode(err, core.CodeFlowNotActive) {
		t.Errorf("a paused flow starts no run: %v", err)
	}
	if r, err = e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowPaused); err != nil || r.Changed {
		t.Errorf("pausing the paused version is a no-op: %+v %v", r, err)
	}
	if _, err := e.Flows.SetState(bg, core.DefaultTenant, "jad", "dependency-watch", "", core.FlowDraft); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("an unsettable state: %v", err)
	}
}

func TestValidatedGate(t *testing.T) {
	e := open(t)
	good, _ := serve.ReadSource(testdata("flows", "inbox-triage"))
	if err := e.Services.Gate.Allow(bg, core.DefaultTenant, core.FlowVersion{Name: "inbox-triage", Version: "0.1.0", Source: good.Data, Bundle: good.Bundle}); err != nil {
		t.Errorf("a valid version: %v", err)
	}
	bad := core.FlowVersion{Name: "f", Version: "0.1.0", Source: []byte("flow: f\nversion: 0.1.0\nsteps:\n  - id: a\n    uses: nobody/home@1\n")}
	if err := e.Services.Gate.Allow(bg, core.DefaultTenant, bad); !core.IsCode(err, core.CodeInvalidFlow) {
		t.Errorf("a version that does not validate: %v", err)
	}
}

func TestStateFileTellsALiveEngineFromAStaleOne(t *testing.T) {
	root := t.TempDir()
	if _, ok, err := serve.ReadState(root); ok || err != nil {
		t.Fatalf("no engine yet: %v %v", ok, err)
	}
	e, err := serve.Open(bg, serve.Config{Root: root, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.Serve(ctx) }()
	var st serve.State
	end := time.Now().Add(10 * time.Second)
	for {
		if s, ok, _ := serve.ReadState(root); ok {
			st = s
			break
		}
		if time.Now().After(end) {
			t.Fatal("the engine wrote no state file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.PID != os.Getpid() || !st.Alive() || st.Root != root {
		t.Errorf("state = %+v", st)
	}
	// the refusal of a second live engine is covered end to end: here both
	// would be this process
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
	if _, ok, _ := serve.ReadState(root); ok {
		t.Error("a stopped engine leaves its state file")
	}
	_ = e.Close(bg)
	if (serve.State{PID: 0}).Alive() || (serve.State{PID: 1 << 30}).Alive() {
		t.Error("a pid that is not a process is not alive")
	}
}
