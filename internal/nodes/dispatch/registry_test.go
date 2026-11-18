package dispatch_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/dispatch"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/spec/node"
)

func decl(ns, name string, major int, desc string) string {
	return "node: " + ns + "/" + name + "@" + strconv.Itoa(major) + "\nkind: command\ndescription: " + desc +
		"\nargv: [x]\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n"
}

func bundleWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nodes"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, "nodes", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func fullRegistry(t *testing.T) (*dispatch.Registry, *dispatch.PackLayer) {
	t.Helper()
	bi := builtin.NewRegistry()
	fx := newRig(t).fx
	if err := builtin.RegisterStd(bi, builtin.Deps{Outputs: fx.DB.Outputs, IDs: fx.IDs, Clock: fx.Clock}); err != nil {
		t.Fatal(err)
	}
	packs := &dispatch.PackLayer{}
	return dispatch.NewRegistry(dispatch.BundleLayer{}, packs, dispatch.BuiltinLayer{Builtins: bi}), packs
}

func TestRegistryPrecedence(t *testing.T) {
	reg, packs := fullRegistry(t)
	ctx := context.Background()
	// the same node type in all three layers: bundle > pack > built-in
	packs.Add(core.NodeSource{Pack: "mine", PackVersion: "0.1.0"}, spec(t, decl("flo", "dedupe", 1, "from a pack")))
	bundle := bundleWith(t, map[string]string{"dedupe.yaml": decl("flo", "dedupe", 1, "from the bundle")})

	got, err := reg.Resolve(ctx, core.NodeScope{Bundle: bundle}, "flo/dedupe@1")
	if err != nil || got.Source.Layer != core.LayerBundle || got.Spec.Description != "from the bundle" {
		t.Fatalf("bundle wins: %+v err=%v", got.Source, err)
	}
	if filepath.Base(got.Source.Path) != "dedupe.yaml" {
		t.Fatalf("source path = %q", got.Source.Path)
	}
	got, err = reg.Resolve(ctx, core.NodeScope{}, "flo/dedupe@1")
	if err != nil || got.Source.Layer != core.LayerPack || got.Source.Pack != "mine" || got.Source.PackVersion != "0.1.0" {
		t.Fatalf("a pack beats the built-in: %+v err=%v", got.Source, err)
	}
	got, err = reg.Resolve(ctx, core.NodeScope{}, "flo/filter@1")
	if err != nil || got.Source.Layer != core.LayerBuiltin {
		t.Fatalf("built-in last: %+v err=%v", got.Source, err)
	}
}

func TestRegistryMajors(t *testing.T) {
	reg, packs := fullRegistry(t)
	ctx := context.Background()
	packs.Add(core.NodeSource{Pack: "p"}, spec(t, decl("p", "x", 1, "one")), spec(t, decl("p", "x", 2, "two")))
	if got, err := reg.Resolve(ctx, core.NodeScope{}, "p/x"); err != nil || got.Spec.Node != "p/x@2" {
		t.Fatalf("unpinned takes the highest N: %v err=%v", got.Spec, err)
	}
	if got, err := reg.Resolve(ctx, core.NodeScope{}, "p/x@1"); err != nil || got.Spec.Node != "p/x@1" {
		t.Fatalf("a pin holds: %v err=%v", got.Spec, err)
	}
	if _, err := reg.Resolve(ctx, core.NodeScope{}, "p/x@3"); !core.IsCode(err, core.CodeNodeNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := reg.Resolve(ctx, core.NodeScope{}, "nope/none"); !core.IsCode(err, core.CodeNodeNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := reg.Resolve(ctx, core.NodeScope{}, "not a ref"); !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistryConflict(t *testing.T) {
	reg, packs := fullRegistry(t)
	packs.Add(core.NodeSource{Pack: "a"}, spec(t, decl("shared", "n", 1, "a")))
	packs.Add(core.NodeSource{Pack: "b"}, spec(t, decl("shared", "n", 1, "b")))
	_, err := reg.Resolve(context.Background(), core.NodeScope{}, "shared/n@1")
	if !core.IsCode(err, core.CodeNodeConflict) {
		t.Fatalf("a spec declared by two packs: %v", err)
	}
	// two bundle files declaring one type conflict as well
	bundle := bundleWith(t, map[string]string{"a.yaml": decl("b", "n", 1, "x"), "b.yml": decl("b", "n", 1, "y")})
	if _, err := reg.Resolve(context.Background(), core.NodeScope{Bundle: bundle}, "b/n@1"); !core.IsCode(err, core.CodeNodeConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistryPluginsArePackSpecs(t *testing.T) {
	reg, packs := fullRegistry(t)
	packs.Plugins = plugin.NewRegistry()
	s := spec(t, pluginSpec)
	if err := packs.Plugins.RegisterNode(&testPlugin{specs: []*node.Spec{s}}); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Resolve(context.Background(), core.NodeScope{}, "calendar/summary@1")
	if err != nil || got.Source.Layer != core.LayerPack || got.Source.Pack != "calendar" {
		t.Fatalf("got=%+v err=%v", got.Source, err)
	}
}

func TestRegistryList(t *testing.T) {
	reg, packs := fullRegistry(t)
	packs.Add(core.NodeSource{Pack: "mine"}, spec(t, decl("flo", "dedupe", 1, "override")), spec(t, decl("a", "z", 1, "z")))
	infos, err := reg.List(context.Background(), core.NodeScope{Bundle: bundleWith(t, map[string]string{"x.json": `{"node":"b/y@1","kind":"command","argv":["x"],"inputs":{},"outputs":{"schema":{}},"capabilities":[],"side_effect":"read","privacy":"local"}`})})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	layers := map[string]core.NodeLayer{}
	for _, in := range infos {
		types = append(types, in.Spec.Node)
		layers[in.Spec.Node] = in.Source.Layer
	}
	want := []string{"a/z@1", "b/y@1", "flo/dedupe@1", "flo/filter@1", "flo/output@1"}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v, want %v", types, want)
		}
	}
	if layers["flo/dedupe@1"] != core.LayerPack || layers["b/y@1"] != core.LayerBundle {
		t.Fatalf("one per type, from the highest layer: %v", layers)
	}
}

func TestRegistryBadBundleFile(t *testing.T) {
	reg, _ := fullRegistry(t)
	bundle := bundleWith(t, map[string]string{"bad.yaml": "node: not-a-type\nkind: command\n"})
	_, err := reg.Resolve(context.Background(), core.NodeScope{Bundle: bundle}, "flo/filter@1")
	if !core.IsCode(err, core.CodeInvalidNode) {
		t.Fatalf("a malformed bundle node is invalid_node: %v", err)
	}
}

func newBuiltins(t *testing.T, ns ...core.BuiltinNode) core.BuiltinRegistry {
	t.Helper()
	r := builtin.NewRegistry()
	for _, n := range ns {
		if err := r.Register(n); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
