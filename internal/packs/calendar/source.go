package calendar

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// Source is where events come from. Events returns every event instance
// whose start lies in the window, recurring events expanded: the complete
// set, not a delta, so the FireMap can drop what disappeared.
type Source interface {
	Events(ctx context.Context, w Window) ([]Event, error)
}

// Source kinds.
const (
	KindICS    = "ics"
	KindCalDAV = "caldav"
	KindCRM    = "crm"
)

// SourceConfig says how to reach a source: `config.calendar.*` merged with
// the trigger's options.
type SourceConfig struct {
	Kind string
	// URL is a file path, a file: or http(s) URL (ics), or the CalDAV
	// collection URL.
	URL string
	// Calendar is a collection path relative to URL (caldav).
	Calendar string
	// Auth is the resolved credential: "user:password" or "bearer:<token>".
	Auth string
	// TZ is the zone of floating times.
	TZ string
}

// NewSource builds the source a config names; client may be nil.
func NewSource(c SourceConfig, client *http.Client) (Source, error) {
	loc := time.UTC
	if c.TZ != "" {
		l, err := time.LoadLocation(c.TZ)
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidNode, "calendar tz %q: %v", c.TZ, err)
		}
		loc = l
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	kind := c.Kind
	if kind == "" {
		kind = inferKind(c.URL)
	}
	switch kind {
	case KindICS:
		return &ICSSource{Location: c.URL, Auth: c.Auth, Client: client, TZ: loc}, nil
	case KindCalDAV:
		return NewCalDAV(c.URL, c.Calendar, c.Auth, client, loc)
	case KindCRM:
		return CRMSource{}, nil
	}
	return nil, core.Errorf(core.CodeInvalidNode, "calendar source %q: want ics, caldav or crm", kind)
}

func inferKind(loc string) string {
	u, err := url.Parse(loc)
	switch {
	case loc == "":
		return ""
	case err != nil || u.Scheme == "" || u.Scheme == "file":
		return KindICS
	case strings.HasSuffix(strings.ToLower(u.Path), ".ics"):
		return KindICS
	}
	return KindCalDAV
}

// ICSSource reads one iCalendar document from a file or a URL.
type ICSSource struct {
	// Location is a path, a file: URL or an http(s) URL.
	Location string
	Auth     string
	Client   *http.Client
	TZ       *time.Location
}

// Events implements Source. A malformed event is skipped; a document that
// does not parse at all is an error.
func (s *ICSSource) Events(ctx context.Context, w Window) ([]Event, error) {
	data, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	evs, errs := ParseICS(data, w, s.TZ)
	if len(evs) == 0 && len(errs) > 0 && !strings.Contains(string(data), "BEGIN:VEVENT") {
		return nil, core.Errorf(core.CodeRunFailed, "calendar %s: %v", s.Location, errs[0])
	}
	return evs, nil
}

func (s *ICSSource) read(ctx context.Context) ([]byte, error) {
	u, err := url.Parse(s.Location)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Location, http.NoBody)
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidNode, "calendar url: %v", err)
		}
		setAuth(req, s.Auth)
		resp, err := s.Client.Do(req)
		if err != nil {
			return nil, core.Errorf(core.CodeRunFailed, "calendar fetch: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			return nil, core.Errorf(core.CodeRunFailed, "calendar fetch: %s", resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return nil, core.Errorf(core.CodeRunFailed, "calendar fetch: %v", err)
		}
		return data, nil
	}
	path := s.Location
	if err == nil && u.Scheme == "file" {
		path = u.Path
	}
	data, err := os.ReadFile(path) //nolint:gosec // the operator's configured calendar file
	if err != nil {
		return nil, core.Errorf(core.CodeRunFailed, "calendar file: %v", err)
	}
	return data, nil
}

// maxBody bounds what one calendar response may hold.
const maxBody = 32 << 20

// setAuth adds the credential: "bearer:<token>" or "user:password".
func setAuth(req *http.Request, auth string) {
	switch {
	case auth == "":
	case strings.HasPrefix(auth, "bearer:"):
		req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(auth, "bearer:"))
	default:
		user, pass, _ := strings.Cut(auth, ":")
		req.SetBasicAuth(user, pass)
	}
}

// CRMSource is the seat of crm's watchable calendar store. crm does not sync
// VEVENTs yet (its calendar-vevent-sync track is open), so it reports the
// feature as unavailable; when the store ships, this is the one place that
// reads it.
type CRMSource struct{}

// Events implements Source.
func (CRMSource) Events(context.Context, Window) ([]Event, error) {
	return nil, core.Errorf(core.CodeEngineFeature, "calendar source crm: the crm VEVENT store is not available yet; use ics or caldav")
}

var _ = fmt.Sprintf
