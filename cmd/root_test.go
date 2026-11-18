package cmd

import (
	"testing"

	kitconformance "hop.top/kit/go/conformance"
)

// TestCLIConforms asserts the command tree satisfies the kit
// Layer-A contract. AssertCLI forces EnforceValidate=true.
func TestCLIConforms(t *testing.T) {
	// AssertCLI fails t itself; the returned error is informational.
	_ = kitconformance.AssertCLI(t, NewRoot())
}
