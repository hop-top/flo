package serve_test

import (
	"os"
	"path/filepath"
	"testing"

	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/prompts"
)

func TestPackPromptsResolve(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "brief")
	if err := os.MkdirAll(filepath.Join(root, "prompts"), 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := "pack: brief\nversion: 0.1.0\nprompts: [prompts/meeting@1.yaml]\n"
	for name, body := range map[string]string{
		serve.ManifestFile:       manifest,
		"prompts/meeting@1.yaml": "template: hello {{ .who }}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := serve.LoadPacks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Prompts().Load("", "meeting@1")
	if err != nil || got.Source != prompts.SourcePack || got.Pack != "brief" {
		t.Fatalf("%+v %v", got, err)
	}
	if s, err := got.Render(map[string]any{"who": "x"}); err != nil || s != "hello x" {
		t.Errorf("%q %v", s, err)
	}
}
