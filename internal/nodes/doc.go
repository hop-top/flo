// Package nodes is the home of node dispatch: the declared command and HTTP
// executors, the plugin and built-in registries, and the dispatcher that
// routes an invocation to them (see the sub-packages). The plugin interfaces
// are named here so a pack imports one path.
package nodes

import "hop.top/flo/internal/nodes/plugin"

// NodePlugin serves step nodes in process (node.md §7).
type NodePlugin = plugin.NodePlugin

// TriggerPlugin serves trigger nodes (role trigger) in process.
type TriggerPlugin = plugin.TriggerPlugin
