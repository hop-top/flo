package digest_test

import (
	"encoding/json"
	"os"
	"testing"

	"hop.top/flo/internal/access/digest"
	"hop.top/flo/internal/core"
)

// vector is one case of testdata/vectors.json. The cases are the inputs of
// the bos request-digest tests (one request through different key orders
// and spacings; a changed argument, number, nesting, actor, person, scope
// or operation changes the digest) plus the forms the canonical encoding
// is easy to get wrong: empty input, HTML characters, exact numbers,
// non-ASCII text, command requests. The canonical text and the digest were
// produced by the bos implementation itself.
type vector struct {
	Name      string `json:"name"`
	Operation string `json:"operation"`
	Scope     string `json:"scope"`
	Actor     string `json:"actor"`
	ActingFor string `json:"acting_for"`
	Input     string `json:"input"`
	Canonical string `json:"canonical"`
	Digest    string `json:"digest"`
}

func load(t *testing.T) []vector {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 10 {
		t.Fatalf("only %d vectors", len(vs))
	}
	return vs
}

func request(v vector) digest.Request {
	return digest.Request{
		Operation: v.Operation, Scope: v.Scope, Actor: v.Actor, ActingFor: v.ActingFor,
		Input: json.RawMessage(v.Input),
	}
}

func TestGoldenVectorsByteForByte(t *testing.T) {
	for _, v := range load(t) {
		t.Run(v.Name, func(t *testing.T) {
			c, err := request(v).Canonical()
			if err != nil {
				t.Fatal(err)
			}
			if string(c) != v.Canonical {
				t.Errorf("canonical\n got %s\nwant %s", c, v.Canonical)
			}
			d, err := request(v).Digest()
			if err != nil {
				t.Fatal(err)
			}
			if d != v.Digest {
				t.Errorf("digest = %s, want %s", d, v.Digest)
			}
			if d != digest.Of([]byte(v.Canonical)) {
				t.Errorf("Of(canonical) differs from Digest")
			}
		})
	}
}

// One request has one digest whatever the key order or spacing; every
// change to the request changes it (the property bos pins across its
// surfaces).
func TestVectorsSameAndDifferent(t *testing.T) {
	byName := map[string]string{}
	for _, v := range load(t) {
		byName[v.Name] = v.Digest
	}
	if byName["base"] != byName["reordered"] {
		t.Error("key order or spacing changed the digest")
	}
	for _, n := range []string{
		"changed argument", "changed number", "changed nested", "extra key", "empty text",
		"other actor", "other person", "other scope", "other operation",
	} {
		if byName[n] == byName["base"] {
			t.Errorf("%s kept the digest", n)
		}
	}
}

func TestNoTrailingNewlineNoHTMLEscape(t *testing.T) {
	c, err := digest.Request{Operation: "a.B", Input: json.RawMessage(`{"x":"<&>"}`)}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if c[len(c)-1] == '\n' {
		t.Error("trailing newline")
	}
	want := `{"acting_for":"","actor":"","input":{"x":"<&>"},"operation":"a.B","scope":"","v":1}`
	if string(c) != want {
		t.Errorf("canonical = %s", c)
	}
}

func TestInputRejectsTrailingData(t *testing.T) {
	if _, err := (digest.Request{Input: json.RawMessage(`{} {}`)}).Digest(); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := (digest.Request{Input: json.RawMessage(`{`)}).Digest(); err == nil {
		t.Error("truncated input accepted")
	}
}

func TestInputOfGoValue(t *testing.T) {
	raw, err := digest.Input(nil)
	if err != nil || string(raw) != `{}` {
		t.Fatalf("nil: %s %v", raw, err)
	}
	raw, err = digest.Input(map[string]any{"b": 1, "a": []any{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := digest.Request{Input: raw}.Digest()
	d2, _ := digest.Request{Input: json.RawMessage(`{"a":["x"],"b":1}`)}.Digest()
	if d1 != d2 {
		t.Error("a Go map and its JSON have different digests")
	}
	if _, err := digest.Input(make(chan int)); !core.IsCode(err, core.CodeInternal) {
		t.Errorf("unmarshalable input: %v", err)
	}
}

func TestOperationMapping(t *testing.T) {
	for _, tc := range []struct{ cap, node, want string }{
		{"mail.send", "inbox/apply@1", "mail.send"}, // the item capability wins
		{"", "inbox/apply@1", "inbox.apply"},        // else node ns.name
		{"", "flo/ai.classify@2", "flo.ai.classify"},
		{"", "exec", "exec"},                                             // a core kind
		{"", "scheduling.HoldReservation", "scheduling.HoldReservation"}, // already mapped
	} {
		if got := digest.Operation(tc.cap, tc.node); got != tc.want {
			t.Errorf("Operation(%q, %q) = %q, want %q", tc.cap, tc.node, got, tc.want)
		}
	}
}

func TestForRunMapping(t *testing.T) {
	r, err := digest.ForRun("acme", "inbox.apply", "noor", "jad", map[string]any{"id": "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope != "acme" || r.Actor != "noor" || r.ActingFor != "jad" || r.Operation != "inbox.apply" {
		t.Errorf("request = %+v", r)
	}
}
