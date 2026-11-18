package coretest

import "hop.top/flo/internal/core"

// Fixture is every fake, wired together on one Clock and one id sequence:
// what a test of any piece starts from. Fields are exported so a test
// scripts the fakes it cares about and ignores the rest.
type Fixture struct {
	Clock *Clock
	IDs   *IDs
	DB    *DB

	Jobs     *Jobs
	Worker   *Worker
	Resumer  *Resumer
	RunLog   *RunLog
	Starter  *Starter
	TrigSync *TrigSync

	Dispatcher *Dispatcher
	Runner     *Runner
	Nodes      *Nodes
	Builtins   *Builtins
	Commands   *Commands
	Checker    *Checker
	Secrets    *Secrets

	Grants    *Grants
	Admission *Admission
	Approvals *ApprovalService
	Gate      *Gate
	Tasks     *Tasks
	Trust     *Trust
}

// New returns a Fixture on a clock at Epoch.
func New() *Fixture {
	f := &Fixture{Clock: NewClock(Epoch), IDs: &IDs{}}
	f.DB = NewDB(f.Clock)
	f.Jobs = NewJobs(f.Clock, f.IDs)
	f.Worker = NewWorker(f.Jobs)
	f.Resumer = NewResumer(f.Jobs)
	f.RunLog = NewRunLog()
	f.TrigSync = &TrigSync{}
	f.Starter = NewStarter(core.StarterDeps{
		Clock: f.Clock, IDs: f.IDs, Flows: f.DB.Flows, Runs: f.DB.Runs, Groups: f.DB.Groups, Jobs: f.Jobs, RunLog: f.RunLog,
	})
	f.Dispatcher, f.Runner = &Dispatcher{}, &Runner{}
	f.Nodes, f.Builtins, f.Commands, f.Checker, f.Secrets = &Nodes{}, &Builtins{}, &Commands{}, &Checker{}, &Secrets{}
	f.Gate, f.Tasks, f.Trust = &Gate{}, &Tasks{Clock: f.Clock}, &Trust{}
	f.Grants = &Grants{Store: f.DB.Grants, Clock: f.Clock}
	f.Admission = &Admission{Receipts: f.DB.Receipts, IDs: f.IDs, Clock: f.Clock}
	f.Approvals = &ApprovalService{
		Store: f.DB.Approvals, Tasks: f.Tasks, Resumer: f.Resumer, Trust: f.Trust, RunLog: f.RunLog, Clock: f.Clock, IDs: f.IDs,
	}
	return f
}

// Services returns the fakes as a *core.Services.
func (f *Fixture) Services() *core.Services {
	return &core.Services{
		Clock: f.Clock, IDs: f.IDs,
		Tenants: f.DB.Tenants, Runs: f.DB.Runs, State: f.DB.State, Flows: f.DB.Flows, Approvals: f.DB.Approvals,
		GrantRows: f.DB.Grants, Receipts: f.DB.Receipts, Outputs: f.DB.Outputs, Triggers: f.DB.Triggers,
		Jobs: f.Jobs, Worker: f.Worker, Resumer: f.Resumer, Groups: f.DB.Groups, RunLog: f.RunLog,
		Starter: f.Starter, TrigSync: f.TrigSync,
		Dispatcher: f.Dispatcher, Runner: f.Runner, Nodes: f.Nodes, Builtins: f.Builtins, Commands: f.Commands,
		Checker: f.Checker, Secrets: f.Secrets,
		Grants: f.Grants, ApprovalSvc: f.Approvals, Admission: f.Admission, Gate: f.Gate, Tasks: f.Tasks, Trust: f.Trust,
	}
}
