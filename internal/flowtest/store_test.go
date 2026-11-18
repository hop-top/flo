package flowtest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
)

func newStore(t *testing.T) *flowtest.Store {
	t.Helper()
	root := t.TempDir()
	return flowtest.NewStore(func(tn core.TenantID) string { return filepath.Join(root, string(tn)) })
}

func TestStoreRoundTripAndLatestWins(t *testing.T) {
	s := newStore(t)
	tn := core.DefaultTenant
	if _, ok, err := s.Load(tn, "f", "h1"); ok || err != nil {
		t.Fatalf("never tested: ok=%v err=%v", ok, err)
	}
	pass := flowtest.Result{Schema: flowtest.SchemaVersion, Flow: "f", Hash: "h1", Mode: flowtest.ModeReplay, Passed: true, At: time.Unix(1, 0).UTC()}
	if err := s.Save(tn, pass); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load(tn, "f", "h1")
	if err != nil || !ok || !got.Passed || got.Mode != flowtest.ModeReplay {
		t.Fatalf("got %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.Load(tn, "f", "h2"); ok {
		t.Error("another version has no result")
	}
	pass.Passed = false
	if err := s.Save(tn, pass); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Load(tn, "f", "h1"); got.Passed {
		t.Error("the latest test must win")
	}
}

func TestStoreRefusesPathsInKeys(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if err := s.Save(core.DefaultTenant, flowtest.Result{Flow: bad, Hash: "h"}); err == nil {
			t.Errorf("flow %q accepted", bad)
		}
	}
}

func TestBinds(t *testing.T) {
	r := flowtest.Result{Flow: "f", Hash: "h", Digest: "d", Passed: true}
	if !r.Binds("h", "d") || r.Binds("h2", "d") || r.Binds("h", "d2") {
		t.Error("a result binds one hash and one digest")
	}
	r.Passed = false
	if r.Binds("h", "d") {
		t.Error("a failing result binds nothing")
	}
}

func TestDigestMovesWithAnyFileButNotHiddenOnes(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("flow.yaml", "a")
	write("nodes/x.yaml", "b")
	d1, err := flowtest.Digest(dir)
	if err != nil || d1 == "" {
		t.Fatalf("digest %q %v", d1, err)
	}
	if d2, _ := flowtest.Digest(dir); d2 != d1 {
		t.Error("digest is not stable")
	}
	write(".git/HEAD", "x")
	write(".DS_Store", "x")
	if d2, _ := flowtest.Digest(dir); d2 != d1 {
		t.Error("hidden files moved the digest")
	}
	write("nodes/x.yaml", "changed")
	d3, _ := flowtest.Digest(dir)
	if d3 == d1 {
		t.Error("a node spec change must move the digest")
	}
	write("record/cassettes/exec-1.req.yaml", "r")
	if d4, _ := flowtest.Digest(dir); d4 == d3 {
		t.Error("a new cassette must move the digest")
	}
	if d, err := flowtest.Digest(""); d != "" || err != nil {
		t.Errorf("single file: %q %v", d, err)
	}
}
