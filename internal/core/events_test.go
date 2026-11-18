package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"hop.top/kit/go/runtime/bus"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time                           { return c.t }
func (fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type countIDs struct{ n int }

func (c *countIDs) New(prefix string) string {
	c.n++
	return SystemIDs{}.New(prefix)
}

func TestTopicsAreValidAndUnique(t *testing.T) {
	seen := map[bus.Topic]bool{}
	for _, ti := range Topics() {
		if err := bus.ValidateTopic(ti.Topic); err != nil {
			t.Errorf("%s: %v", ti.Topic, err)
		}
		if seen[ti.Topic] {
			t.Errorf("duplicate topic %s", ti.Topic)
		}
		seen[ti.Topic] = true
	}
	for _, want := range []bus.Topic{TopicRunSucceeded, TopicStepStarted, TopicApprovalRequested,
		TopicAdmissionDecided, TopicBudgetExceeded, TopicTriggerSkipped} {
		if !seen[want] {
			t.Errorf("topic %s missing from Topics()", want)
		}
	}
}

func TestEveryTopicBuildsAValidCloudEvent(t *testing.T) {
	clock := fixedClock{time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	ids := &countIDs{}
	for _, ti := range Topics() {
		ev := NewEvent(clock, ids, ti.Topic, DefaultTenant, "run_x", ti.Payload)
		ev.Extensions[ExtSeq] = int64(1)
		ce, err := bus.ToCloudEvent(ev)
		if err != nil {
			t.Errorf("%s: ToCloudEvent: %v", ti.Topic, err)
			continue
		}
		if err := ce.Validate(); err != nil {
			t.Errorf("%s: invalid CloudEvent: %v", ti.Topic, err)
		}
		if ce.Source != "flo/default" || ce.Subject != "run_x" {
			t.Errorf("%s: source %q subject %q", ti.Topic, ce.Source, ce.Subject)
		}
		b, err := bus.MarshalCloudEvent(ev)
		if err != nil {
			t.Errorf("%s: marshal: %v", ti.Topic, err)
		} else if !json.Valid(b) {
			t.Errorf("%s: not json", ti.Topic)
		}
	}
}
