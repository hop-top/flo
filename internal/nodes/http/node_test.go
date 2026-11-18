package httpnode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"hop.top/kit/go/runtime/sideeffect/real"
	"hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/memory"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	httpnode "hop.top/flo/internal/nodes/http"
	"hop.top/flo/internal/nodes/schema"
	"hop.top/flo/spec/node"
)

// fakeSecret is built at runtime so it matches no provider key format.
func fakeSecret() string { return "tok" + "-" + "fixture" + "-9f3a1c" }

func spec(t *testing.T, y string) *node.Spec {
	t.Helper()
	s, err := node.Decode([]byte(y), "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newExec(secrets core.Secrets) *httpnode.Executor {
	return &httpnode.Executor{HTTP: real.NewHTTP(httpnode.NewClient()), Secrets: secrets}
}

func invoke(s *node.Spec, in map[string]any, cfg map[string]any) core.Invocation {
	return core.Invocation{
		Tenant: "default", Run: core.RunContext{ID: "run_1", Flow: "f"}, StepID: "s",
		Kind: core.StepKindNode, Node: s, In: in, Config: cfg, RunAs: "noor", Attempt: 1,
	}
}

const findSpec = `
node: ctxt/find@1
kind: http
request:
  method: POST
  url: "{{ config.url }}/api/v1/find"
  headers: { Accept: application/json }
  query: { tag: "{{ in.tags }}", none: "{{ in.none }}" }
  json: { query: "{{ in.query }}", limit: "{{ in.limit }}" }
  auth: { secret: ctxt-token, scheme: bearer }
response: { success: [200] }
inputs:
  query: { type: string, required: true }
  limit: { type: integer, default: 10 }
  tags: { type: array, default: [] }
  none: { type: [string, 'null'] }
outputs:
  from: body.results
  schema: { type: array, items: { type: object } }
side_effect: read
privacy: local
capabilities: [knowledge.read]
timeout: 5s
`

func secretsFor(t *testing.T) *coretest.Secrets {
	s := &coretest.Secrets{}
	s.Put("noor", "ctxt-token", []byte(fakeSecret()))
	return s
}

func TestRequestAndOutput(t *testing.T) {
	var gotAuth, gotIdem, gotCT, gotAccept, gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotIdem, gotCT, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), r.Header.Get("Accept")
		gotQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"id":"a"},{"id":"b"}]}`)
	}))
	defer srv.Close()
	s := spec(t, findSpec)
	in, ierr := schema.ApplyInputs(s.Inputs, map[string]any{"query": "cobra", "tags": []any{"x", "y"}})
	if ierr != nil {
		t.Fatal(ierr)
	}
	inv := invoke(s, in, map[string]any{"url": srv.URL})
	inv.IdempotencyKey = "run_1/s/0"
	res, err := newExec(secretsFor(t)).Execute(context.Background(), inv)
	if err != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if gotAuth != "Bearer "+fakeSecret() || gotIdem != "run_1/s/0" || gotCT != "application/json" || gotAccept != "application/json" {
		t.Fatalf("headers: auth=%q idem=%q ct=%q accept=%q", gotAuth, gotIdem, gotCT, gotAccept)
	}
	if gotQuery != "tag=x&tag=y" {
		t.Fatalf("query = %q (list repeats, null omitted)", gotQuery)
	}
	if gotBody["query"] != "cobra" || gotBody["limit"] != float64(10) {
		t.Fatalf("typed json body: %v", gotBody)
	}
	if len(res.Output.([]any)) != 2 {
		t.Fatalf("output = %v", res.Output)
	}
	if cerr := schema.CheckOutput(s, res.Output); cerr != nil {
		t.Fatal(cerr)
	}
	// the secret is on the wire only
	assertNoSecret(t, res)
}

func assertNoSecret(t *testing.T, vals ...any) {
	t.Helper()
	for _, v := range vals {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), fakeSecret()) {
			t.Fatalf("secret leaked into %s", b)
		}
	}
}

func TestRateLimited429HonorsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "slow down")
	}))
	defer srv.Close()
	s := spec(t, findSpec)
	res, err := newExec(secretsFor(t)).Execute(context.Background(), invoke(s, map[string]any{"query": "q", "limit": 1, "tags": []any{}, "none": nil}, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	e := res.Err
	if res.Outcome != core.OutcomeFailed || e.Code != core.CodeProviderRateLimited || !e.Retryable() || res.Effect != core.EffectNone {
		t.Fatalf("res = %+v", res)
	}
	if e.Detail["retry_after_ms"] != int64(7000) {
		t.Fatalf("detail = %v", e.Detail)
	}
}

func TestStatusClasses(t *testing.T) {
	s := spec(t, findSpec)
	for name, tc := range map[string]struct {
		status    int
		retry     bool
		effect    core.Effect
		wantCode  core.Code
		retryHdr  string
		wantAfter bool
	}{
		"404 permanent":      {http.StatusNotFound, false, core.EffectDone, core.CodeRunFailed, "", false},
		"401 permanent":      {http.StatusUnauthorized, false, core.EffectDone, core.CodeRunFailed, "", false},
		"500 transient":      {http.StatusInternalServerError, true, core.EffectUnknown, core.CodeRunFailed, "", false},
		"503 safe transient": {http.StatusServiceUnavailable, true, core.EffectNone, core.CodeRunFailed, "3", true},
		"304 unlisted":       {http.StatusNotModified, false, core.EffectDone, core.CodeRunFailed, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryHdr != "" {
					w.Header().Set("Retry-After", tc.retryHdr)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			res, err := newExec(secretsFor(t)).Execute(context.Background(), invoke(s, map[string]any{"query": "q", "limit": 1, "tags": []any{}, "none": nil}, map[string]any{"url": srv.URL}))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != core.OutcomeFailed || res.Err.Code != tc.wantCode || res.Err.Retryable() != tc.retry || res.Effect != tc.effect {
				t.Fatalf("res = %+v err=%+v", res, res.Err)
			}
			if _, ok := res.Err.Detail["retry_after_ms"]; ok != tc.wantAfter {
				t.Fatalf("detail = %v", res.Err.Detail)
			}
		})
	}
}

func TestSecretNeverLeaks(t *testing.T) {
	// a server that echoes the request back into the body, a header and the error text
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"you_sent": r.Header.Get("Authorization")})
	}))
	defer srv.Close()
	s := spec(t, findSpec)
	inv := invoke(s, map[string]any{"query": "q", "limit": 1, "tags": []any{}, "none": nil}, map[string]any{"url": srv.URL})
	x := newExec(secretsFor(t))
	res, err := x.Execute(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := x.Plan(inv)
	if perr != nil {
		t.Fatal(perr)
	}
	if plan["auth"].(map[string]any)["secret"] != "ctxt-token" {
		t.Fatalf("plan names the secret: %v", plan)
	}
	assertNoSecret(t, res, res.Err, res.Raw, plan, res.Err.Message)
	if !strings.Contains(res.Err.Message, "[redacted]") {
		t.Fatalf("the echoed value should be redacted in the message: %q", res.Err.Message)
	}
}

func TestSecretMissing(t *testing.T) {
	s := spec(t, findSpec)
	res, err := newExec(&coretest.Secrets{}).Execute(context.Background(), invoke(s, map[string]any{"query": "q", "limit": 1, "tags": []any{}, "none": nil}, map[string]any{"url": "http://127.0.0.1:1"}))
	if err != nil || res.Err.Code != core.CodeSecretNotFound || res.Effect != core.EffectNone {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

const optionalAuthSpec = "node: x/a@1\nkind: http\nrequest: { method: GET, url: '%s', auth: { secret: k, scheme: bearer, optional: %t } }\ninputs: {}\noutputs: { schema: {} }\nside_effect: read\nprivacy: local\ncapabilities: []\n"

// optional auth: a missing secret sends no header, a present one is used; a
// required auth with a missing secret stays secret_not_found.
func TestOptionalAuth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		optional bool
		have     bool
		wantAuth string
		wantCode core.Code
	}{
		{"optional absent: no header", true, false, "", ""},
		{"optional present: bearer", true, true, "Bearer " + fakeSecret(), ""},
		{"required absent: secret_not_found", false, false, "", core.CodeSecretNotFound},
		{"required present: bearer", false, true, "Bearer " + fakeSecret(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			var gotAuth string
			var hasAuth bool
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				hits++
				_, hasAuth = r.Header["Authorization"]
				gotAuth = r.Header.Get("Authorization")
			}))
			defer srv.Close()
			s := spec(t, fmt.Sprintf(optionalAuthSpec, srv.URL, tc.optional))
			secrets := &coretest.Secrets{}
			if tc.have {
				secrets.Put("noor", "k", []byte(fakeSecret()))
			}
			res, err := newExec(secrets).Execute(context.Background(), invoke(s, nil, nil))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantCode != "" {
				if res.Err == nil || res.Err.Code != tc.wantCode || hits != 0 {
					t.Fatalf("res=%+v hits=%d", res, hits)
				}
				return
			}
			if res.Outcome != core.OutcomeOK || hits != 1 || gotAuth != tc.wantAuth || hasAuth != (tc.wantAuth != "") {
				t.Fatalf("res=%+v hits=%d auth=%q present=%v", res, hits, gotAuth, hasAuth)
			}
			assertNoSecret(t, res)
		})
	}
}

// an optional secret that cannot be read for a reason other than absence still
// fails: only absence is optional.
func TestOptionalAuthStoreError(t *testing.T) {
	s := spec(t, fmt.Sprintf(optionalAuthSpec, "http://127.0.0.1:1", true))
	res, err := newExec(brokenSecrets{}).Execute(context.Background(), invoke(s, nil, nil))
	if err != nil || res.Err == nil || res.Err.Code != core.CodeInternal {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

type brokenSecrets struct{}

func (brokenSecrets) Get(context.Context, core.TenantID, string, string) ([]byte, error) {
	return nil, core.Errorf(core.CodeInternal, "secret store: down")
}

func TestAuthSchemes(t *testing.T) {
	for scheme, check := range map[string]func(r *http.Request) bool{
		"basic":          func(r *http.Request) bool { u, p, ok := r.BasicAuth(); return ok && u == "ab" && p == "cd" },
		"header:X-Key":   func(r *http.Request) bool { return r.Header.Get("X-Key") == "ab:cd" },
		"bearer":         func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer ab:cd" },
		"header:X-Other": func(r *http.Request) bool { return r.Header.Get("X-Other") == "ab:cd" },
	} {
		t.Run(scheme, func(t *testing.T) {
			ok := false
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ok = check(r) }))
			defer srv.Close()
			s := spec(t, "node: x/a@1\nkind: http\nrequest: { method: GET, url: '"+srv.URL+"', auth: { secret: k, scheme: '"+scheme+"' } }\ninputs: {}\noutputs: { schema: {} }\nside_effect: read\nprivacy: local\ncapabilities: []\n")
			secrets := &coretest.Secrets{}
			secrets.Put("noor", "k", []byte("ab:cd"))
			res, err := newExec(secrets).Execute(context.Background(), invoke(s, nil, nil))
			if err != nil || res.Outcome != core.OutcomeOK || !ok {
				t.Fatalf("res=%+v err=%v ok=%v", res, err, ok)
			}
		})
	}
}

func TestRedirectsFollowOnlyForGet(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/final", http.StatusFound) })
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := spec(t, "node: x/g@1\nkind: http\nrequest: { method: GET, url: '"+srv.URL+"/start' }\ninputs: {}\noutputs: { schema: { type: object } }\nside_effect: read\nprivacy: local\ncapabilities: []\n")
	res, _ := newExec(nil).Execute(context.Background(), invoke(get, nil, nil))
	if res.Outcome != core.OutcomeOK || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("GET follows: res=%+v hits=%d", res, hits)
	}

	post := spec(t, "node: x/p@1\nkind: http\nrequest: { method: POST, url: '"+srv.URL+"/start', json: { a: 1 } }\ninputs: {}\noutputs: { schema: {} }\nside_effect: write-shared\nprivacy: local\ncapabilities: [tasks.write]\n")
	res, _ = newExec(nil).Execute(context.Background(), invoke(post, nil, nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeRunFailed || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("POST must not follow: res=%+v hits=%d", res, hits)
	}
}

func TestBodiesFormAndText(t *testing.T) {
	var ct, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ct, body = r.Header.Get("Content-Type"), string(b)
		_, _ = io.WriteString(w, "plain text answer")
	}))
	defer srv.Close()
	form := spec(t, "node: x/f@1\nkind: http\nrequest: { method: POST, url: '"+srv.URL+"', form: { a: '{{ in.a }}', l: '{{ in.l }}' } }\ninputs: { a: { type: string }, l: { type: array } }\noutputs: { schema: { type: string } }\nside_effect: write-shared\nprivacy: local\ncapabilities: [tasks.write]\n")
	res, _ := newExec(nil).Execute(context.Background(), invoke(form, map[string]any{"a": "x y", "l": []any{"1", "2"}}, nil))
	if res.Outcome != core.OutcomeOK || ct != "application/x-www-form-urlencoded" || body != "a=x+y&l=1&l=2" || res.Output != "plain text answer" {
		t.Fatalf("form: ct=%q body=%q res=%+v", ct, body, res)
	}
	text := spec(t, "node: x/t@1\nkind: http\nrequest: { method: PUT, url: '"+srv.URL+"', text: 'hello {{ in.a }}' }\ninputs: { a: { type: string } }\noutputs: { from: status, schema: { type: integer } }\nside_effect: write-shared\nprivacy: local\ncapabilities: [tasks.write]\n")
	res, _ = newExec(nil).Execute(context.Background(), invoke(text, map[string]any{"a": "w"}, nil))
	if res.Outcome != core.OutcomeOK || !strings.HasPrefix(ct, "text/plain") || body != "hello w" || res.Output != int64(200) {
		t.Fatalf("text: ct=%q body=%q res=%+v", ct, body, res)
	}
}

func TestFailWhenAndMapOverBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = io.WriteString(w, `{"items":[1,2,3],"errors":["bad"]}`)
	}))
	defer srv.Close()
	s := spec(t, "node: x/m@1\nkind: http\nrequest: { method: GET, url: '"+srv.URL+"' }\ninputs: {}\noutputs: { from: body, map: \"{{ {'n': size(out.items), 'errors': out.errors} }}\", fail_when: \"{{ size(out.errors) > 0 }}\", schema: {} }\nside_effect: read\nprivacy: local\ncapabilities: []\n")
	res, _ := newExec(nil).Execute(context.Background(), invoke(s, nil, nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeNodeReportedFailure || res.Output.(map[string]any)["n"] != int64(3) {
		t.Fatalf("res = %+v", res)
	}
}

func TestConnectionRefusedIsNoEffect(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens now
	s := spec(t, "node: x/c@1\nkind: http\nrequest: { method: POST, url: '"+url+"', json: {} }\ninputs: {}\noutputs: { schema: {} }\nside_effect: write-shared\nprivacy: local\ncapabilities: [tasks.write]\n")
	res, _ := newExec(nil).Execute(context.Background(), invoke(s, nil, nil))
	if res.Outcome != core.OutcomeFailed || res.Effect != core.EffectNone || !res.Err.Retryable() {
		t.Fatalf("res = %+v", res)
	}
}

func TestTimeoutIsUnknownEffect(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	s := spec(t, "node: x/s@1\nkind: http\ntimeout: 150ms\nrequest: { method: POST, url: '"+srv.URL+"', json: {} }\ninputs: {}\noutputs: { schema: {} }\nside_effect: write-shared\nprivacy: local\ncapabilities: [tasks.write]\n")
	res, _ := newExec(nil).Execute(context.Background(), invoke(s, nil, nil))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeTimeout || res.Effect != core.EffectUnknown {
		t.Fatalf("res = %+v", res)
	}
}

func TestBodyCapFlagsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", core.DefaultMaxOutput+10)))
	}))
	defer srv.Close()
	s := spec(t, "node: x/b@1\nkind: http\nrequest: { method: GET, url: '"+srv.URL+"' }\ninputs: {}\noutputs: { schema: { type: string } }\nside_effect: read\nprivacy: local\ncapabilities: []\n")
	res, _ := newExec(nil).Execute(context.Background(), invoke(s, nil, nil))
	if res.Outcome != core.OutcomeOK || !res.Truncated || len(res.Output.(string)) != core.DefaultMaxOutput {
		t.Fatalf("truncated=%v len=%d", res.Truncated, len(res.Output.(string)))
	}
}

func TestStoreSecretsAdapter(t *testing.T) {
	st := memory.New()
	ss := httpnode.StoreSecrets{Store: st, Prefix: "flo/"}
	if err := st.Set(context.Background(), ss.Key("default", "noor", "ctxt-token"), []byte(fakeSecret())); err != nil {
		t.Fatal(err)
	}
	got, err := ss.Get(context.Background(), "default", "noor", "ctxt-token")
	if err != nil || string(got) != fakeSecret() {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := ss.Get(context.Background(), "default", "noor", "nope"); !core.IsCode(err, core.CodeSecretNotFound) {
		t.Fatalf("err = %v", err)
	}
	var _ secret.Store = st
}
