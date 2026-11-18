package spec

import (
	"os/exec"
	"strings"
	"testing"
)

// The root package must stay parse-only: importers that never evaluate an
// expression must not link the CEL library (it lives in spec/cel).
func TestRootPackageDoesNotImportCEL(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "cel.dev/") || strings.Contains(pkg, "cel-go") {
			t.Errorf("hop.top/flo/spec depends on %s", pkg)
		}
	}
}

// The spec module is the part tlc and other tools import: it must never
// link kit or any package of the flo engine.
func TestModuleHasNoKitOrEngineDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "hop.top/kit") || strings.HasPrefix(pkg, "hop.top/flo/internal") ||
			pkg == "hop.top/flo/cmd" || pkg == "hop.top/flo" {
			t.Errorf("the spec module depends on %s", pkg)
		}
	}
}
