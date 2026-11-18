package core

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTenant(t *testing.T) {
	for _, ok := range []string{"default", "a", "acme-1", "a_b"} {
		if _, err := ParseTenant(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "A", "a/b", "a b", "..", strings.Repeat("a", 64)} {
		if _, err := ParseTenant(bad); !IsCode(err, CodeUsage) {
			t.Errorf("%q accepted", bad)
		}
	}
	if TenantID("").OrDefault() != DefaultTenant || RequireTenant("") == nil {
		t.Error("OrDefault / RequireTenant")
	}
}

func TestIDs(t *testing.T) {
	id := NewRunID(SystemIDs{})
	if _, err := ParseRunID(string(id)); err != nil {
		t.Errorf("minted id does not parse: %v", err)
	}
	if !strings.HasPrefix(string(id), "run_") || !strings.HasPrefix(string(NewApprovalID(SystemIDs{})), "ap_") {
		t.Error("prefixes")
	}
	if _, err := ParseRunID(string(NewApprovalID(SystemIDs{}))); !IsCode(err, CodeUsage) {
		t.Error("an approval id is not a run id")
	}
	if _, err := ParseApprovalID("nope"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestJSONNumbers(t *testing.T) {
	v, err := DecodeJSONValue([]byte(`{"i":3,"f":3.5,"e":1e3,"big":99999999999999999999,"l":[1,2.0],"n":null,"s":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if _, ok := m["i"].(int64); !ok {
		t.Errorf("3 is %T, want int64", m["i"])
	}
	for _, k := range []string{"f", "e", "big"} {
		if _, ok := m[k].(float64); !ok {
			t.Errorf("%s is %T, want float64", k, m[k])
		}
	}
	l := m["l"].([]any)
	if _, ok := l[0].(int64); !ok {
		t.Errorf("l[0] %T", l[0])
	}
	if _, ok := l[1].(float64); !ok {
		t.Errorf("l[1] %T", l[1])
	}
	if _, err := DecodeJSONValue([]byte(`1 2`)); err == nil {
		t.Error("trailing data accepted")
	}
	n, _ := NormalizeJSON(map[string]any{"a": 7, "b": float32(1.5)})
	if n.(map[string]any)["a"] != int64(7) {
		t.Errorf("normalize: %#v", n)
	}
}

func TestStatusHelpers(t *testing.T) {
	for _, s := range []RunStatus{RunSucceeded, RunFailed, RunCancelled} {
		if !s.Terminal() || s.Active() {
			t.Errorf("%s", s)
		}
	}
	for _, s := range []RunStatus{RunQueued, RunRunning, RunAwaitingApproval} {
		if s.Terminal() || !s.Active() {
			t.Errorf("%s", s)
		}
	}
	if StepWaiting.Terminal() || !StepSkipped.Terminal() {
		t.Error("step terminality")
	}
	if !VerdictAuto.Approving() || !VerdictCorrect.Approving() || VerdictReject.Approving() {
		t.Error("verdicts")
	}
	if (Cost{USD: 1, Tokens: 2}).Add(Cost{USD: 2, Tokens: 3}) != (Cost{USD: 3, Tokens: 5}) {
		t.Error("cost add")
	}
}

// core is the contract: it may use the spec module and kit, never another
// package of this module, and never the tools flo orchestrates.
func TestCoreImportsNoSiblingPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.HasPrefix(p, "hop.top/flo/internal/") && p != "hop.top/flo/internal/core" {
			t.Errorf("core imports %s", p)
		}
		for _, bad := range []string{"hop.top/heo", "hop.top/ctxt", "hop.top/tlc"} {
			if strings.HasPrefix(p, bad) {
				t.Errorf("core depends on %s", p)
			}
		}
	}
}
