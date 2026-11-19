package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"hop.top/kit/go/runtime/sideeffect"
	"hop.top/kit/go/runtime/sideeffect/real"
	"hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/env"

	"hop.top/flo/internal/access/admission"
	"hop.top/flo/internal/access/grants"
	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/brief"
	"hop.top/flo/internal/check/eva"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/run"
	"hop.top/flo/internal/flowtest"
	"hop.top/flo/internal/jobs"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/command"
	"hop.top/flo/internal/nodes/dispatch"
	httpnode "hop.top/flo/internal/nodes/http"
	"hop.top/flo/internal/nodes/ratelimit"
	"hop.top/flo/internal/packs/calendar"
	"hop.top/flo/internal/placement"
	"hop.top/flo/internal/runlog"
	"hop.top/flo/internal/runrec"
	"hop.top/flo/internal/store/sqlite"
	"hop.top/flo/internal/tasks/tlccli"
	"hop.top/flo/internal/triggers"
	"hop.top/flo/spec/node"
)

// Engine is the assembled engine: every port wired to its implementation.
// The CLI uses Services for its reads and writes; Serve runs the worker.
type Engine struct {
	Config   Config
	Services *core.Services

	Store     *sqlite.Store
	Queue     *jobs.Queue
	Worker    *jobs.Worker
	RunLog    *runlog.Log
	Nodes     *Nodes
	Executor  *run.Executor
	Scheduler *triggers.Scheduler
	Calendar  *calendar.Syncer
	Approvals *approvals.Service
	Pruner    *brief.Pruner
	Grants    *grants.Service
	Flows     *Flows
	// TestResults keeps the latest test of each flow version, which the
	// activation gate reads.
	TestResults *flowtest.Store
	// Recordings keeps the recordings of runs started with --record.
	Recordings *runrec.Store

	dispDeps dispatch.Deps // what a recording dispatcher is rebuilt from
	liveExec sideeffect.Exec
	liveHTTP sideeffect.HTTP
	// secretLog remembers the secrets nodes resolved, for a recording to scrub.
	secretLog *secretLog
}

// Open assembles the engine over the data root. It creates the root, the
// tenant's database and the jobs database when they are missing, and seeds
// the grants of the owner and of the engine's profile (idempotent).
func Open(ctx context.Context, cfg Config) (*Engine, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Root, 0o750); err != nil {
		return nil, core.Errorf(core.CodeInternal, "data root %s: %w", cfg.Root, err)
	}
	e := &Engine{Config: cfg}
	if err := e.open(ctx); err != nil {
		_ = e.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return e, nil
}

func (e *Engine) open(ctx context.Context) error {
	cfg := e.Config
	e.Store = sqlite.Open(cfg.Root, sqlite.Options{Clock: cfg.Clock, NoSync: cfg.NoSync})
	reg := e.Store.Registry()
	if err := reg.Ensure(ctx, cfg.Tenant); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	q, err := jobs.Open(reg.JobsPath(), jobs.Options{Clock: cfg.Clock, ClaimTTL: cfg.ClaimTTL, NoSync: cfg.NoSync})
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	e.Queue = q
	e.RunLog = runlog.New(runlog.Options{RunsDir: reg.RunsDir})
	e.Worker = jobs.NewWorker(q, jobs.WorkerOptions{
		Pollers: cfg.Pollers, Interval: cfg.Interval, StaleInterval: cfg.StaleInterval,
		ShutdownGrace: cfg.ShutdownGrace, Logger: cfg.Logger,
	})
	if err := e.wire(); err != nil {
		return err
	}
	return e.seed(ctx)
}

func (e *Engine) wire() error {
	cfg, st := e.Config, e.Store
	ids := core.SystemIDs{}
	var exec sideeffect.Exec = real.Exec{}
	if cfg.Exec != nil {
		exec = cfg.Exec
	}
	var httpc sideeffect.HTTP = real.NewHTTP(httpnode.NewClient())
	if cfg.HTTP != nil {
		httpc = cfg.HTTP
	}
	var secrets secret.Store = env.New(SecretPrefix)
	if cfg.Secrets != nil {
		secrets = cfg.Secrets
	}
	commands := command.NewRunner(exec)
	// Every secret a node resolves goes through secretLog, the model keys of
	// the generate node too, so a recording can scrub them.
	e.secretLog = &secretLog{Secrets: httpnode.StoreSecrets{Store: secrets}}

	e.Grants = grants.New(st.Grants, cfg.Clock)
	adm := admission.New(e.Grants, st.Receipts, ids, cfg.Clock)

	packs, err := LoadPacks(cfg.PacksDir)
	if err != nil {
		return err
	}
	builtins := builtin.NewRegistry()
	if err := builtin.RegisterStd(builtins, builtin.Deps{Outputs: st.Outputs, IDs: ids, Clock: cfg.Clock}); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	classifier, err := builtin.ClassifierFor(cfg.Classifier)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if err := builtin.RegisterClassify(builtins, classifier); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	generator, err := builtin.GeneratorFor(cfg.Generator, builtin.LLMDeps{
		HTTP: httpc, Secrets: e.secretLog, LocalHosts: cfg.LocalHosts,
	})
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	localHosts, err := node.NewLocalHosts(cfg.LocalHosts)
	if err != nil {
		return core.Wrap(core.CodeUsage, err)
	}
	if err := builtin.RegisterGenerate(builtins, generator, packs.Prompts(), localHosts); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	e.Nodes = newNodes(packs, builtins)
	if err := e.Nodes.Plugins.RegisterTrigger(&calendar.Plugin{Clock: cfg.Clock, Secrets: httpnode.StoreSecrets{Store: secrets}}); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}

	trustDirs := append([]string{filepath.Join(cfg.Root, "trusts")}, cfg.TrustDirs...)
	trust, err := approvals.NewCommandTrust(commands, packs.Locator(trustDirs...))
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	tasks := tlccli.New(commands, cfg.Tasks)
	resumer := jobs.NewResumer(e.Queue)
	e.Approvals = approvals.New(approvals.Deps{
		Store: st.Approvals, Tasks: tasks, Resumer: resumer, Trust: trust, Runs: st.Runs, Flows: st.Flows,
		Jobs: e.Queue, RunLog: e.RunLog, Clock: cfg.Clock, IDs: ids, PollEvery: cfg.ApprovalPoll,
	})
	approval, err := approvals.NewNode(approvals.NodeDeps{Approvals: e.Approvals, Store: st.Approvals, Trust: trust, Clock: cfg.Clock})
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if err := builtins.Register(approval); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}

	e.dispDeps = dispatch.Deps{
		Builtins: builtins, Commands: commands, HTTP: httpc, Secrets: e.secretLog,
		Plugins: e.Nodes.Plugins, Clock: cfg.Clock, Limits: ratelimit.New(cfg.Clock),
	}
	e.liveExec, e.liveHTTP = exec, httpc
	dispatcher := humanDispatcher{dispatch.New(e.dispDeps)}
	starter := core.NewStarter(core.StarterDeps{
		Clock: cfg.Clock, IDs: ids, Flows: st.Flows, Runs: st.Runs, Groups: st.Groups, Jobs: e.Queue, RunLog: e.RunLog,
	})
	e.Scheduler = triggers.New(triggers.Deps{
		Clock: cfg.Clock, IDs: ids, Tenants: st.Tenants, Flows: st.Flows, Triggers: st.Triggers, Jobs: e.Queue,
		Starter: starter, RunLog: e.RunLog, StartedBy: cfg.Engine,
	})
	e.Calendar = calendar.NewSyncer(calendar.Deps{
		Clock: cfg.Clock, Tenants: st.Tenants, Triggers: st.Triggers, Jobs: e.Queue, Plugins: e.Nodes.Plugins,
		Scheduler: e.Scheduler, Config: calendar.ConfigOf(cfg.ExprConfig),
	})

	e.TestResults = flowtest.NewStore(e.Store.Registry().Dir)
	e.Recordings = runrec.NewStore(e.Store.Registry().RunsDir)
	gate := &testedGate{results: e.TestResults}
	e.Services = &core.Services{
		Clock: cfg.Clock, IDs: ids,
		Tenants: st.Tenants, Runs: st.Runs, State: st.State, Flows: st.Flows, Approvals: st.Approvals,
		GrantRows: st.Grants, Receipts: st.Receipts, Outputs: st.Outputs, Triggers: st.Triggers,
		Jobs: e.Queue, Worker: e.Worker, Resumer: resumer, Groups: st.Groups, RunLog: e.RunLog,
		Starter: starter, TrigSync: e.Calendar.Hook(e.Scheduler),
		Dispatcher: dispatcher, Runner: recordingRunner{e: e, inner: placement.New(dispatcher)}, Nodes: e.Nodes.Registry, Builtins: builtins,
		Commands: commands, Checker: recordingChecker{e: e, inner: eva.New(httpc)}, Secrets: e.secretLog,
		Grants: e.Grants, ApprovalSvc: e.Approvals, Admission: adm, Gate: gate, Tasks: tasks, Trust: trust,
	}
	e.Executor = run.New(e.Services, run.Config{MaxParallel: cfg.MaxParallel, Config: cfg.ExprConfig})
	e.Worker.Handle(core.JobRunAdvance, e.advance)
	e.Scheduler.Register(e.Worker)
	e.Calendar.Register(e.Worker)
	e.Approvals.Register(e.Worker)
	e.Pruner = &brief.Pruner{Outputs: st.Outputs, Jobs: e.Queue, Clock: cfg.Clock, Also: e.pruneRecordings}
	e.Pruner.Register(e.Worker)
	e.Flows = NewFlows(e.Services, e.Nodes)
	e.Flows.localHosts = localHosts
	gate.flows = e.Flows
	return nil
}

// seed gives the owner the wildcard and the engine's profile the capabilities
// the installed packs declare.
func (e *Engine) seed(ctx context.Context) error {
	cfg := e.Config
	if cfg.Owner != "" {
		if _, err := e.Grants.SeedOwner(ctx, cfg.Tenant, cfg.Owner); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
	}
	if _, err := e.Grants.SyncEngine(ctx, cfg.Tenant, cfg.Engine, e.Nodes.Capabilities()); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	return nil
}

// Close releases the databases and the run log.
func (e *Engine) Close(ctx context.Context) error {
	var errs []error
	if e.RunLog != nil {
		errs = append(errs, e.RunLog.Close(ctx))
	}
	if e.Queue != nil {
		errs = append(errs, e.Queue.Close())
	}
	if e.Store != nil {
		errs = append(errs, e.Store.Close())
	}
	return errors.Join(errs...)
}

// humanDispatcher runs the core human step through approvals.Human, which
// validates on_timeout and names an untitled step; every other kind, and a
// human step under dry run, goes to the dispatcher.
type humanDispatcher struct{ core.Dispatcher }

func (h humanDispatcher) Dispatch(ctx context.Context, inv core.Invocation) (core.Result, error) {
	if inv.Kind == core.StepKindHuman && !inv.DryRun {
		return approvals.Human{}.Execute(ctx, inv) //nolint:wrapcheck // the node's own contract
	}
	return h.Dispatcher.Dispatch(ctx, inv) //nolint:wrapcheck // the dispatcher's own contract
}
