// Package coretest holds in-memory fakes of every port of package core.
//
// The fakes are the test doubles each piece of the engine is built and tested
// against while the real implementations are written elsewhere. They are
// concurrency-safe (the executor runs steps in goroutines) and deterministic:
// time moves only when a test advances the Clock, ids are sequential, and
// listings have a defined order. They are faithful where it matters to a
// caller (idempotency, compare-and-set, terminal-state rules, deny by default)
// and simple elsewhere; they are not a specification of the real stores.
//
// Every fake is checked against its port by a compile-time assertion
// (assert.go). New builds a Fixture: all fakes wired together, and
// Fixture.Services gives them as a *core.Services.
package coretest
