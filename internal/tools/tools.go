//go:build tools

// Package tools pins the dependencies the flo engine builds on, so that
// every later piece of work compiles against one go.mod. A blank import
// here keeps a module required and tidy before the first real import lands.
package tools

import (
	_ "github.com/santhosh-tekuri/jsonschema/v5"
	_ "hop.top/flo/spec"
	_ "hop.top/flo/spec/cel"
	_ "hop.top/flo/spec/node"
	_ "hop.top/flo/spec/run"
	_ "hop.top/kit/go/ai/ext"
	_ "hop.top/kit/go/core/id"
	_ "hop.top/kit/go/runtime/bus"
	_ "hop.top/kit/go/runtime/job"
	_ "hop.top/kit/go/runtime/job/durabletask"
	_ "hop.top/kit/go/runtime/sideeffect"
	_ "hop.top/kit/go/runtime/sideeffect/real"
	_ "hop.top/kit/go/runtime/sideeffect/testfake"
	_ "hop.top/kit/go/storage/secret"
	_ "hop.top/kit/go/storage/sqldb"
	_ "hop.top/vstar/rrule"
)
