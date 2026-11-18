package calendar

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// CalDAVSource reads one calendar collection with a calendar-query REPORT
// (RFC 4791 §7.8) filtered to the window; the server returns the series that
// have an instance in it and the engine expands them. Google (CalDAV), iCloud,
// Fastmail and Nextcloud answer this report.
type CalDAVSource struct {
	url    string
	auth   string
	client *http.Client
	tz     *time.Location
}

// NewCalDAV returns a CalDAV source for the collection at base, or at the
// path calendar relative to it.
func NewCalDAV(base, calendar, auth string, client *http.Client, tz *time.Location) (*CalDAVSource, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, core.Errorf(core.CodeInvalidNode, "caldav url %q: want an http(s) URL", base)
	}
	if calendar != "" {
		ref, err := url.Parse(calendar)
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidNode, "caldav calendar %q: %v", calendar, err)
		}
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/"
		}
		u = u.ResolveReference(ref)
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/" // a collection URL ends in a slash
		}
	}
	return &CalDAVSource{url: u.String(), auth: auth, client: client, tz: tz}, nil
}

const caldavTime = "20060102T150405Z"

var reportBody = `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
    <C:time-range start="%s" end="%s"/>
  </C:comp-filter></C:comp-filter></C:filter>
</C:calendar-query>`

type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Status   string `xml:"status"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				Data string `xml:"calendar-data"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// Events implements Source.
func (s *CalDAVSource) Events(ctx context.Context, w Window) ([]Event, error) {
	body := strings.Replace(strings.Replace(reportBody, "%s", w.From.UTC().Format(caldavTime), 1), "%s", w.To.UTC().Format(caldavTime), 1)
	req, err := http.NewRequestWithContext(ctx, "REPORT", s.url, strings.NewReader(body))
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidNode, "caldav url: %v", err)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	setAuth(req, s.auth)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, core.Errorf(core.CodeRunFailed, "caldav report: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, core.Errorf(core.CodeRunFailed, "caldav report: %v", err)
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, core.Errorf(core.CodeRunFailed, "caldav report: %s", resp.Status)
	}
	var ms multistatus
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&ms); err != nil {
		return nil, core.Errorf(core.CodeRunFailed, "caldav report: %v", err)
	}
	var out []Event
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, " 200 ") || strings.TrimSpace(ps.Prop.Data) == "" {
				continue
			}
			evs, _ := ParseICS([]byte(ps.Prop.Data), w, s.tz) // a bad resource skips its events
			out = append(out, evs...)
		}
	}
	sortEvents(out)
	return out, nil
}
