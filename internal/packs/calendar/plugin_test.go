package calendar_test

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/internal/packs/calendar"
)

var bg = context.Background()

func newPlugin(t *testing.T, clock core.Clock) *calendar.Plugin {
	t.Helper()
	return &calendar.Plugin{Clock: clock, Secrets: &coretest.Secrets{}}
}

const two = `
BEGIN:VEVENT
UID:a@x
DTSTART:20261119T100000Z
SUMMARY:A
ATTENDEE:mailto:bob@example.com
END:VEVENT
BEGIN:VEVENT
UID:b@x
DTSTART:20261120T100000Z
SUMMARY:B
END:VEVENT`

func TestPackLoadsAndPluginRegisters(t *testing.T) {
	lp, err := calendar.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if lp.Pack.Pack != "calendar" || len(lp.Nodes) != 1 || lp.Nodes[0].Node != calendar.EventsNode {
		t.Fatalf("pack = %+v", lp.Pack)
	}
	reg := plugin.NewRegistry()
	if err := reg.RegisterTrigger(newPlugin(t, coretest.NewClock(time.Time{}))); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Trigger(calendar.EventsNode); !ok {
		t.Fatal("trigger node not served")
	}
	if caps := lp.Nodes[0].Capabilities; len(caps) != 1 || caps[0] != "calendar.read" {
		t.Errorf("capabilities = %v", caps)
	}
}

func TestPollICSFileAndCursor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cal.ics")
	if err := os.WriteFile(file, ics(two), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := coretest.NewClock(utc("2026-11-18T12:00:00Z"))
	p := newPlugin(t, clock)
	req := plugin.TriggerRequest{Tenant: core.DefaultTenant, Node: calendar.EventsNode, Config: map[string]any{"url": file}}
	b, err := p.Poll(bg, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 2 || b.Events[0].ID != "a@x|" || b.Events[1].Payload["title"] != "B" || b.Cursor == "" {
		t.Fatalf("batch = %+v", b)
	}
	if again, _ := p.Poll(bg, req); again.Cursor != b.Cursor {
		t.Error("an unchanged calendar keeps its cursor")
	}
	// the event moves: the cursor changes
	moved := strings.Replace(two, "20261119T100000Z", "20261119T150000Z", 1)
	if err := os.WriteFile(file, ics(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, _ := p.Poll(bg, req); again.Cursor == b.Cursor {
		t.Error("a moved event changes the cursor")
	}
	// an explicit window bounds the starts
	req.From, req.To = utc("2026-11-20T00:00:00Z"), utc("2026-11-21T00:00:00Z")
	if only, _ := p.Poll(bg, req); len(only.Events) != 1 || only.Events[0].ID != "b@x|" {
		t.Errorf("windowed = %+v", only.Events)
	}
}

func TestPollICSOverHTTPWithBearer(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(ics(two))
	}))
	defer srv.Close()
	sec := &coretest.Secrets{}
	sec.Put("", "cal-token", []byte("bearer:tok123"))
	p := &calendar.Plugin{Clock: coretest.NewClock(utc("2026-11-18T12:00:00Z")), Secrets: sec}
	b, err := p.Poll(bg, plugin.TriggerRequest{Tenant: core.DefaultTenant, Config: map[string]any{"url": srv.URL + "/cal.ics", "credentials": "cal-token"}})
	if err != nil || len(b.Events) != 2 {
		t.Fatalf("%v %+v", err, b)
	}
	if auth.Load() != "Bearer tok123" {
		t.Errorf("authorization = %v", auth.Load())
	}
}

func TestPollCalDAVReport(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if r.Method != "REPORT" || !ok || u != "ann" || p != "s3cret" || r.URL.Path != "/cal/work/" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		var esc strings.Builder
		_ = xml.EscapeText(&esc, ics(two))
		var gone strings.Builder
		_ = xml.EscapeText(&gone, ics("BEGIN:VEVENT\nUID:nope\nDTSTART:20261119T100000Z\nEND:VEVENT"))
		_, _ = io.WriteString(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
<D:response><D:href>/cal/work/a.ics</D:href><D:propstat><D:prop><D:getetag>"1"</D:getetag><C:calendar-data>`+esc.String()+`</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/cal/work/gone.ics</D:href><D:propstat><D:prop><C:calendar-data>`+gone.String()+`</C:calendar-data></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>
</D:multistatus>`)
	}))
	defer srv.Close()
	sec := &coretest.Secrets{}
	sec.Put("", "dav", []byte("ann:s3cret"))
	p := &calendar.Plugin{Clock: coretest.NewClock(utc("2026-11-18T12:00:00Z")), Secrets: sec}
	b, err := p.Poll(bg, plugin.TriggerRequest{
		Tenant: core.DefaultTenant, Options: map[string]any{"calendar": "work"},
		Config: map[string]any{"source": "caldav", "url": srv.URL + "/cal", "credentials": "dav"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 2 || b.Events[0].Payload["uid"] != "a@x" {
		t.Fatalf("events = %+v", b.Events)
	}
	if !strings.Contains(body, `<C:time-range start="20261118T120000Z" end="20261125T120000Z"/>`) {
		t.Errorf("report window: %s", body)
	}
}

func TestPollCalDAVRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	p := newPlugin(t, coretest.NewClock(time.Time{}))
	_, err := p.Poll(bg, plugin.TriggerRequest{Config: map[string]any{"source": "caldav", "url": srv.URL}})
	if !core.IsCode(err, core.CodeRunFailed) {
		t.Fatalf("err = %v", err)
	}
}

func TestPollCRMStubIsUnavailable(t *testing.T) {
	p := newPlugin(t, coretest.NewClock(time.Time{}))
	_, err := p.Poll(bg, plugin.TriggerRequest{Config: map[string]any{"source": "crm"}})
	if !core.IsCode(err, core.CodeEngineFeature) {
		t.Fatalf("err = %v", err)
	}
}

func TestPollNeedsAURL(t *testing.T) {
	p := newPlugin(t, coretest.NewClock(time.Time{}))
	if _, err := p.Poll(bg, plugin.TriggerRequest{}); !core.IsCode(err, core.CodeInvalidNode) {
		t.Fatalf("err = %v", err)
	}
}
