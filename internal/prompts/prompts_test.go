package prompts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/prompts"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, body string) *prompts.Prompt {
	t.Helper()
	p, err := prompts.Parse(prompts.Ref{Name: "p", Version: 1}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseRef(t *testing.T) {
	for _, bad := range []string{"", "p", "p@0", "p@x", "../p@1", "a/b@1", "P@1", "p@01"} {
		if _, err := prompts.ParseRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	r, err := prompts.ParseRef("meeting-brief@12")
	if err != nil || r.Name != "meeting-brief" || r.Version != 12 || r.File() != "meeting-brief@12.yaml" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRender(t *testing.T) {
	p := load(t, "system: be brief\ntemplate: |\n  {{ .who }}: {{ join \", \" .items }} / {{ trunc 3 .long }} / {{ default \"none\" .empty }}\n")
	got, err := p.Render(map[string]any{"who": "ann", "items": []any{"a", 2, "c"}, "long": "héllo", "empty": ""})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "ann: a, 2, c / hél / none" {
		t.Errorf("got %q", got)
	}
	if p.System != "be brief" {
		t.Errorf("system %q", p.System)
	}
}

func TestMissingKeyIsAnError(t *testing.T) {
	p := load(t, "template: \"{{ .nope }}\"\n")
	if _, err := p.Render(map[string]any{"x": 1}); err == nil {
		t.Fatal("a missing key rendered")
	}
	if _, err := p.Render(nil); err == nil {
		t.Fatal("a nil context rendered a missing key")
	}
}

func TestNoOtherFuncs(t *testing.T) {
	if _, err := prompts.Parse(prompts.Ref{Name: "p", Version: 1}, []byte("template: '{{ env \"HOME\" }}'\n")); err == nil {
		t.Fatal("env is callable")
	}
}

func TestParseRejects(t *testing.T) {
	for name, body := range map[string]string{
		"no template":  "system: x\n",
		"blank":        "template: '  '\n",
		"unknown key":  "template: x\nextra: 1\n",
		"bad template": "template: '{{ .a '\n",
	} {
		if _, err := prompts.Parse(prompts.Ref{Name: "p", Version: 1}, []byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestHashIsTheFileHash(t *testing.T) {
	body := "template: hi\n"
	sum := sha256.Sum256([]byte(body))
	if p := load(t, body); p.Hash != hex.EncodeToString(sum[:]) {
		t.Errorf("hash %s", p.Hash)
	}
	if load(t, body).Hash == load(t, "template: ho\n").Hash {
		t.Error("hash ignores the text")
	}
}

func TestResolutionPrecedence(t *testing.T) {
	bundle, packs := t.TempDir(), t.TempDir()
	pk := write(t, packs, "inbox/prompts/p@1.yaml", "template: from-pack\n")
	pk2 := write(t, packs, "other/prompts/q@2.yaml", "template: only-pack\n")
	l := prompts.Loader{Packs: []prompts.PackFiles{{Pack: "inbox", Files: []string{pk}}, {Pack: "other", Files: []string{pk2}}}}

	p, err := l.Load(bundle, "p@1")
	if err != nil || p.Template != "from-pack\n" || p.Source != prompts.SourcePack || p.Pack != "inbox" {
		t.Fatalf("pack: %+v %v", p, err)
	}
	write(t, bundle, "prompts/p@1.yaml", "template: from-bundle\n")
	p, err = l.Load(bundle, "p@1")
	if err != nil || p.Template != "from-bundle\n" || p.Source != prompts.SourceBundle {
		t.Fatalf("bundle must win: %+v %v", p, err)
	}
	if p, err = l.Load("", "q@2"); err != nil || p.Pack != "other" {
		t.Fatalf("%+v %v", p, err)
	}
	// versions are separate files
	if _, err = l.Load(bundle, "p@2"); err == nil || core.CodeOf(err) != core.CodeNotFound {
		t.Fatalf("p@2: %v", err)
	}
}

func TestBrokenBundleFileDoesNotFallThrough(t *testing.T) {
	bundle, packs := t.TempDir(), t.TempDir()
	write(t, bundle, "prompts/p@1.yaml", "template: '{{ .a '\n")
	pk := write(t, packs, "x/p@1.yaml", "template: ok\n")
	l := prompts.Loader{Packs: []prompts.PackFiles{{Pack: "x", Files: []string{pk}}}}
	if _, err := l.Load(bundle, "p@1"); err == nil {
		t.Fatal("a broken bundle prompt fell through to the pack")
	}
}
