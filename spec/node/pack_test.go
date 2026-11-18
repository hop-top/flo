package node

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadInboxPack(t *testing.T) {
	lp, err := LoadPack(os.DirFS("testdata/pack/inbox"), "pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(lp.Nodes) != 4 || len(lp.Trusts) != 1 || lp.Trusts[0].Trust != "inbox" {
		t.Fatalf("nodes=%d trusts=%d", len(lp.Nodes), len(lp.Trusts))
	}
	if got := lp.Namespaces(); len(got) != 1 || got[0] != "inbox" {
		t.Errorf("Namespaces = %v", got)
	}
	e := lp.Entries()[0]
	if e.Source.Layer != LayerPack || e.Source.Pack != "inbox" || e.Source.PackVersion != "0.1.0" {
		t.Errorf("source = %+v", e.Source)
	}
	if !lp.Pack.Satisfies("0.1.0") || lp.Pack.Satisfies("0.0.9") {
		t.Error("requires.flo >=0.1 not applied")
	}
}

func TestPackRefusals(t *testing.T) {
	node := func(ref string) string {
		return "node: " + ref + "\nkind: command\nargv: [x, \"{{ config.inbox.nope }}\"]\ninputs: {}\noutputs: { schema: { type: object } }\ncapabilities: []\nside_effect: none\nprivacy: local\n"
	}
	cases := []struct {
		name, manifest, node, want string
	}{
		{"undeclared config key", "pack: inbox\nversion: 0.1.0\nconfig: { inbox: { root: { type: string } } }\nnodes: [n.yaml]\n", node("inbox/a@1"), "inbox.nope"},
		{"undeclared config ns", "pack: inbox\nversion: 0.1.0\nnodes: [n.yaml]\n", node("inbox/a@1"), "namespace"},
		{"bad version", "pack: inbox\nversion: one\n", "", "version"},
		{"path escape", "pack: inbox\nversion: 0.1.0\nnodes: [../n.yaml]\n", "", "inside the pack"},
		{"unknown key", "pack: inbox\nversion: 0.1.0\nbogus: 1\n", "", "bogus"},
		{"bad constraint", "pack: inbox\nversion: 0.1.0\nrequires: { flo: \"~>1\" }\n", "", "constraint"},
		{"duplicate node", "pack: inbox\nversion: 0.1.0\nconfig: { inbox: { nope: { type: string } } }\nnodes: [n.yaml, m.yaml]\n", node("inbox/a@1"), "declared by"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := fstest.MapFS{"pack.yaml": {Data: []byte(c.manifest)}}
			if c.node != "" {
				fsys["n.yaml"] = &fstest.MapFile{Data: []byte(c.node)}
				fsys["m.yaml"] = &fstest.MapFile{Data: []byte(c.node)}
			}
			_, err := LoadPack(fsys, "pack.yaml")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v; want it to mention %q", err, c.want)
			}
			var es Errors
			if !errors.As(err, &es) {
				t.Errorf("error is not typed Errors: %T", err)
			}
		})
	}
}

func TestPackXKeysAtRoot(t *testing.T) {
	p, err := ParsePack(strings.NewReader("pack: a\nversion: 1.0.0\nx-owner: me\n"), "pack.yaml")
	if err != nil || p.Ext["x-owner"] != "me" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := ParsePack(strings.NewReader("pack: a\nversion: 1.0.0\nconfig: { a: { b: { type: string, x-note: 1 } } }\n"), "pack.yaml"); err == nil {
		t.Error("x-* below the root must stay an error")
	}
}

func TestTrustProviderRules(t *testing.T) {
	cases := map[string]string{
		"missing read":        "trust: t\nkind: command\n",
		"irreversible":        "trust: t\nkind: command\nside_effect: irreversible\nread: { argv: [a] }\n",
		"http op needs req":   "trust: t\nkind: http\nread: { argv: [a] }\n",
		"scope":               "trust: t\nkind: command\nread: { argv: [a, \"{{ results.x }}\"] }\n",
		"bad kind":            "trust: t\nkind: plugin\nread: { argv: [a] }\n",
		"secret in header":    "trust: t\nkind: http\nread: { request: { method: GET, url: \"https://x.test\", headers: { Authorization: x } } }\n",
		"command op, request": "trust: t\nkind: command\nread: { argv: [a], request: { method: GET, url: x } }\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadTrust(strings.NewReader(doc), "t.yaml"); err == nil {
				t.Error("want an error")
			}
		})
	}
	ok := "trust: t\nkind: command\nread: { argv: [a, \"{{ in.x }}\"] }\n"
	if _, err := LoadTrust(strings.NewReader(ok), "t.yaml"); err != nil {
		t.Errorf("valid provider refused: %v", err)
	}
}
