package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"hop.top/kit/go/ai/ext"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/spec/node"
	specrun "hop.top/flo/spec/run"
)

// Plugin serves the calendar/events@1 trigger node: Poll reads the source
// and reports the upcoming event instances. It never says when to fire (the
// engine owns timing, node.md §7).
type Plugin struct {
	Clock core.Clock
	// Secrets resolves the `credentials` secret name; nil means none.
	Secrets core.Secrets
	// Client is the HTTP client of ics and caldav sources; nil takes a default.
	Client *http.Client
	// Open builds the source of a config; nil is NewSource. Tests swap it.
	Open func(SourceConfig, *http.Client) (Source, error)
}

var _ plugin.TriggerPlugin = (*Plugin)(nil)

// Meta implements ext.Extension.
func (p *Plugin) Meta() ext.Metadata {
	return ext.Metadata{Name: Namespace, Version: "0.1.0", Description: "Upcoming calendar events from ICS feeds and CalDAV"}
}

// Capabilities implements ext.Extension.
func (p *Plugin) Capabilities() ext.Capability { return ext.CapRegistry }

// Init implements ext.Extension.
func (p *Plugin) Init(context.Context) error { return nil }

// Close implements ext.Extension.
func (p *Plugin) Close() error { return nil }

// Specs implements plugin.TriggerPlugin: the embedded node specs.
func (p *Plugin) Specs() []*node.Spec {
	lp, err := Pack()
	if err != nil {
		panic("calendar: embedded pack is invalid: " + err.Error()) // a build-time defect, caught by tests
	}
	return lp.Nodes
}

// Poll implements plugin.TriggerPlugin. Events are every instance whose start
// lies in [From, To) (a zero To is seven days after From, a zero From is now).
// Cursor identifies the answer: it changes when an event is added, removed,
// moved or edited.
func (p *Plugin) Poll(ctx context.Context, r plugin.TriggerRequest) (plugin.TriggerBatch, error) {
	if r.Node != "" && r.Node != EventsNode {
		return plugin.TriggerBatch{}, core.Errorf(core.CodeNodeNotFound, "calendar pack does not serve %s", r.Node)
	}
	cfg, err := p.sourceConfig(ctx, r)
	if err != nil {
		return plugin.TriggerBatch{}, err
	}
	open := p.Open
	if open == nil {
		open = NewSource
	}
	src, err := open(cfg, p.Client)
	if err != nil {
		return plugin.TriggerBatch{}, err
	}
	w := Window{From: r.From, To: r.To}
	if w.From.IsZero() {
		w.From = p.Clock.Now()
	}
	if w.To.IsZero() {
		w.To = w.From.Add(specrun.DefaultHorizon)
	}
	evs, err := src.Events(ctx, w)
	if err != nil {
		return plugin.TriggerBatch{}, err
	}
	sortEvents(evs)
	out := plugin.TriggerBatch{Events: make([]plugin.Event, len(evs))}
	h := sha256.New()
	for i, ev := range evs {
		out.Events[i] = plugin.Event{ID: ev.ID(), Payload: ev.Payload()}
		b, _ := json.Marshal(out.Events[i])
		h.Write(b)
	}
	out.Cursor = hex.EncodeToString(h.Sum(nil)[:8])
	return out, nil
}

func (p *Plugin) sourceConfig(ctx context.Context, r plugin.TriggerRequest) (SourceConfig, error) {
	c := SourceConfig{
		Kind: str(r.Config, "source"), URL: str(r.Config, "url"), TZ: str(r.Config, "tz"),
		Calendar: str(r.Options, "calendar"),
	}
	if c.URL == "" && c.Kind != KindCRM {
		return c, core.Errorf(core.CodeInvalidNode, "calendar: config.calendar.url is not set")
	}
	if name := str(r.Config, "credentials"); name != "" {
		if p.Secrets == nil {
			return c, core.Errorf(core.CodeSecretNotFound, "calendar: credentials %q: no secret store", name)
		}
		v, err := p.Secrets.Get(ctx, r.Tenant, "", name)
		if err != nil {
			return c, err //nolint:wrapcheck // *core.Error
		}
		c.Auth = strings.TrimSpace(string(v))
	}
	return c, nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}
