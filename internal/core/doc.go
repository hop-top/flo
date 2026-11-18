// Package core is the contract of the flo engine: the domain types, the
// ports the engine is written against, and the typed errors and events every
// piece shares. It holds no I/O of its own.
//
// Every piece of the engine (persistence, the run executor, node dispatch,
// access and approvals, the CLI) is written against these ports and tested
// with the in-memory fakes of package coretest, so the pieces can be built
// apart and joined at assembly. The rules that keep that true:
//
//   - core imports no other package of this module (see TestCoreImportsNoSiblingPackage);
//     it may import the spec module and kit.
//   - every port takes the tenant explicitly. A tenant is the unit of storage
//     isolation (one database per tenant); phase 1 runs a single tenant, but
//     no port may assume it.
//   - a port returns *Error for every failure a caller may branch on; the code
//     is stable and maps to an exit code (see errors.go).
//   - ports are safe for concurrent use: the executor runs steps in goroutines.
//
// # Layout
//
//	tenant.go, ids.go   TenantID, typed ids, IDs port
//	model*.go           Run, StepRun, Approval, Grant, ... (dek Job/Proposal/Plan shaped)
//	errors.go           Error, codes, exit-code table
//	events.go           CloudEvent topics and payloads of the run log
//	ports*.go           the ports
//	services.go         Services: every port, for wiring
//	starter.go          Starter: create-and-admit a run (shared by triggers and the CLI)
package core
