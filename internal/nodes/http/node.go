// Package httpnode runs declared HTTP nodes (node.md §5): one request through
// kit's sideeffect.HTTP seam (so a recorder or a cassette can stand in), the
// response read into the node's output. A secret named by `auth` is resolved
// for the run's profile and appears only on the wire: never in the step
// record, the plan, an error or the output.
package httpnode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hop.top/kit/go/runtime/sideeffect"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/render"
	"hop.top/flo/spec/node"
)

// maxBody bounds the response body kept (node.md §4.1 caps, applied here too).
const maxBody = core.DefaultMaxOutput

// bodySnippet is how much of a failing response a message carries.
const bodySnippet = 512

const redacted = "[redacted]"

// Executor runs declared HTTP nodes.
type Executor struct {
	// HTTP sends the request. Build it with NewClient so redirects follow
	// the rule of node.md §5.2 (GET only).
	HTTP sideeffect.HTTP
	// Secrets resolves `auth.secret`.
	Secrets core.Secrets
	// Clock reads time for an HTTP-date Retry-After; nil is the system clock.
	Clock core.Clock
}

// Request is a node's rendered call, as a plan shows it: the secret is
// named, never held.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	// Body is the rendered body and ContentType its media type.
	Body        []byte
	ContentType string
	// Auth is the secret reference, if any.
	Auth *node.Auth
	// IdempotencyKey is sent as the Idempotency-Key header.
	IdempotencyKey string
}

// NewClient returns an *http.Client whose redirects follow node.md §5.2:
// followed for GET, never for another method (the 3xx is returned as the
// response, an unlisted status).
func NewClient() *http.Client {
	return &http.Client{CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if via[0].Method != http.MethodGet {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

// Plan renders the request without sending it (dry run). The secret is
// shown by name.
func (x *Executor) Plan(inv core.Invocation) (map[string]any, *core.Error) {
	req, err := render1(inv)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"method": req.Method, "url": req.URL}
	if len(req.Headers) > 0 {
		out["headers"] = req.Headers
	}
	if len(req.Body) > 0 {
		out["body"] = string(req.Body)
		out["content_type"] = req.ContentType
	}
	if req.Auth != nil {
		out["auth"] = map[string]any{"secret": req.Auth.Secret, "scheme": req.Auth.Scheme}
	}
	return out, nil
}

func render1(inv core.Invocation) (Request, *core.Error) {
	env, err := render.Shared()
	if err != nil {
		return Request{}, core.Wrap(core.CodeInternal, err)
	}
	spec := inv.Node
	if spec.Request == nil {
		return Request{}, core.Errorf(core.CodeInvalidNode, "%s: an http node needs request", spec.Node)
	}
	rq := spec.Request
	act := render.Scope{In: inv.In, Config: inv.Config, Run: inv.Run}.Activation()
	req := Request{Method: strings.ToUpper(rq.Method), Auth: rq.Auth, IdempotencyKey: inv.IdempotencyKey}
	raw, err := env.String(rq.URL, act)
	if err != nil {
		return Request{}, render.Fail("request.url", err)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Request{}, core.Errorf(core.CodeInvalidNode, "%s: request.url %q is not an absolute http(s) URL", spec.Node, raw)
	}
	if len(rq.Query) > 0 {
		qv, qerr := env.Value(rq.Query, act)
		if qerr != nil {
			return Request{}, render.Fail("request.query", qerr)
		}
		q := u.Query()
		for k, v := range qv.(map[string]any) { //nolint:errcheck,forcetypeassert // Value of a map is a map
			addQuery(q, k, v)
		}
		u.RawQuery = q.Encode()
	}
	req.URL = u.String()
	if req.Headers, err = env.StringMap(rq.Headers, act); err != nil {
		return Request{}, render.Fail("request.headers", err)
	}
	switch {
	case rq.JSON != nil:
		v, jerr := env.Value(rq.JSON, act)
		if jerr != nil {
			return Request{}, render.Fail("request.json", jerr)
		}
		if req.Body, err = json.Marshal(v); err != nil {
			return Request{}, core.Errorf(core.CodeEvalError, "request.json: %v", err)
		}
		req.ContentType = "application/json"
	case len(rq.Form) > 0:
		fv, ferr := env.Value(rq.Form, act)
		if ferr != nil {
			return Request{}, render.Fail("request.form", ferr)
		}
		f := url.Values{}
		for k, v := range fv.(map[string]any) { //nolint:errcheck,forcetypeassert // Value of a map is a map
			addQuery(f, k, v)
		}
		req.Body = []byte(f.Encode())
		req.ContentType = "application/x-www-form-urlencoded"
	case rq.Text != "":
		t, terr := env.String(rq.Text, act)
		if terr != nil {
			return Request{}, render.Fail("request.text", terr)
		}
		req.Body = []byte(t)
		req.ContentType = "text/plain; charset=utf-8"
	}
	return req, nil
}

// addQuery sets a parameter: a list adds one value per element, a null adds
// none.
func addQuery(q url.Values, k string, v any) {
	switch t := v.(type) {
	case nil:
	case []any:
		for _, it := range t {
			q.Add(k, render.Text(it))
		}
	default:
		q.Add(k, render.Text(v))
	}
}

// Execute sends the request once. A failure of the step is a Result with
// OutcomeFailed; the error return is for faults of the engine.
func (x *Executor) Execute(ctx context.Context, inv core.Invocation) (core.Result, error) {
	req, rerr := render1(inv)
	if rerr != nil {
		return fail(core.Result{}, rerr, core.EffectNone), nil
	}
	var sc scrubber
	var authz authHeader
	if req.Auth != nil {
		val, err := x.Secrets.Get(ctx, inv.Tenant, inv.RunAs, req.Auth.Secret)
		if err != nil {
			return fail(core.Result{}, core.Wrap(core.CodeSecretNotFound, err), core.EffectNone), nil
		}
		var aerr *core.Error
		if authz, sc, aerr = buildAuth(req.Auth, val); aerr != nil {
			return fail(core.Result{}, aerr, core.EffectNone), nil
		}
	}

	timeout := render.Timeout(inv)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(rctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return fail(core.Result{}, core.Errorf(core.CodeInvalidNode, "%s: %v", inv.Node.Node, sc.text(err.Error())), core.EffectNone), nil
	}
	hreq.Header.Set("User-Agent", "flo")
	if req.ContentType != "" {
		hreq.Header.Set("Content-Type", req.ContentType)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	if req.IdempotencyKey != "" {
		hreq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
	if authz.name != "" {
		hreq.Header.Set(authz.name, authz.value)
	}

	out := core.Result{Calls: 1}
	resp, err := x.HTTP.Do(hreq)
	if err != nil {
		return sendFailure(ctx, out, inv, err, timeout, sc), nil
	}
	defer resp.Body.Close() //nolint:errcheck // response body
	body, truncated := readBody(resp.Body)
	body = sc.bytes(body)
	out.Truncated = truncated

	headers := responseHeaders(resp.Header, sc)
	out.Raw = map[string]any{
		"status": int64(resp.StatusCode), "headers": headers, "body": string(body), "truncated": truncated,
	}

	if !successful(inv.Node, resp.StatusCode) {
		return x.statusFailure(out, inv, resp, body), nil
	}
	parsed, perr := parseBody(resp.Header.Get("Content-Type"), body)
	if perr != nil {
		return fail(out, perr, core.EffectDone), nil
	}
	value := fromPath(inv.Node.Outputs.From, map[string]any{
		"status": int64(resp.StatusCode), "headers": headers, "body": parsed,
	})
	env, _ := render.Shared()
	value, failed, rerr := env.Reshape(inv.Node.Outputs, value, render.Scope{In: inv.In, Config: inv.Config, Run: inv.Run})
	if rerr != nil {
		return fail(out, rerr, core.EffectDone), nil
	}
	out.Output = value
	if failed {
		e := core.Errorf(core.CodeNodeReportedFailure, "%s reported a failure (outputs.fail_when)", inv.Node.Node)
		return fail(out, e, core.EffectDone), nil
	}
	out.Outcome, out.Effect = core.OutcomeOK, core.EffectDone
	return out, nil
}

func (x *Executor) now() time.Time {
	if x.Clock == nil {
		return time.Now()
	}
	return x.Clock.Now()
}

func successful(s *node.Spec, status int) bool {
	if s.Response != nil && len(s.Response.Success) > 0 {
		for _, c := range s.Response.Success {
			if c == status {
				return true
			}
		}
		return false
	}
	return status >= 200 && status <= 299
}

// statusFailure classifies an unlisted status (node.md §5.2): a 429 or a 5xx
// is transient (the response's Retry-After is carried in the detail, for the
// executor's backoff), anything else permanent. A 429 and a 503 are known
// not to have executed.
func (x *Executor) statusFailure(out core.Result, inv core.Invocation, resp *http.Response, body []byte) core.Result {
	snip := strings.TrimSpace(string(body))
	if len(snip) > bodySnippet {
		snip = snip[:bodySnippet] + "..."
	}
	msg := fmt.Sprintf("%s: HTTP %d", inv.Node.Node, resp.StatusCode)
	if snip != "" {
		msg += ": " + snip
	}
	var e *core.Error
	eff := core.EffectDone
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e = core.New(core.CodeProviderRateLimited, msg)
		eff = core.EffectNone
	case resp.StatusCode == http.StatusServiceUnavailable:
		e = core.New(core.CodeRunFailed, msg).WithTransience(core.TransienceTransient)
		eff = core.EffectNone
	case resp.StatusCode >= 500:
		e = core.New(core.CodeRunFailed, msg).WithTransience(core.TransienceTransient)
		eff = core.EffectUnknown
	default:
		e = core.New(core.CodeRunFailed, msg)
	}
	e = e.WithDetail("status", resp.StatusCode)
	if d, ok := retryAfter(resp.Header.Get("Retry-After"), x.now()); ok && e.Retryable() {
		e = e.WithDetail("retry_after_ms", d.Milliseconds())
	}
	return fail(out, e, eff)
}

// retryAfter reads a Retry-After header: delay-seconds or an HTTP date.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func sendFailure(ctx context.Context, out core.Result, inv core.Invocation, err error, timeout time.Duration, sc scrubber) core.Result {
	switch {
	case ctx.Err() != nil:
		return fail(out, core.Errorf(core.CodeRunCancelled, "%s: cancelled while the request was in flight", inv.Node.Node), core.EffectUnknown)
	case errors.Is(err, context.DeadlineExceeded):
		return fail(out, core.Errorf(core.CodeTimeout, "%s: no response in %s", inv.Node.Node, timeout), core.EffectUnknown)
	}
	msg := sc.text(err.Error())
	e := core.Errorf(core.CodeRunFailed, "%s: request failed: %s", inv.Node.Node, msg).WithTransience(core.TransienceTransient)
	if refused(err) {
		return fail(out, e, core.EffectNone)
	}
	return fail(out, e, core.EffectUnknown)
}

// refused reports a connection that was never made.
func refused(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

func readBody(r io.Reader) ([]byte, bool) {
	b, _ := io.ReadAll(io.LimitReader(r, maxBody+1)) //nolint:errcheck // a broken body is the body so far
	if len(b) > maxBody {
		return b[:maxBody], true
	}
	return b, false
}

// parseBody reads the body as JSON when the response says it is, else as a
// string (node.md §5.3).
func parseBody(contentType string, body []byte) (any, *core.Error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "application/json" || strings.HasSuffix(mt, "+json") {
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, nil
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, core.Errorf(core.CodeOutputInvalid, "the response is declared JSON but is not: %v", err)
		}
		return v, nil
	}
	return string(body), nil
}

// fromPath reads a dotted path out of {status, headers, body}; the default
// is body. A path that leads nowhere is null, and the schema decides.
func fromPath(path string, root map[string]any) any {
	if path == "" {
		path = "body"
	}
	var cur any = root
	for _, seg := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case map[string]any:
			cur = t[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil
			}
			cur = t[i]
		default:
			return nil
		}
	}
	return cur
}

// responseHeaders returns the headers as lower-case name to first value,
// without cookies.
func responseHeaders(h http.Header, sc scrubber) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if lk == "set-cookie" || lk == "authorization" || len(v) == 0 {
			continue
		}
		out[lk] = sc.text(v[0])
	}
	return out
}

func fail(r core.Result, e *core.Error, eff core.Effect) core.Result {
	r.Outcome, r.Err, r.Effect = core.OutcomeFailed, e, eff
	return r
}

// authHeader is the one header that carries the secret.
type authHeader struct{ name, value string }

func buildAuth(a *node.Auth, secret []byte) (authHeader, scrubber, *core.Error) {
	val := string(secret)
	sc := scrubber{values: []string{val}}
	switch {
	case a.Scheme == "bearer":
		return authHeader{"Authorization", "Bearer " + val}, sc, nil
	case a.Scheme == "basic":
		enc := base64.StdEncoding.EncodeToString(secret)
		sc.values = append(sc.values, enc)
		return authHeader{"Authorization", "Basic " + enc}, sc, nil
	case strings.HasPrefix(a.Scheme, "header:") && len(a.Scheme) > len("header:"):
		return authHeader{strings.TrimPrefix(a.Scheme, "header:"), val}, sc, nil
	}
	return authHeader{}, scrubber{}, core.Errorf(core.CodeInvalidNode, "auth scheme %q is not bearer, basic or header:<Name>", a.Scheme)
}

// scrubber removes secret values from text that may echo them (a server
// that reflects its request, a transport error).
type scrubber struct{ values []string }

func (s scrubber) text(t string) string {
	for _, v := range s.values {
		if v != "" {
			t = strings.ReplaceAll(t, v, redacted)
		}
	}
	return t
}

func (s scrubber) bytes(b []byte) []byte {
	for _, v := range s.values {
		if v != "" {
			b = bytes.ReplaceAll(b, []byte(v), []byte(redacted))
		}
	}
	return b
}
