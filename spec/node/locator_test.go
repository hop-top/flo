package node

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func nodeDoc(ref, desc string) string {
	return "node: " + ref + "\nkind: command\ndescription: " + desc + "\nargv: [x]\ninputs: {}\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: none\nprivacy: local\n"
}

func builtinDoc(ref, desc string) string {
	return "node: " + ref + "\nkind: builtin\ndescription: " + desc + "\ninputs: {}\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: none\nprivacy: local\n"
}

func dirEntries(t *testing.T, src Source, docs map[string]string) []Entry {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, doc := range docs {
		fsys["d/"+name] = &fstest.MapFile{Data: []byte(doc)}
	}
	out, err := LoadDir(fsys, "d", src)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func descOf(t *testing.T, l *Locator, ref string) string {
	t.Helper()
	e, err := l.Locate(ref)
	if err != nil {
		t.Fatalf("Locate(%s): %v", ref, err)
	}
	return e.Spec.Description
}

func TestLocatorPrecedence(t *testing.T) {
	bundle := dirEntries(t, Source{Layer: LayerBundle}, map[string]string{
		"a.yaml": nodeDoc("acme/a@1", "bundle"),
	})
	pack := dirEntries(t, Source{Layer: LayerPack, Pack: "acme", PackVersion: "1.2.0"}, map[string]string{
		"a1.yaml": nodeDoc("acme/a@1", "pack"),
		"a2.yaml": nodeDoc("acme/a@2", "pack v2"),
		"b.yaml":  nodeDoc("acme/b@1", "pack b"),
	})
	builtin := dirEntries(t, Source{Layer: LayerBuiltin}, map[string]string{
		"b.yaml":  builtinDoc("flo/b@1", "builtin"),
		"c.yaml":  builtinDoc("flo/c@3", "builtin c3"),
		"c2.yaml": builtinDoc("flo/c@10", "builtin c10"),
	})
	l, err := NewLocator(Layers{Bundle: bundle, Packs: pack, Builtin: builtin})
	if err != nil {
		t.Fatal(err)
	}
	if got := descOf(t, l, "acme/a@1"); got != "bundle" {
		t.Errorf("a@1 served by %q; the bundle shadows the pack", got)
	}
	if got := descOf(t, l, "acme/a@2"); got != "pack v2" {
		t.Errorf("a@2 = %q; a pin the bundle lacks falls through to the pack", got)
	}
	// Unpinned: the first layer with the name, its highest N. The bundle
	// has a@1 only, so it wins over the pack's a@2.
	if got := descOf(t, l, "acme/a"); got != "bundle" {
		t.Errorf("unpinned a = %q, want the bundle's", got)
	}
	if got := descOf(t, l, "flo/c"); got != "builtin c10" {
		t.Errorf("unpinned c = %q; majors compare numerically (10 > 3)", got)
	}
	e, _ := l.Locate("acme/b@1")
	if e.Source.Layer != LayerPack || e.Source.Pack != "acme" || e.Source.PackVersion != "1.2.0" {
		t.Errorf("source = %+v", e.Source)
	}
	if _, err := l.Locate("acme/a@9"); !IsNotFound(err) {
		t.Errorf("a@9: %v", err)
	}
	if _, err := l.Locate("nope"); err == nil {
		t.Error("a reference without a namespace must be refused")
	}
	if n := len(l.List()); n != 6 {
		t.Errorf("List = %d effective types, want 6", n)
	}
}

func TestLocatorDuplicates(t *testing.T) {
	a := dirEntries(t, Source{Layer: LayerPack, Pack: "p1", PackVersion: "1.0.0"}, map[string]string{"x.yaml": nodeDoc("acme/x@1", "one")})
	b := dirEntries(t, Source{Layer: LayerPack, Pack: "p2", PackVersion: "2.0.0"}, map[string]string{"x.yaml": nodeDoc("acme/x@1", "two")})
	_, err := NewLocator(Layers{Packs: append(a, b...)})
	var es Errors
	if !errors.As(err, &es) || es[0].Code != CodeNodeConflict {
		t.Fatalf("two packs, one type: %v", err)
	}
	if !strings.Contains(err.Error(), "p1@1.0.0") || !strings.Contains(err.Error(), "p2@2.0.0") {
		t.Errorf("the error names both packs: %v", err)
	}
	// The same type in two layers is shadowing, not a conflict.
	c := dirEntries(t, Source{Layer: LayerBundle}, map[string]string{"x.yaml": nodeDoc("acme/x@1", "bundle")})
	if _, err := NewLocator(Layers{Bundle: c, Packs: a}); err != nil {
		t.Errorf("shadowing refused: %v", err)
	}
	// Two files of one bundle declaring one type.
	d := dirEntries(t, Source{Layer: LayerBundle}, map[string]string{"x.yaml": nodeDoc("acme/x@1", "1"), "y.yaml": nodeDoc("acme/x@1", "2")})
	if _, err := NewLocator(Layers{Bundle: d}); err == nil {
		t.Error("duplicate inside the bundle layer accepted")
	}
}

func TestLoadBundleAndTrust(t *testing.T) {
	fsys := fstest.MapFS{
		"nodes/a.yaml":    {Data: []byte(nodeDoc("acme/a@1", "x"))},
		"nodes/notes.txt": {Data: []byte("ignored")},
		"trusts/t.yaml":   {Data: []byte("trust: t\nkind: command\nread: { argv: [a] }\n")},
	}
	nodes, trusts, err := LoadBundle(fsys)
	if err != nil || len(nodes) != 1 || len(trusts) != 1 {
		t.Fatalf("%v %v %v", nodes, trusts, err)
	}
	l, err := NewLocator(Layers{Bundle: nodes, BundleTrusts: trusts})
	if err != nil {
		t.Fatal(err)
	}
	if te, err := l.Trust("t"); err != nil || te.Source.Layer != LayerBundle {
		t.Errorf("Trust: %v %v", te, err)
	}
	if _, err := l.Trust("zz"); !IsNotFound(err) {
		t.Errorf("unknown provider: %v", err)
	}
	if _, _, err := LoadBundle(fstest.MapFS{}); err != nil {
		t.Errorf("a bundle without nodes/ is empty, not broken: %v", err)
	}
	bad := fstest.MapFS{"nodes/a.yaml": {Data: []byte("node: nope\n")}}
	if _, _, err := LoadBundle(bad); err == nil {
		t.Error("an invalid node file must fail the load")
	}
}
