package coretest

import "hop.top/flo/internal/core"

// Every fake is checked against its port at compile time, so a port that
// changes breaks the fake here rather than a test elsewhere.
var (
	_ core.Clock           = (*Clock)(nil)
	_ core.IDs             = (*IDs)(nil)
	_ core.Tenants         = (*Tenants)(nil)
	_ core.RunStore        = (*RunStore)(nil)
	_ core.StateStore      = (*StateStore)(nil)
	_ core.FlowStore       = (*FlowStore)(nil)
	_ core.ApprovalStore   = (*ApprovalStore)(nil)
	_ core.GrantStore      = (*GrantStore)(nil)
	_ core.ReceiptStore    = (*ReceiptStore)(nil)
	_ core.OutputStore     = (*OutputStore)(nil)
	_ core.TriggerStore    = (*TriggerStore)(nil)
	_ core.Groups          = (*Groups)(nil)
	_ core.Jobs            = (*Jobs)(nil)
	_ core.JobWorker       = (*Worker)(nil)
	_ core.Resumer         = (*Resumer)(nil)
	_ core.RunLog          = (*RunLog)(nil)
	_ core.Starter         = (*Starter)(nil)
	_ core.TriggerSync     = (*TrigSync)(nil)
	_ core.Dispatcher      = (*Dispatcher)(nil)
	_ core.Runner          = (*Runner)(nil)
	_ core.NodeRegistry    = (*Nodes)(nil)
	_ core.BuiltinRegistry = (*Builtins)(nil)
	_ core.BuiltinNode     = FuncNode{}
	_ core.CommandRunner   = (*Commands)(nil)
	_ core.Checker         = (*Checker)(nil)
	_ core.Secrets         = (*Secrets)(nil)
	_ core.Grants          = (*Grants)(nil)
	_ core.Admission       = (*Admission)(nil)
	_ core.ApprovalService = (*ApprovalService)(nil)
	_ core.ActivationGate  = (*Gate)(nil)
	_ core.Tasks           = (*Tasks)(nil)
	_ core.Trust           = (*Trust)(nil)
)
