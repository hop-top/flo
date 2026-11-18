package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// StarterDeps are the ports a Starter is built from.
type StarterDeps struct {
	Clock  Clock
	IDs    IDs
	Flows  FlowStore
	Runs   RunStore
	Groups Groups
	Jobs   Jobs
	RunLog RunLog
}

type starter struct{ StarterDeps }

// NewStarter returns the Starter over the given ports.
func NewStarter(d StarterDeps) Starter { return &starter{d} }

// AdvanceJob returns the run.advance job of a run: one per run, keyed by its
// id so a second request while one is pending changes nothing (D2).
func AdvanceJob(tenant TenantID, run RunID, at *time.Time) JobSpec {
	return JobSpec{
		Type: JobRunAdvance, Tenant: tenant, Key: string(run),
		Payload: RunAdvancePayload{Tenant: tenant, Run: run}, At: at,
	}
}

type jobResumer struct{ jobs Jobs }

// NewJobResumer returns a Resumer that enqueues the run's advance job.
func NewJobResumer(jobs Jobs) Resumer { return jobResumer{jobs} }

// Resume implements Resumer.
func (r jobResumer) Resume(ctx context.Context, tenant TenantID, run RunID) error {
	if err := RequireTenant(tenant); err != nil {
		return err
	}
	_, err := r.jobs.Enqueue(ctx, AdvanceJob(tenant, run, nil))
	return err //nolint:wrapcheck // port error passes through
}

func (s *starter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	if err := RequireTenant(req.Tenant); err != nil {
		return StartResult{}, err
	}
	fv, err := s.version(ctx, req)
	if err != nil {
		return StartResult{}, err
	}
	if req.IdemKey != "" {
		if r, ok, err := s.Runs.LookupIdem(ctx, req.Tenant, fv.Name, req.IdemKey); err != nil {
			return StartResult{}, err //nolint:wrapcheck // port error passes through
		} else if ok {
			return StartResult{Run: r}, nil
		}
	}
	f, err := cel.ParseFlow(bytes.NewReader(fv.Source), fv.Name+".yaml")
	if err != nil {
		return StartResult{}, Wrap(CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return StartResult{}, Wrap(CodeInvalidFlow, err)
	}
	if req.Trigger.Kind == "" {
		req.Trigger.Kind = specrun.TriggerManual
	}
	now := s.Clock.Now()
	if req.Trigger.At.IsZero() {
		req.Trigger.At = now
	}
	group, err := s.group(f, set, req)
	if err != nil {
		return StartResult{}, err
	}
	id := NewRunID(s.IDs)
	dec, err := s.Groups.Admit(ctx, GroupRequest{
		Tenant: req.Tenant, Group: group, Max: set.Concurrency.Max,
		OnOverlap: set.Concurrency.OverlapFor(req.Trigger.Kind), Run: id,
	})
	if err != nil {
		return StartResult{}, err //nolint:wrapcheck // port error passes through
	}
	if dec.Action == GroupSkip {
		return StartResult{Skipped: true}, nil
	}
	runAs := set.RunAs
	if runAs == "" {
		runAs = req.StartedBy
	}
	run, created, err := s.Runs.CreateRun(ctx, NewRun{
		ID: id, Tenant: req.Tenant, Flow: fv.Ref(), Vars: req.Vars, Trigger: req.Trigger,
		IdemKey: req.IdemKey, Group: group, GroupMax: set.Concurrency.Max, Admitted: dec.Action == GroupRun,
		StartedBy: req.StartedBy, RunAs: runAs, Record: req.Record, At: now,
	})
	if err != nil {
		_ = s.Groups.Abandon(ctx, req.Tenant, group, id)
		return StartResult{}, err //nolint:wrapcheck // port error passes through
	}
	if !created {
		_ = s.Groups.Abandon(ctx, req.Tenant, group, id)
		return StartResult{Run: run}, nil
	}
	res := StartResult{Run: run, Created: true, Queued: dec.Action == GroupQueue, Cancelled: dec.Cancel}
	if err := s.afterCreate(ctx, run, dec); err != nil {
		return res, err
	}
	return res, nil
}

func (s *starter) afterCreate(ctx context.Context, run Run, dec GroupDecision) error {
	s.emit(ctx, run, TopicRunCreated)
	if dec.Action == GroupRun {
		if _, err := s.Jobs.Enqueue(ctx, AdvanceJob(run.Tenant, run.ID, nil)); err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
	}
	var errs []error
	for _, id := range dec.Cancel {
		if _, err := s.Runs.RequestCancel(ctx, run.Tenant, id); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.Jobs.Enqueue(ctx, AdvanceJob(run.Tenant, id, nil)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *starter) Release(ctx context.Context, run Run) error {
	if run.Group == "" {
		return nil
	}
	promoted, err := s.Groups.Release(ctx, run.Tenant, run.Group)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	var errs []error
	for _, id := range promoted {
		if _, err := s.Jobs.Enqueue(ctx, AdvanceJob(run.Tenant, id, nil)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *starter) version(ctx context.Context, req StartRequest) (FlowVersion, error) {
	if req.Flow == "" {
		return FlowVersion{}, Errorf(CodeUsage, "a flow name is required")
	}
	if req.Version == "" {
		return s.Flows.Active(ctx, req.Tenant, req.Flow) //nolint:wrapcheck // port error passes through
	}
	return s.Flows.GetVersion(ctx, req.Tenant, req.Flow, req.Version) //nolint:wrapcheck // port error passes through
}

// group renders the concurrency group: the flow name by default, else the
// template over the trigger and vars.
func (s *starter) group(f *spec.Flow, set *specrun.Settings, req StartRequest) (string, error) {
	if set.Concurrency.Group == "" {
		return req.Flow, nil
	}
	env, err := cel.EnvForFlow(f)
	if err != nil {
		return "", Wrap(CodeInternal, err)
	}
	tmpl, err := env.CompileTemplate(set.Concurrency.Group)
	if err != nil {
		return "", Wrap(CodeInvalidFlow, err)
	}
	trig := map[string]any{"kind": string(req.Trigger.Kind), "at": req.Trigger.At.UTC().Format(time.RFC3339)}
	for k, v := range req.Trigger.Payload {
		trig[k] = v
	}
	out, err := tmpl.Render(cel.BindVars(cel.Activation{"trigger": trig}, req.Vars))
	if err != nil {
		return "", Wrap(CodeEvalError, err)
	}
	g := fmt.Sprint(out)
	if g == "" {
		return req.Flow, nil
	}
	return g, nil
}

func (s *starter) emit(ctx context.Context, run Run, topic bus.Topic) {
	ev := NewEvent(s.Clock, s.IDs, topic, run.Tenant, run.ID, RunEvent{
		Tenant: run.Tenant, Run: run.ID, Flow: run.Flow.Name, Version: run.Flow.Version,
		Hash: run.Flow.Hash, Status: run.Status, Trigger: run.Trigger.Kind,
	})
	_ = s.RunLog.Emit(ctx, run.Tenant, run.ID, ev)
}
