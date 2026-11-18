package eva_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/check/eva"
	"hop.top/flo/internal/core"
)

func gateway(t *testing.T, status int, body string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/contract/invoke" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestPass200(t *testing.T) {
	srv, got := gateway(t, 200, `{"ok":true}`)
	res, err := eva.New(real.NewHTTP(nil)).Check(context.Background(), core.CheckRequest{
		Contract: "dep-judge-v1", URL: srv.URL + "/", Body: map[string]any{"items": []any{1}},
	})
	if err != nil || !res.Passed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if (*got)["contract"] != "dep-judge-v1" || (*got)["body"].(map[string]any)["items"] == nil {
		t.Fatalf("sent = %v", *got)
	}
}

func TestFail422CarriesMessage(t *testing.T) {
	for name, body := range map[string]string{
		"message":    `{"message":"label not in set"}`,
		"error":      `{"error":"label not in set"}`,
		"violations": `{"violations":["label not in set"]}`,
		"text":       `label not in set`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := gateway(t, 422, body)
			res, err := eva.New(real.NewHTTP(nil)).Check(context.Background(), core.CheckRequest{Contract: "c", URL: srv.URL})
			if err != nil || res.Passed || res.Message != "label not in set" {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}
}

func TestOtherStatusesAreTransientUnavailable(t *testing.T) {
	for _, status := range []int{400, 404, 500, 503} {
		srv, _ := gateway(t, status, "nope")
		_, err := eva.New(real.NewHTTP(nil)).Check(context.Background(), core.CheckRequest{Contract: "c", URL: srv.URL})
		if !core.IsCode(err, core.CodeCheckUnavailable) || !eva.ErrUnavailable(err) || core.ClassOf(err) != core.TransienceTransient {
			t.Fatalf("%d: err = %v", status, err)
		}
	}
}

func TestUnreachableGatewayIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := eva.New(real.NewHTTP(nil)).Check(context.Background(), core.CheckRequest{Contract: "c", URL: url})
	if !core.IsCode(err, core.CodeCheckUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := eva.New(real.NewHTTP(nil)).Check(context.Background(), core.CheckRequest{Contract: "c"}); !core.IsCode(err, core.CodeCheckUnavailable) {
		t.Fatalf("no URL: %v", err)
	}
}

func TestSlowGatewayTimesOutAsUnavailable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := &eva.Client{HTTP: real.NewHTTP(nil), Timeout: 100 * time.Millisecond}
	_, err := c.Check(context.Background(), core.CheckRequest{Contract: "c", URL: srv.URL})
	if !core.IsCode(err, core.CodeCheckUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelledCallerIsNotUnavailable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := eva.New(real.NewHTTP(nil)).Check(ctx, core.CheckRequest{Contract: "c", URL: srv.URL})
	if err == nil || core.IsCode(err, core.CodeCheckUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestMessageIsBounded(t *testing.T) {
	srv, _ := gateway(t, 422, strings.Repeat("x", 5000))
	res, _ := eva.New(real.NewHTTP(nil)).Check(context.Background(), core.CheckRequest{Contract: "c", URL: srv.URL})
	if len(res.Message) > 1100 {
		t.Fatalf("len = %d", len(res.Message))
	}
}
