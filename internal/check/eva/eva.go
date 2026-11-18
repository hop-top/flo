// Package eva is the client of the contract gateway (flow.md §5.8): it
// sends a step's output to eva and reports the verdict. 200 passes, 422 is a
// failing check with the gateway's message, anything else, and an
// unreachable gateway, is a transient check_unavailable.
package eva

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"hop.top/kit/go/runtime/sideeffect"

	"hop.top/flo/internal/core"
)

// invokePath is the gateway's contract endpoint.
const invokePath = "/v1/contract/invoke"

// DefaultTimeout bounds one check when the client sets none.
const DefaultTimeout = 15 * time.Second

// maxResponse bounds what is read of a gateway response.
const maxResponse = 64 << 10

// maxMessage bounds the message of a failing check.
const maxMessage = 1024

// Client is the core.Checker over HTTP. HTTP is the kit side-effect seam, so
// a cassette or a recorder can stand in for the gateway.
type Client struct {
	HTTP sideeffect.HTTP
	// Timeout bounds one check; zero is DefaultTimeout.
	Timeout time.Duration
}

// New returns a Client over h.
func New(h sideeffect.HTTP) *Client { return &Client{HTTP: h} }

// Check implements core.Checker.
func (c *Client) Check(ctx context.Context, req core.CheckRequest) (core.CheckResult, error) {
	if strings.TrimSpace(req.URL) == "" {
		return core.CheckResult{}, core.Errorf(core.CodeCheckUnavailable, "check %s: no gateway URL (check.eva_url or config.eva.url)", req.Contract)
	}
	body, err := json.Marshal(map[string]any{"contract": req.Contract, "body": req.Body})
	if err != nil {
		return core.CheckResult{}, core.Errorf(core.CodeCheckUnavailable, "check %s: the output is not JSON: %v", req.Contract, err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(req.URL, "/")+invokePath, bytes.NewReader(body))
	if err != nil {
		return core.CheckResult{}, core.Errorf(core.CodeCheckUnavailable, "check %s: %v", req.Contract, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return core.CheckResult{}, fmt.Errorf("check %s: %w", req.Contract, ctx.Err())
		}
		return core.CheckResult{}, core.Errorf(core.CodeCheckUnavailable, "check %s: gateway unreachable: %v", req.Contract, err)
	}
	defer resp.Body.Close()                                       //nolint:errcheck // response body
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse)) //nolint:errcheck // a broken body is the body so far

	switch resp.StatusCode {
	case http.StatusOK:
		return core.CheckResult{Passed: true}, nil
	case http.StatusUnprocessableEntity:
		return core.CheckResult{Passed: false, Message: message(data)}, nil
	}
	return core.CheckResult{}, core.Errorf(core.CodeCheckUnavailable, "check %s: gateway answered HTTP %d: %s",
		req.Contract, resp.StatusCode, message(data)).WithDetail("status", resp.StatusCode)
}

// message reads the reason out of a gateway body: the usual keys of a JSON
// error, else the text.
func message(data []byte) string {
	text := strings.TrimSpace(string(data))
	var m map[string]any
	if json.Unmarshal(data, &m) == nil {
		for _, k := range []string{"message", "error", "detail", "violations", "errors"} {
			if v, ok := m[k]; ok && v != nil {
				text = flatten(v)
				break
			}
		}
	}
	if len(text) > maxMessage {
		text = text[:maxMessage] + "..."
	}
	return text
}

func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, it := range t {
			parts[i] = flatten(it)
		}
		return strings.Join(parts, "; ")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

var _ core.Checker = (*Client)(nil)

// ErrUnavailable reports whether err is a check_unavailable failure.
func ErrUnavailable(err error) bool {
	var ce *core.Error
	return errors.As(err, &ce) && ce.Code == core.CodeCheckUnavailable
}
