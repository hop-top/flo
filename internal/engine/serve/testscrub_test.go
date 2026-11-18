//go:build !windows

package serve_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hop.top/kit/go/storage/secret"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/flowtest"
)

const (
	storeSecret = "store-secret-6a1f0c93" //nolint:gosec // a fake: resolved from a secret store, not the environment
	argvSecret  = "argv-tok-2e7b5d18"     //nolint:gosec // a fake: a redacted input that a plan renders
)

// echoStore resolves every secret to storeSecret: a store that is not the
// environment, so only the engine's own log of resolved secrets can scrub it.
type echoStore struct{}

func (echoStore) Get(_ context.Context, key string) (*secret.Secret, error) {
	return &secret.Secret{Key: key, Value: []byte(storeSecret)}, nil
}
func (echoStore) List(context.Context, string) ([]string, error) { return nil, nil }
func (echoStore) Exists(context.Context, string) (bool, error)   { return true, nil }

// echoHTTP answers the declared HTTP node with a body that holds the secret
// the request authenticated with.
type echoHTTP struct {
	mu    sync.Mutex
	calls int
}

func (h *echoHTTP) Do(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	auth := req.Header.Get("Authorization")
	body := `{"echo":"` + strings.TrimPrefix(auth, "Bearer ") + `","visible":"kept-http"}`
	return &http.Response{
		StatusCode: 200, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req,
		Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"sid=" + storeSecret}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}, nil
}

const scrubFlow = `flow: lookup
version: 0.1.0
on:
  - manual: {}
run_as: automation
privacy: local-only
tasks: none
state:
  cursor: { default: "" }
  password: { default: "pw-default-9z8y7x" }
steps:
  - id: find
    uses: demo/lookup@1
    with: { query: alpha, token: "{{ trigger.token }}" }
  - id: remote
    uses: demo/remote@1
    depends_on: [find]
  - id: push
    uses: demo/push@1
    depends_on: [remote]
    with: { token: "` + argvSecret + `" }
`

const remoteNode = `node: demo/remote@1
kind: http
description: Ask a service.
request:
  method: GET
  url: https://svc.example.invalid/v1/echo
  auth: { secret: svc-token, scheme: bearer }
inputs: {}
outputs:
  from: body
  schema: { type: object }
side_effect: read
privacy: local
capabilities: []
timeout: 30s
`

const pushNode = `node: demo/push@1
kind: command
description: Write somewhere shared.
argv: [push]
stdin: { json: { token: "{{ in.token }}" } }
inputs:
  token: { type: string, required: true }
outputs: { from: stdout, format: json, schema: { type: object } }
side_effect: write-shared
privacy: local
capabilities: []
redact: [in.token]
timeout: 30s
`

// scrubRig is an engine whose recordings meet secrets of every kind.
func scrubRig(t *testing.T) (*serve.Engine, string) {
	t.Helper()
	t.Setenv("FLO_SECRET_DEFAULT_AUTOMATION_KEY", envSecret)
	e, err := serve.Open(bg, serve.Config{Root: t.TempDir(), Owner: "jad", NoSync: true, Exec: &echoExec{}, HTTP: &echoHTTP{}, Secrets: echoStore{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(bg) })
	bundle := filepath.Join(t.TempDir(), "lookup")
	for name, body := range map[string]string{
		"flow.yaml": scrubFlow, "nodes/lookup.yaml": lookupNode, "nodes/remote.yaml": remoteNode, "nodes/push.yaml": pushNode,
	} {
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
	if _, err := e.Flows.Apply(bg, core.DefaultTenant, "jad", src); err != nil {
		t.Fatal(err)
	}
	return e, bundle
}

func recordFiles(t *testing.T, bundle string) (string, int) {
	t.Helper()
	var all strings.Builder
	n := 0
	_ = filepath.WalkDir(filepath.Join(bundle, "record"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p) //nolint:gosec // a temp bundle
			all.Write(b)
			n++
		}
		return nil
	})
	return all.String(), n
}

func TestRecordedTestKeepsNoSecretsInTheBundle(t *testing.T) {
	e, bundle := scrubRig(t)
	res, err := e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{
		Flow: "lookup", Caller: "jad", Mode: flowtest.ModeRecord, AllowLive: true,
		Trigger: map[string]any{"token": secretToken, "note": "kept-trigger"},
	})
	if err != nil || !res.Passed {
		t.Fatalf("%+v %v", res, err)
	}
	text, files := recordFiles(t, bundle)
	if files < 7 { // 2 cassettes (req, resp) x 2 calls, trigger, plans, state
		t.Fatalf("only %d record files:\n%s", files, text)
	}
	for what, s := range map[string]string{
		"a trigger field under a redact path": secretToken,
		"a redacted output field":             secretOut,
		"a FLO_SECRET_* value":                envSecret,
		"a resolved secret":                   storeSecret,
		"a state key named by a redact path":  stateSecret,
		"a redacted input a plan renders":     argvSecret,
	} {
		if strings.Contains(text, s) {
			t.Errorf("the bundle keeps %s (%s)", what, s)
		}
	}
	for _, kept := range []string{"alpha", "kept-trigger", "kept-http", "cursor"} {
		if !strings.Contains(text, kept) {
			t.Errorf("the bundle lost %q, which is not redacted", kept)
		}
	}
	trig, _ := os.ReadFile(filepath.Join(bundle, "record", "trigger.json")) //nolint:gosec // a temp bundle
	state, _ := os.ReadFile(filepath.Join(bundle, "record", "state.json"))  //nolint:gosec // a temp bundle
	if strings.Contains(string(trig), `"token"`) || strings.Contains(string(state), `"password"`) {
		t.Errorf("a redacted key was kept: trigger %s, state %s", trig, state)
	}
}

// A recording started from a seed file keeps no redacted value of it either.
func TestRecordedSeedStateIsScrubbed(t *testing.T) {
	e, bundle := scrubRig(t)
	seed := writeFile(t, filepath.Join(t.TempDir(), "seed.json"), `{"cursor":"c-9","password":"`+stateSecret+`"}`)
	res, err := e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{
		Flow: "lookup", Caller: "jad", Mode: flowtest.ModeRecord, AllowLive: true, State: seed,
		Trigger: map[string]any{"token": secretToken},
	})
	if err != nil || !res.Passed {
		t.Fatalf("%+v %v", res, err)
	}
	state, _ := os.ReadFile(filepath.Join(bundle, "record", "state.json")) //nolint:gosec // a temp bundle
	if strings.Contains(string(state), stateSecret) || !strings.Contains(string(state), "c-9") {
		t.Errorf("state.json = %s", state)
	}
}

func TestRecordRefusesLiveNodesWithoutAllowLive(t *testing.T) {
	e, bundle := scrubRig(t)
	req := serve.TestRequest{Flow: "lookup", Caller: "jad", Mode: flowtest.ModeRecord, Trigger: map[string]any{"token": secretToken}}
	_, err := e.TestFlow(bg, core.DefaultTenant, req)
	if !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("no --allow-live: %v", err)
	}
	// both declared reads are named, and the write-shared node (planned, not run) is not
	for _, want := range []string{"find", "remote", "demo/lookup@1", "--allow-live"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "push") {
		t.Errorf("a planned write is not run live: %v", err)
	}
	if _, n := recordFiles(t, bundle); n != 0 {
		t.Errorf("a refused recording wrote %d files", n)
	}
	plan, err := e.PlanTest(bg, core.DefaultTenant, req)
	if err != nil || len(plan.Live) != 2 || plan.Live[0].Step != "find" || plan.Live[1].SideEffect != "read" {
		t.Errorf("the dry-run list: %+v %v", plan.Live, err)
	}
	rep, err := e.PlanTest(bg, core.DefaultTenant, serve.TestRequest{Flow: "lookup", Caller: "jad", Mode: flowtest.ModeReplay})
	if err != nil || len(rep.Live) != 0 {
		t.Errorf("a replay runs nothing live: %+v %v", rep.Live, err)
	}
	req.AllowLive = true
	if res, err := e.TestFlow(bg, core.DefaultTenant, req); err != nil || !res.Passed {
		t.Errorf("--allow-live: %+v %v", res, err)
	}
}

// Built-in nodes and nodes that only compute or are planned are not live.
func TestRecordOfBuiltinsNeedsNoAllowLive(t *testing.T) {
	r := newRig(t)
	r.apply(t, "inbox-triage", "mail.read", "mail.write")
	plan, err := r.e.PlanTest(bg, core.DefaultTenant, serve.TestRequest{Flow: "inbox-triage", Caller: "jad", Mode: flowtest.ModeRecord})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range plan.Live {
		if strings.HasPrefix(n.Node, "flo/") || (n.SideEffect != "read" && n.SideEffect != "write-local") {
			t.Errorf("not live: %+v", n)
		}
	}
}

// The stored result of a test (tests/<flow>/<hash>.json) is scrubbed as a
// recording is, in record and in replay mode.
func TestStoredTestResultKeepsNoSecrets(t *testing.T) {
	e, _ := scrubRig(t)
	for _, mode := range []flowtest.Mode{flowtest.ModeRecord, flowtest.ModeReplay} {
		res, err := e.TestFlow(bg, core.DefaultTenant, serve.TestRequest{
			Flow: "lookup", Caller: "jad", Mode: mode, AllowLive: true,
			Trigger: map[string]any{"token": secretToken, "note": "kept-trigger"},
		})
		if err != nil || !res.Passed {
			t.Fatalf("%s: %+v %v", mode, res, err)
		}
		p := filepath.Join(e.Store.Registry().Dir(core.DefaultTenant), "tests", res.Flow, res.Hash+".json")
		b, err := os.ReadFile(p) //nolint:gosec // a temp data root
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for what, s := range map[string]string{
			"a trigger field under a redact path": secretToken,
			"a redacted output field":             secretOut,
			"a FLO_SECRET_* value":                envSecret,
			"a resolved secret":                   storeSecret,
			"a redacted input a plan renders":     argvSecret,
		} {
			if strings.Contains(text, s) {
				t.Errorf("%s: the stored result keeps %s (%s)", mode, what, s)
			}
		}
		for _, kept := range []string{"alpha", "kept-http"} {
			if !strings.Contains(text, kept) {
				t.Errorf("%s: the stored result lost %q, which is not redacted:\n%s", mode, kept, text)
			}
		}
		if stored, ok, err := e.TestResults.Load(core.DefaultTenant, res.Flow, res.Hash); err != nil || !ok || !stored.Passed || stored.Hash != res.Hash || stored.Digest != res.Digest {
			t.Errorf("%s: gate fields changed: %+v %v %v", mode, stored, ok, err)
		}
	}
}
