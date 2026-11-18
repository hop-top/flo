package runrec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/runrec"
)

func TestRedactorDropsPathsFromJSONInsideText(t *testing.T) {
	r := runrec.NewRedactor([]string{"in.token", "out.items.secret", "headers[]"})
	payload := map[string]any{
		"stdin":  `{"query":"q","token":"t0k3n"}`,
		"stdout": `{"auth":{"token":"deep"},"items":[{"id":1,"secret":"s"},{"id":2}],"name":"n"}`,
		"argv":   []any{"lookup", "q"},
		"token":  "structure keys are not node data and stay",
	}
	got := r.Payload(payload).(map[string]any)
	if s := got["stdin"].(string); strings.Contains(s, "t0k3n") || !strings.Contains(s, `"query":"q"`) {
		t.Errorf("stdin = %s", s)
	}
	if s := got["stdout"].(string); strings.Contains(s, "deep") || strings.Contains(s, `"secret"`) || !strings.Contains(s, `"name":"n"`) || !strings.Contains(s, `"id":2`) {
		t.Errorf("stdout = %s", s)
	}
	if got["token"] != payload["token"] {
		t.Errorf("a key of the cassette itself was dropped: %v", got)
	}
	if payload["stdin"] != `{"query":"q","token":"t0k3n"}` {
		t.Error("the payload was changed in place")
	}
}

func TestRedactorScrubsKnownValuesFromAnyText(t *testing.T) {
	r := runrec.NewRedactor(nil, "hunter2-value", "ab") // "ab" is too short to scrub
	got := r.Text("Authorization: hunter2-value; and ab")
	if got != "Authorization: [redacted]; and ab" {
		t.Errorf("got %q", got)
	}
	nested := r.Payload(map[string]any{"files": []any{map[string]any{"content": `{"k":"hunter2-value"}`}}}).(map[string]any)
	if strings.Contains(nested["files"].([]any)[0].(map[string]any)["content"].(string), "hunter2-value") {
		t.Errorf("a value in a nested file survived: %v", nested)
	}
}

func TestRedactorFindsTheValuesOfRedactedInputs(t *testing.T) {
	r := runrec.NewRedactor([]string{"in.token", "in.creds.pass"})
	vals := r.ValuesAt(map[string]any{"query": "q", "token": "t0k3n-xyz", "creds": map[string]any{"pass": "pw-1234", "user": "u"}})
	if len(vals) != 2 {
		t.Fatalf("values = %v", vals)
	}
	if got := r.With(vals...).Text("curl --user u:pw-1234 t0k3n-xyz"); strings.Contains(got, "pw-1234") || strings.Contains(got, "t0k3n-xyz") {
		t.Errorf("got %q", got)
	}
}

func newStore(t *testing.T) (*runrec.Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runs")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	return runrec.NewStore(func(core.TenantID) string { return root }), root
}

func TestStoreKeepsOwnerOnlyAndRefusesBadRunIDs(t *testing.T) {
	s, root := newStore(t)
	if err := s.Open(core.DefaultTenant, "run_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(core.DefaultTenant, runrec.Meta{Run: "run_1", Status: runrec.StatusRecording}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "run_1.rec"), filepath.Join(root, "run_1.rec", "cassettes"), filepath.Join(root, "run_1.rec", runrec.MetaFile)} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: %v %v", p, info, err)
		}
	}
	for _, bad := range []string{"../x", "a/b", ""} {
		if _, err := s.Dir(core.DefaultTenant, core.RunID(bad)); err == nil {
			t.Errorf("run id %q was accepted", bad)
		}
	}
}

func TestMayLeaveOnlyWhenTheFlowAllowsCloud(t *testing.T) {
	for privacy, want := range map[string]bool{"local-only": false, "cloud-allowed": true, "": false, "other": false} {
		if got := (runrec.Meta{Privacy: privacy}).MayLeave(); got != want {
			t.Errorf("MayLeave(%q) = %v", privacy, got)
		}
	}
}

func TestReplayableNeedsAClosedWholeRecording(t *testing.T) {
	if err := (runrec.Meta{Run: "r", Status: runrec.StatusComplete}).Replayable(); err != nil {
		t.Error(err)
	}
	if err := (runrec.Meta{Run: "r", Status: runrec.StatusRecording}).Replayable(); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Errorf("open: %v", err)
	}
	if err := (runrec.Meta{Run: "r", Status: runrec.StatusComplete, Partial: []string{"a call"}}).Replayable(); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Errorf("partial: %v", err)
	}
}

func TestPruneDeletesOldRecordingsOnly(t *testing.T) {
	s, root := newStore(t)
	now := time.Now()
	for run, age := range map[core.RunID]time.Duration{"run_old": 100 * 24 * time.Hour, "run_new": time.Hour} {
		if err := s.Save(core.DefaultTenant, runrec.Meta{Run: string(run), CreatedAt: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "run_old.jsonl"), []byte("{}\n"), 0o600); err != nil { // a run log is not a recording
		t.Fatal(err)
	}
	n, err := s.Prune(core.DefaultTenant, now.Add(-core.OutputRetention))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if s.Exists(core.DefaultTenant, "run_old", runrec.MetaFile) || !s.Exists(core.DefaultTenant, "run_new", runrec.MetaFile) {
		t.Error("the wrong recording went")
	}
	if _, err := os.Stat(filepath.Join(root, "run_old.jsonl")); err != nil {
		t.Errorf("the run log was touched: %v", err)
	}
}

func TestPruneByKeepsEachFlowAsLongAsItSays(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	day := 24 * time.Hour
	for _, m := range []runrec.Meta{
		{Run: "a_old", Flow: "a", CreatedAt: now.Add(-40 * day)},
		{Run: "a_new", Flow: "a", CreatedAt: now.Add(-10 * day)},
		{Run: "b_old", Flow: "b", CreatedAt: now.Add(-40 * day)},
		{Run: "nometa_flow", CreatedAt: now.Add(-40 * day)},
	} {
		if err := s.Save(core.DefaultTenant, m); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneBy(core.DefaultTenant, now, func(flow string) time.Duration {
		if flow == "a" {
			return 30 * day
		}
		return 90 * day
	})
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	for run, want := range map[core.RunID]bool{"a_old": false, "a_new": true, "b_old": true, "nometa_flow": true} {
		if got := s.Exists(core.DefaultTenant, run, runrec.MetaFile); got != want {
			t.Errorf("%s kept = %v, want %v", run, got, want)
		}
	}
}
