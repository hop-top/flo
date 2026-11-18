package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

// legacyAliases is a host vocabulary: recipe/gate/run.recipe.
var legacyAliases = &Aliases{
	Top:  map[string]string{"recipe": "flow"},
	Step: map[string]string{"gate": "check"},
	Run:  map[string]string{"recipe": "flow"},
}

const legacyDoc = `recipe: legacy
version: 0.1.0
steps:
  - id: build
    title: "Build for {{run.recipe}}@{{ run.version }}"
    kind: exec
    exec: {argv: [make]}
  - id: ship
    title: Ship
    kind: human
    human: {timeout: 1h, on_timeout: reject}
    depends_on: [build]
    gate: {contract: release-ok, eva_url: "http://eva"}
`

func TestParseFlowWith_AliasedYAML(t *testing.T) {
	f, err := ParseFlowWith(strings.NewReader(legacyDoc), "legacy.yaml", ParseOptions{Aliases: legacyAliases})
	if err != nil {
		t.Fatalf("ParseFlowWith: %v", err)
	}
	if f.Name != "legacy" || f.Steps[1].Check == nil || f.Steps[1].Check.Contract != "release-ok" {
		t.Errorf("parsed = %+v", f)
	}
	if want := RawHash([]byte(legacyDoc)); f.Hash != want {
		t.Errorf("Hash = %s; want hash of authored bytes %s", f.Hash, want)
	}
	if !strings.Contains(f.Steps[0].Title, "{{run.flow}}") {
		t.Errorf("title = %q; want run.recipe mapped to run.flow", f.Steps[0].Title)
	}
	got, err := RenderStep(f.Steps[0], Scope{Run: map[string]any{"flow": "legacy", "version": "0.1.0"}})
	if err != nil || got.Title != "Build for legacy@0.1.0" {
		t.Errorf("render = %q, %v", got.Title, err)
	}
}

func TestParseFlowWith_NoAliasesRejectsHostKeys(t *testing.T) {
	if _, err := ParseFlowWith(strings.NewReader(legacyDoc), "legacy.yaml", ParseOptions{}); err == nil {
		t.Fatal("want an error without aliases")
	}
}

func TestParseFlowWith_AliasedJSON(t *testing.T) {
	doc := `{"recipe":"legacy","version":"0.1.0","steps":[` +
		`{"id":"a","title":"A","kind":"human","human":{"timeout":"1h","on_timeout":"reject"},` +
		`"gate":{"contract":"c","eva_url":"u"}}]}`
	f, err := ParseFlowWith(strings.NewReader(doc), "legacy.json", ParseOptions{Aliases: legacyAliases})
	if err != nil || f.Name != "legacy" || f.Steps[0].Check == nil {
		t.Fatalf("json: %v %+v", err, f)
	}
	if f.Hash != RawHash([]byte(doc)) {
		t.Errorf("Hash not of authored bytes")
	}
}

func TestParseFlowWith_NestedAliasAndLinesPreserved(t *testing.T) {
	doc := "recipe: nest\nversion: 1\nsteps:\n  - id: loop\n    repeat: 2\n    steps:\n" +
		"      - id: inner\n        title: x\n        gate: {contract: c, eva_url: u}\n" +
		"      - id: bad\n        title: y\n        bogus: 1\n"
	_, err := ParseFlowWith(strings.NewReader(doc), "nest.yaml", ParseOptions{Aliases: legacyAliases})
	if err == nil || !strings.Contains(err.Error(), "line 12") || strings.Contains(err.Error(), "gate") {
		t.Fatalf("err = %v; want the unknown key at its original line 12", err)
	}
	ok := strings.Replace(doc, "      - id: bad\n        title: y\n        bogus: 1\n", "", 1)
	f, err := ParseFlowWith(strings.NewReader(ok), "nest.yaml", ParseOptions{Aliases: legacyAliases})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c := f.Steps[0].Steps[0].Check; c == nil || c.Contract != "c" {
		t.Errorf("nested alias not mapped: %+v", f.Steps[0].Steps[0])
	}
}

func TestAliasesRewrite_Shapes(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"quoted key":                  {"\"recipe\": x\n", "\"flow\": x\n"},
		"canonical wins":              {"recipe: a\nflow: b\n", "recipe: a\nflow: b\n"},
		"unparsable":                  {"recipe: [\n", "recipe: [\n"},
		"not a map":                   {"- recipe\n", "- recipe\n"},
		"run ref only in placeholder": {"title: run.recipe {{ run.recipe }}\n", "title: run.recipe {{ run.flow }}\n"},
		"run field prefix kept":       {"t: {{run.recipes}}\n", "t: {{run.recipes}}\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := string(legacyAliases.Rewrite([]byte(tc.in), "x.yaml")); got != tc.want {
				t.Errorf("Rewrite(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := legacyAliases.Rewrite([]byte("recipe: x"), "x.yaml"); string(got) != "flow: x" {
		t.Errorf("got %q", got)
	}
	if z := (Aliases{}); !z.IsZero() || string(z.Rewrite([]byte("recipe: x"), "x.yaml")) != "recipe: x" {
		t.Error("zero Aliases must be a no-op")
	}
}

func TestAliasesInvert_RewriteNodeRoundTrip(t *testing.T) {
	f, err := ParseFlowWith(strings.NewReader(legacyDoc), "legacy.yaml", ParseOptions{Aliases: legacyAliases})
	if err != nil {
		t.Fatal(err)
	}
	var node yaml.Node
	if err := node.Encode(f); err != nil {
		t.Fatal(err)
	}
	legacyAliases.Invert().RewriteNode(&node)
	out, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.HasPrefix(text, "recipe: legacy\n") || !strings.Contains(text, "gate:") ||
		strings.Contains(text, "flow:") || strings.Contains(text, "check:") {
		t.Errorf("not in host vocabulary:\n%s", text)
	}
	if _, err := ParseFlowWith(strings.NewReader(text), "again.yaml", ParseOptions{Aliases: legacyAliases}); err != nil {
		t.Errorf("re-parse: %v", err)
	}
}

func TestRawHash_Stable(t *testing.T) {
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256("")
	if got := RawHash(nil); got != want {
		t.Errorf("RawHash(nil) = %s", got)
	}
	if RawHash([]byte("a")) == RawHash([]byte("b")) {
		t.Error("distinct inputs collide")
	}
}

func TestLayerLocator_AliasedLayersAndRealPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(path, []byte(legacyDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	const shipped = "recipe: shipped\nversion: 2.0.0\nsteps:\n  - id: a\n    title: A\n"
	loc := &LayerLocator{
		Aliases: legacyAliases,
		Layers: []Layer{
			{Source: dir, FS: os.DirFS(dir), Dir: dir},
			{Source: BuiltinSource, FS: fstest.MapFS{"shipped.yaml": {Data: []byte(shipped)}}},
		},
	}
	f, err := loc.Locate("legacy")
	if err != nil || f.Path != path || f.Source != dir || f.Hash != RawHash([]byte(legacyDoc)) {
		t.Fatalf("Locate legacy: %v %+v", err, f)
	}
	b, err := loc.Locate("shipped@2.0.0")
	if err != nil || b.Source != BuiltinSource || b.Path != "builtin:shipped.yaml" || b.Hash != RawHash([]byte(shipped)) {
		t.Fatalf("Locate shipped: %v %+v", err, b)
	}
	infos, err := loc.List()
	if err != nil || len(infos) != 2 || infos[0].Path != path || infos[0].Source != dir ||
		infos[1].Source != BuiltinSource || infos[1].Path != "shipped.yaml" {
		t.Fatalf("List = %+v, %v", infos, err)
	}
	var _ Locator = loc
}

func TestLayerLocator_FirstLayerWinsPinnedSearchesAll(t *testing.T) {
	mk := func(v string) fstest.MapFS {
		return fstest.MapFS{"x.yaml": {Data: []byte("flow: x\nversion: " + v + "\nsteps:\n  - id: a\n    title: A\n")}}
	}
	loc := &LayerLocator{Layers: []Layer{{Source: "one", FS: mk("1.0.0")}, {Source: "two", FS: mk("2.0.0")}}}
	if f, err := loc.Locate("x"); err != nil || f.Version != "1.0.0" || f.Source != "one" {
		t.Errorf("unpinned: %v %+v", err, f)
	}
	if f, err := loc.Locate("x@2.0.0"); err != nil || f.Source != "two" {
		t.Errorf("pinned: %v %+v", err, f)
	}
}

func TestLayerLocator_MissingInvalidAndBroken(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("recipe: broken\nversion: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc := &LayerLocator{Aliases: legacyAliases, Layers: []Layer{{Source: dir, FS: os.DirFS(dir), Dir: dir}}}

	_, err := loc.Locate("nope")
	var nf *FlowNotFoundError
	if !errors.As(err, &nf) || nf.Ref != "nope" || len(nf.Dirs) != 1 || nf.Dirs[0] != dir {
		t.Errorf("missing: %v", err)
	}
	if _, err = loc.Locate("Bad Name"); err == nil || !strings.Contains(err.Error(), "invalid flow reference") {
		t.Errorf("bad ref: %v", err)
	}
	if _, err = loc.Locate("broken"); err == nil || !strings.HasPrefix(err.Error(), broken+": ") {
		t.Errorf("broken: %v; want real path prefix %s", err, broken)
	}
}

func TestLayerLocator_PathRefUsesAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(path, []byte(legacyDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := (&LayerLocator{Aliases: legacyAliases}).Locate(path)
	if err != nil || f.Name != "legacy" || f.Source != dir {
		t.Errorf("path ref: %v %+v", err, f)
	}
}
