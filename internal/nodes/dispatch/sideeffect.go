package dispatch

import (
	"hop.top/kit/go/console/cli"

	"hop.top/flo/spec/node"
)

// KitSideEffect maps a node's side-effect class (node.md §8) onto kit's tool
// side-effect annotation (kit cli SideEffect, the ladder `--dry-run` and the
// policy read), as the table of node.md §19 open question 1 asks:
//
//	flo none          -> read              pure compute: no mutation
//	flo read          -> read
//	flo write-local   -> write-local
//	flo write-shared  -> write-shared
//	flo irreversible  -> destructive-shared
//
// An unknown class maps to the most dangerous tier.
func KitSideEffect(s node.SideEffect) cli.SideEffect {
	switch s {
	case node.SideEffectNone, node.SideEffectRead:
		return cli.SideEffectRead
	case node.SideEffectWriteLocal:
		return cli.SideEffectWriteLocal
	case node.SideEffectWriteShared:
		return cli.SideEffectWriteShared
	}
	return cli.SideEffectDestructiveShared
}

// ExecutesInDryRun reports whether a node runs under dry run (node.md §8):
// the read and local tiers do, the shared-write and destructive tiers return
// their plan instead. It is kit's policy applied to the mapped tier.
func ExecutesInDryRun(s node.SideEffect) bool {
	switch KitSideEffect(s) {
	case cli.SideEffectWriteShared, cli.SideEffectDestructiveShared:
		return false
	}
	return true
}
