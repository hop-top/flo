//go:build !windows

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
)

func TestReadTriggerFile(t *testing.T) {
	file := func(body string) string {
		p := filepath.Join(t.TempDir(), "event.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, path, stdin string
		want              map[string]any
		wantErr           bool
		errHas, errNot    string
	}{
		{name: "unset: no payload", path: ""},
		{name: "stdin object", path: "-", stdin: `{"event":{"id":"e1"}}`, want: map[string]any{"event": map[string]any{"id": "e1"}}},
		{name: "stdin not json", path: "-", stdin: `nope`, wantErr: true, errHas: "stdin", errNot: "open"},
		{name: "stdin not an object", path: "-", stdin: `[1,2]`, wantErr: true, errHas: "stdin", errNot: "open"},
		{name: "stdin empty", path: "-", stdin: ``, wantErr: true, errHas: "stdin", errNot: "open"},
		{name: "file object", path: file(`{"a":"b"}`), stdin: `{"ignored":true}`, want: map[string]any{"a": "b"}},
		{name: "file missing", path: filepath.Join(t.TempDir(), "nope.json"), wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readTriggerFile(strings.NewReader(c.stdin), c.path)
			if c.wantErr {
				wantCode(t, err, core.CodeUsage)
				if c.errHas != "" && !strings.Contains(err.Error(), c.errHas) {
					t.Errorf("error %q does not name %q", err, c.errHas)
				}
				if c.errNot != "" && strings.Contains(err.Error(), c.errNot) {
					t.Errorf("error %q treats stdin as a file", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("payload = %v, want %v", got, c.want)
			}
			for k := range c.want {
				if _, ok := got[k]; !ok {
					t.Fatalf("payload = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// run create --trigger-file - takes the payload from standard input.
func TestRunCreateTriggerFileStdin(t *testing.T) {
	r := newFlowRig(t)
	for _, args := range [][]string{
		{"flow", "apply", "-f", r.bundle, "--yes"},
		{"flow", "test", "dependency-watch", "--record", "--yes", "--allow-live"},
		{"flow", "set", "dependency-watch", "--state", "active", "--yes"},
	} {
		if _, _, err := r.run(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	create := func(stdin string, extra ...string) (string, error) {
		out, _, err := r.runIn(stdin, append([]string{"run", "create", "--flow", "dependency-watch", "--format", "json"}, extra...)...)
		return out, err
	}

	out, err := create(`{"event":{"id":"e1"}}`, "--trigger-file", "-", "--idempotency-key", "k-stdin")
	if err != nil {
		t.Fatalf("stdin payload: %v", err)
	}
	var made runCreated
	decodeJSON(t, out, &made)
	run, err := r.eng.Services.Runs.GetRun(context.Background(), core.DefaultTenant, core.RunID(made.Run))
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := run.Trigger.Payload["event"].(map[string]any)
	if ev["id"] != "e1" {
		t.Errorf("run trigger payload = %v, want the stdin object", run.Trigger.Payload)
	}

	for name, in := range map[string]string{"not json": "nope", "not an object": `"s"`, "empty": ""} {
		_, err := create(in, "--trigger-file", "-", "--idempotency-key", "k-"+name)
		if !core.IsCode(err, core.CodeUsage) {
			t.Errorf("%s: err = %v, want usage", name, err)
		}
	}

	// a real path still works, and stdin is then ignored
	p := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(p, []byte(`{"event":{"id":"from-file"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = create(`{"event":{"id":"stdin"}}`, "--trigger-file", p, "--idempotency-key", "k-file")
	if err != nil {
		t.Fatalf("file payload: %v", err)
	}
	decodeJSON(t, out, &made)
	run, _ = r.eng.Services.Runs.GetRun(context.Background(), core.DefaultTenant, core.RunID(made.Run))
	ev, _ = run.Trigger.Payload["event"].(map[string]any)
	if ev["id"] != "from-file" {
		t.Errorf("run trigger payload = %v, want the file's", run.Trigger.Payload)
	}
}
