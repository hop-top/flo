package cmd

import (
	"testing"

	kitconformance "hop.top/kit/go/conformance"
)

// TestCLIConforms asserts the command tree satisfies the kit
// Layer-A contract. AssertCLI forces EnforceValidate=true.
func TestCLIConforms(t *testing.T) {
	kitconformance.AssertCLI(t, NewRoot())
}
