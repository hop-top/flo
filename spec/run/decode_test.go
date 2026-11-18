package run

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/flo/spec"
)

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

func exampleFlow(t *testing.T, name string) *spec.Flow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "spec", "examples.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range yamlFence.FindAllStringSubmatch(string(data), -1) {
		if strings.HasPrefix(m[1], "flow: "+name+"\n") {
			f, err := spec.ParseFlow(strings.NewReader(m[1]), name+".yaml")
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			return f
		}
	}
	t.Fatalf("examples.md has no flow %q", name)
	return nil
}

func TestDecodeDependencyWatch(t *testing.T) {
	s, err := Decode(exampleFlow(t, "dependency-watch"))
	if err != nil {
		t.Fatal(err)
	}
	if s.RunAs != "automation" || s.Privacy != PrivacyCloudAllowed || s.Tasks != TasksNone {
		t.Errorf("header = %+v", s)
	}
	if s.Budget.USD != 0.5 || s.Budget.Tokens != 0 {
		t.Errorf("budget = %+v", s.Budget)
	}
	if len(s.Triggers) != 2 || s.Triggers[0].Kind != TriggerSchedule || s.Triggers[1].Kind != TriggerManual {
		t.Fatalf("triggers = %+v", s.Triggers)
	}
	if tr := s.Triggers[0]; tr.RRule != "FREQ=DAILY;BYHOUR=7" || tr.TZ != "America/Toronto" {
		t.Errorf("schedule = %+v", tr)
	}
	for _, key := range []string{"seen", "seen_adv", "cursor"} {
		if d, ok := s.State[key]; !ok || !d.HasDefault {
			t.Errorf("state %q = %+v", key, d)
		}
	}
	if d := s.State["cursor"]; d.Default != nil {
		t.Errorf("cursor default = %v, want null", d.Default)
	}
	if s.Concurrency.Max != 1 || s.Concurrency.OverlapFor(TriggerSchedule) != OverlapSkip || s.Concurrency.OverlapFor(TriggerManual) != OverlapQueue {
		t.Errorf("concurrency = %+v", s.Concurrency)
	}
}

func TestDecodeMeetingBrief(t *testing.T) {
	s, err := Decode(exampleFlow(t, "meeting-brief"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Privacy != PrivacyLocalOnly || s.Tasks != TasksNone || s.Budget.USD != 0.10 {
		t.Errorf("settings = %+v", s)
	}
	if len(s.Triggers) != 1 {
		t.Fatalf("triggers = %+v", s.Triggers)
	}
	tr := s.Triggers[0]
	if tr.Kind != TriggerCalendar || tr.Source != "calendar/events@1" || tr.Offset != "-30m" || !strings.Contains(tr.When, "attendees") {
		t.Errorf("calendar = %+v", tr)
	}
	if !tr.Kind.Timed() {
		t.Error("calendar is timed")
	}
}

func TestDecodeInboxTriage(t *testing.T) {
	s, err := Decode(exampleFlow(t, "inbox-triage"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Concurrency.OnOverlap != OverlapSkip || s.Concurrency.Max != 1 || s.Tasks != TasksPerRun {
		t.Errorf("settings = %+v", s)
	}
	// a schedule, and a manual trigger so a person can start it
	if len(s.Triggers) != 2 || s.Triggers[0].TZ != DefaultTZ || s.Triggers[0].RRule != "FREQ=MINUTELY;INTERVAL=15" || s.Triggers[1].Kind != TriggerManual {
		t.Errorf("triggers = %+v", s.Triggers)
	}
}

func TestDecodeDefaults(t *testing.T) {
	s, err := Decode(&spec.Flow{Name: "f", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Privacy != PrivacyLocalOnly || s.Tasks != TasksPerRun || s.Concurrency.Max != 1 || !s.Budget.IsZero() {
		t.Errorf("defaults = %+v", s)
	}
	if len(s.Triggers) != 1 || s.Triggers[0].Kind != TriggerManual {
		t.Errorf("absent on: is a manual trigger; got %+v", s.Triggers)
	}
}

func TestConcurrencyOverlapExplicitWins(t *testing.T) {
	c := Concurrency{OnOverlap: OverlapCancelPrevious}
	if c.OverlapFor(TriggerSchedule) != OverlapCancelPrevious {
		t.Error("an explicit policy wins over the trigger default")
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := []struct {
		name  string
		flow  spec.Flow
		field string
	}{
		{"privacy", spec.Flow{Privacy: "public"}, "privacy"},
		{"tasks", spec.Flow{Tasks: "always"}, "tasks"},
		{"budget key", spec.Flow{Budget: map[string]any{"eur": 1}}, "budget"},
		{"budget duration", spec.Flow{Budget: map[string]any{"duration": "soon"}}, "budget.duration"},
		{"budget negative", spec.Flow{Budget: map[string]any{"usd": -1}}, "budget"},
		{"concurrency overlap", spec.Flow{Concurrency: map[string]any{"on_overlap": "drop"}}, "concurrency.on_overlap"},
		{"concurrency max", spec.Flow{Concurrency: map[string]any{"max": -2}}, "concurrency.max"},
		{"concurrency key", spec.Flow{Concurrency: map[string]any{"limit": 2}}, "concurrency"},
		{"state shape", spec.Flow{State: map[string]any{"seen": []any{}}}, "state.seen"},
		{"state key", spec.Flow{State: map[string]any{"seen": map[string]any{"initial": 1}}}, "state.seen"},
		{"trigger two keys", spec.Flow{On: []map[string]any{{"manual": map[string]any{}, "schedule": map[string]any{}}}}, "on[0]"},
		{"trigger kind", spec.Flow{On: []map[string]any{{"cron": map[string]any{}}}}, "on[0]"},
		{"schedule key", spec.Flow{On: []map[string]any{{"schedule": map[string]any{"cron": "x"}}}}, "on[0].schedule"},
		{"manual options", spec.Flow{On: []map[string]any{{"manual": map[string]any{"a": 1}}}}, "on[0].manual"},
		{"offset type", spec.Flow{On: []map[string]any{{"calendar": map[string]any{"offset": 5}}}}, "on[0].calendar.offset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(&tt.flow)
			var re *Error
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *run.Error", err)
			}
			if re.Code != CodeInvalidFlow || re.Field != tt.field {
				t.Errorf("error = %+v, want field %q", re, tt.field)
			}
		})
	}
}

func TestCalendarKeepsSourceOptions(t *testing.T) {
	f := spec.Flow{On: []map[string]any{{"calendar": map[string]any{
		"source": "calendar/events@1", "horizon": "3d", "calendar": "work",
	}}}}
	s, err := Decode(&f)
	if err != nil {
		t.Fatal(err)
	}
	tr := s.Triggers[0]
	if tr.Horizon != "3d" || tr.Options["calendar"] != "work" || len(tr.Options) != 1 {
		t.Errorf("trigger = %+v", tr)
	}
}

func TestDecodeStep(t *testing.T) {
	st := spec.Step{
		ID: "s", RunAs: "other", Privacy: "local-only", Timeout: "2m",
		Budget:      map[string]any{"usd": 0.1, "calls": 3, "duration": "1h"},
		Retry:       &spec.RetrySpec{MaxAttempts: 3, Backoff: "5s"},
		Concurrency: 8, OnItemError: "skip",
	}
	got, err := DecodeStep(&st)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunAs != "other" || got.Privacy != PrivacyLocalOnly || got.Timeout != 2*time.Minute {
		t.Errorf("step = %+v", got)
	}
	if got.Budget.USD != 0.1 || got.Budget.Calls != 3 || got.Budget.Duration != time.Hour {
		t.Errorf("budget = %+v", got.Budget)
	}
	if got.Retry == nil || got.Retry.MaxAttempts != 3 || got.Retry.Backoff != 5*time.Second {
		t.Errorf("retry = %+v", got.Retry)
	}
	if got.ForEachConcurrency != 8 || got.OnItemError != OnItemErrorSkip {
		t.Errorf("for_each = %d %s", got.ForEachConcurrency, got.OnItemError)
	}

	def, err := DecodeStep(&spec.Step{ID: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if def.ForEachConcurrency != 4 || def.OnItemError != OnItemErrorFail || def.Retry != nil || def.Privacy != "" {
		t.Errorf("defaults = %+v", def)
	}

	zeroAttempts, err := DecodeStep(&spec.Step{ID: "z", Retry: &spec.RetrySpec{Backoff: "1s"}})
	if err != nil || zeroAttempts.Retry.MaxAttempts != 1 {
		t.Errorf("retry with no max_attempts = %+v, %v; want 1 attempt", zeroAttempts.Retry, err)
	}
}

func TestDecodeStepErrors(t *testing.T) {
	for name, st := range map[string]spec.Step{
		"privacy":     {ID: "s", Privacy: "x"},
		"timeout":     {ID: "s", Timeout: "later"},
		"backoff":     {ID: "s", Retry: &spec.RetrySpec{Backoff: "later"}},
		"attempts":    {ID: "s", Retry: &spec.RetrySpec{MaxAttempts: -1}},
		"concurrency": {ID: "s", Concurrency: -1},
		"on_item":     {ID: "s", OnItemError: "ignore"},
		"budget":      {ID: "s", Budget: map[string]any{"usd": "lots"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeStep(&st); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestPrivacyNarrows(t *testing.T) {
	if !PrivacyLocalOnly.Narrows(PrivacyCloudAllowed) || !PrivacyLocalOnly.Narrows(PrivacyLocalOnly) || !PrivacyCloudAllowed.Narrows(PrivacyCloudAllowed) {
		t.Error("local-only narrows anything; equal values narrow")
	}
	if PrivacyCloudAllowed.Narrows(PrivacyLocalOnly) {
		t.Error("cloud-allowed widens local-only")
	}
}
