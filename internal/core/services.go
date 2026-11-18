package core

// Services is every port the engine is wired from. Assembly fills it with
// real implementations; tests fill it with the fakes of package coretest
// (coretest.NewServices). A command or handler that needs several ports takes
// *Services and reads the fields it uses.
type Services struct {
	Clock Clock
	IDs   IDs

	Tenants   Tenants
	Runs      RunStore
	State     StateStore
	Flows     FlowStore
	Approvals ApprovalStore
	GrantRows GrantStore
	Receipts  ReceiptStore
	Outputs   OutputStore
	Triggers  TriggerStore

	Jobs     Jobs
	Worker   JobWorker
	Resumer  Resumer
	Groups   Groups
	RunLog   RunLog
	Starter  Starter
	TrigSync TriggerSync

	Dispatcher Dispatcher
	Runner     Runner
	Nodes      NodeRegistry
	Builtins   BuiltinRegistry
	Commands   CommandRunner
	Checker    Checker
	Secrets    Secrets

	Grants Grants
	// ApprovalSvc is the approval service (Open, Decide, GuardSatisfied...);
	// Approvals above is its store.
	ApprovalSvc ApprovalService
	Admission   Admission
	Gate        ActivationGate
	Tasks       Tasks
	Trust       Trust
}
