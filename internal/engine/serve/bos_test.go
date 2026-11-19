package serve_test

import (
	"testing"

	"hop.top/flo/internal/bos"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// With a business scope the engine admits through bos and reads trust from
// bos; without one it stays on its local backends.
func TestOpenWiresBosAsGovernanceBackend(t *testing.T) {
	open := func(c *bos.Config) *serve.Engine {
		e, err := serve.Open(bg, serve.Config{Root: t.TempDir(), Owner: "jad", NoSync: true, Bos: c})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(bg) })
		return e
	}
	local := open(nil)
	if _, ok := local.Services.Admission.(*bos.Admission); ok {
		t.Error("local engine admits through bos")
	}
	if _, ok := local.Services.Trust.(*bos.Trust); ok {
		t.Error("local engine reads trust from bos")
	}
	withBos := open(&bos.Config{Scope: "business_scopes_1", Principals: map[string]string{"jad": "principals_1"}})
	if _, ok := withBos.Services.Admission.(*bos.Admission); !ok {
		t.Errorf("admission = %T", withBos.Services.Admission)
	}
	if _, ok := withBos.Services.Trust.(*bos.Trust); !ok {
		t.Errorf("trust = %T", withBos.Services.Trust)
	}
	if ok, _ := withBos.Grants.Covers(bg, core.DefaultTenant, "jad", "x.y"); !ok {
		t.Error("the owner holds the wildcard unless narrowed")
	}
	narrow := open(&bos.Config{Scope: "business_scopes_1", NarrowOwner: true})
	if ok, _ := narrow.Grants.Covers(bg, core.DefaultTenant, "jad", "x.y"); ok {
		t.Error("a narrowed owner was seeded the wildcard")
	}
}
