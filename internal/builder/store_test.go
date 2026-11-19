package builder_test

import (
	"testing"
	"time"

	"hop.top/flo/internal/builder"
	"hop.top/flo/internal/core"
)

func TestStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := builder.NewStore(func(core.TenantID) string { return root })
	d := builder.Draft{
		ID: "drf_01", Flow: "demo", Instruction: "x", Source: "flow: demo\n", Hash: "h2",
		Base: builder.Ref{Version: "0.1.0", Hash: "h1"}, Head: "h1", CreatedAt: time.Unix(5, 0).UTC(),
		DryRun: &builder.DryRun{Ran: true, Diffs: []string{"d"}},
	}
	if err := s.Save(core.DefaultTenant, d); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(core.DefaultTenant, "drf_01")
	if err != nil || got.Source != d.Source || got.Base.Hash != "h1" || got.DryRun == nil || got.DryRun.Diffs[0] != "d" {
		t.Fatalf("got %+v err %v", got, err)
	}
	d2 := d
	d2.ID, d2.Flow = "drf_02", "other"
	if err := s.Save(core.DefaultTenant, d2); err != nil {
		t.Fatal(err)
	}
	all, _ := s.List(core.DefaultTenant, "")
	one, _ := s.List(core.DefaultTenant, "demo")
	if len(all) != 2 || len(one) != 1 || all[0].ID != "drf_02" {
		t.Fatalf("list = %v / %v (want newest first)", all, one)
	}
	if err := s.Delete(core.DefaultTenant, "drf_01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(core.DefaultTenant, "drf_01"); !core.IsCode(err, core.CodeDraftNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(core.DefaultTenant, "drf_01"); !core.IsCode(err, core.CodeDraftNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := s.Load(core.DefaultTenant, "../x"); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("path id: %v", err)
	}
}
