package flowcheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
	"hop.top/flo/spec/run"
)

// builtins are the flo/* nodes the worked flows use, as node.md §6 gives
// them. Only the contract matters here: inputs, privacy, side effect.
var builtins = map[string]string{
	"dedupe.yaml": `node: flo/dedupe@1
kind: builtin
inputs:
  items: { type: array, required: true }
  key: { type: string, required: true }
  seen: { type: array, default: [] }
  max: { type: integer, default: 10000 }
outputs: { schema: { type: object } }
capabilities: []
side_effect: none
privacy: local
`,
	"classify.yaml": `node: flo/ai.classify@1
kind: builtin
inputs:
  items: { type: array, required: true }
  labels: { type: array, required: true }
  model: { type: [string, array], items: { type: string } }
  instructions: { type: string }
  abstain: { type: boolean }
  engine: { type: string, enum: [local, jev, auto] }
outputs: { schema: { type: object } }
capabilities: []
side_effect: none
privacy: cloud
`,
	"generate.yaml": `node: flo/ai.generate@1
kind: builtin
inputs:
  prompt: { type: string, required: true }
  context: { type: object }
  model: { type: [string, array], items: { type: string }, required: true }
outputs: { schema: { type: object } }
capabilities: []
side_effect: none
privacy: cloud
`,
	"approval.yaml": `node: flo/approval@1
kind: builtin
inputs:
  items: { type: array, required: true }
  surface: { type: string, enum: [tlc, none] }
  trust: { type: string }
  context: { type: object }
  verdicts: { type: array }
  title: { type: string }
outputs: { schema: { type: object } }
capabilities: []
side_effect: write-shared
privacy: local
`,
	"output.yaml": `node: flo/output@1
kind: builtin
inputs:
  kind: { type: string, required: true, enum: [findings, brief] }
  title: { type: string }
  body: { type: string }
  items: { type: array }
outputs: { schema: { type: object } }
capabilities: []
side_effect: write-local
privacy: local
`,
}

// testLocator layers the node-spec examples, the inbox pack's trust
// provider and the built-ins above.
func testLocator(t *testing.T, extra map[string]string) *node.Locator {
	t.Helper()
	files, _ := filepath.Glob("../node/testdata/examples/*.yaml")
	fsys := fstest.MapFS{}
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // fixed glob
		if err != nil {
			t.Fatal(err)
		}
		fsys["packs/"+filepath.Base(f)] = &fstest.MapFile{Data: data}
	}
	for name, doc := range extra {
		fsys["packs/"+name] = &fstest.MapFile{Data: []byte(doc)}
	}
	packs, err := node.LoadDir(fsys, "packs", node.Source{Layer: node.LayerPack, Pack: "test", PackVersion: "0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	bfs := fstest.MapFS{}
	for name, doc := range builtins {
		bfs["b/"+name] = &fstest.MapFile{Data: []byte(doc)}
	}
	bi, err := node.LoadDir(fs.FS(bfs), "b", node.Source{Layer: node.LayerBuiltin})
	if err != nil {
		t.Fatal(err)
	}
	trust, err := node.LoadTrustDir(os.DirFS("../node/testdata/pack/inbox"), "trusts", node.Source{Layer: node.LayerPack, Pack: "inbox", PackVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	loc, err := node.NewLocator(node.Layers{Packs: packs, Builtin: bi, PackTrusts: trust})
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

func exampleFlow(t *testing.T, name string) *spec.Flow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "spec", "examples.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range yamlFence.FindAllStringSubmatch(string(data), -1) {
		if strings.HasPrefix(m[1], "flow: "+name+"\n") {
			f, err := spec.ParseFlow(strings.NewReader(m[1]), name+".yaml")
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			return f
		}
	}
	t.Fatalf("no flow %q", name)
	return nil
}

func parse(t *testing.T, doc string) *spec.Flow {
	t.Helper()
	f, err := spec.ParseFlow(strings.NewReader(doc), "t.yaml")
	if err != nil {
		t.Fatalf("fixture must parse: %v", err)
	}
	return f
}

func opts(t *testing.T) Options {
	return Options{Nodes: testLocator(t, nil), AllowAgent: true, AllowSubFlow: true}
}

// The nodes of inbox-triage's judgment (packs/inbox/nodes/rows.yaml and
// decisions.yaml, reduced to what the flow check reads).
var inboxJudgeNodes = map[string]string{
	"inbox_rows_1.yaml": `node: inbox/rows@1
kind: command
lowers_to: engine
argv: [inbox-rows, --out, "{{ run.step_dir }}/out.json", "{{ in.run_dir }}"]
inputs: { run_dir: { type: string, required: true } }
outputs: { from: "file:out.json", format: json, schema: { type: object } }
side_effect: read
privacy: local
capabilities: [mail.read]
timeout: 60s
`,
	"inbox_decisions_1.yaml": `node: inbox/decisions@1
kind: command
lowers_to: engine
argv: [inbox-decisions, --out, "{{ run.step_dir }}/out.json", "{{ in.run_dir }}"]
stdin: { json: "{{ in.labels }}" }
inputs:
  run_dir: { type: string, required: true }
  labels: { type: array, required: true }
  weights: { type: array, required: true }
  sop_labels: { type: array, required: true }
  engine: { type: [string, 'null'], default: null }
  model: { type: [string, 'null'], default: null }
outputs: { from: "file:out.json", format: json, schema: { type: object } }
side_effect: write-local
idempotent: true
privacy: local
capabilities: [mail.read]
timeout: 60s
`,
}

// The three worked flows agree with their node specs.
func TestExampleFlowsClean(t *testing.T) {
	for _, name := range []string{"dependency-watch", "meeting-brief", "inbox-triage"} {
		t.Run(name, func(t *testing.T) {
			o := opts(t)
			if name == "inbox-triage" {
				o.Nodes = testLocator(t, inboxJudgeNodes)
			}
			if r := Check(exampleFlow(t, name), o); len(r) > 0 {
				t.Errorf("%v", r)
			}
		})
	}
}

func issueWith(r Report, rule, code string, frag ...string) bool {
	for _, i := range r {
		if i.Rule == rule && i.Code == code && (len(frag) == 0 || strings.Contains(i.Msg, frag[0])) {
			return true
		}
	}
	return false
}

const head = "flow: t\nversion: 1.0.0\n"

func TestRules(t *testing.T) {
	cases := []struct {
		name, rule, code, doc string
		warn                  bool
		frag                  string
	}{
		{"V19 cloud node, local-only flow", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: d, uses: gh/dependency-list@1, with: { repos: [a/b] } }\n", false, ""},
		{"V19 hosted model under local-only", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'anthropic://claude' } }\n", false, ""},
		{"V19 public ollama host under local-only", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://ollama.example.com:11434/gemma' } }\n", false, ""},
		{"V19 base_url past a local host", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'openai://localhost:8000/m?base_url=https://api.openai.com/v1' } }\n", false, "is cloud"},
		{"V19 tailnet host is cloud unless listed", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://100.64.1.2:11434/gemma' } }\n", false, "is cloud"},
		{"V19 empty authority is not runnable", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama:///llama3.1' } }\n", false, "ollama://<model>"},
		{"V19 portless localhost is not runnable", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://localhost/llama3.1' } }\n", false, "scheme://host:port/<model>"},
		{"V19 unix socket is not runnable, even under cloud-allowed", "V19", CodePrivacyViolation,
			head + "privacy: cloud-allowed\nsteps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://unix:/run/ollama.sock' } }\n", false, "unix socket"},
		{"V19 a list of hosted models under local-only", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: ['anthropic://claude', 'openai://gpt-5'] } }\n", false, "is cloud"},
		{"V19 jev alone under local-only", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: c, uses: flo/ai.classify@1, with: { items: [], labels: [a, b], model: [jev] } }\n", false, "is cloud"},
		{"V19 a list with an entry that is not runnable", "V19", CodePrivacyViolation,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: ['ollama://gemma', 'ollama:///x'] } }\n", false, "ollama://<model>"},
		{"V11 a list of non-strings", "V11", CodeInvalidFlow,
			head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: [1, 2] } }\n", false, "want string"},
		{"V20 irreversible, no approval", "V20", CodeUnguardedIrreversible,
			head + "privacy: local-only\nsteps:\n  - { id: f, uses: inbox/fetch@1 }\n  - { id: a, uses: inbox/apply@1, depends_on: [f], with: { run: x } }\n", false, ""},
		{"V20 an exec step does not guard", "V20", CodeUnguardedIrreversible,
			head + "steps:\n  - { id: ok, kind: exec, exec: { argv: [true] } }\n  - { id: a, uses: inbox/apply@1, depends_on: [ok], with: { run: x } }\n", false, "ancestor flo/approval"},
		{"V12 unknown node", "V12", CodeNodeNotFound,
			head + "steps:\n  - { id: a, uses: nope/none@1 }\n", false, "not found"},
		{"V12 unpinned sub-flow", "V12", CodeInvalidFlow,
			head + "steps:\n  - { id: a, uses: child }\n", false, "exact version"},
		{"V11 with key outside inputs", "V11", CodeInvalidFlow,
			head + "steps:\n  - { id: a, uses: tlc/task.list@1, with: { bogus: 1 } }\n", false, "not an input"},
		{"V11 required input missing", "V11", CodeInvalidFlow,
			head + "steps:\n  - { id: a, uses: tlc/task.create@1, with: { description: x } }\n", false, "required input"},
		{"V11 literal of the wrong type", "V11", CodeInvalidFlow,
			head + "steps:\n  - { id: a, uses: tlc/task.create@1, with: { title: [1, 2] } }\n", false, "want string"},
		{"V22 retry on non-idempotent", "V22", CodeInvalidFlow,
			head + "steps:\n  - { id: a, uses: inbox/fetch@1, retry: { max_attempts: 3 } }\n", false, ""},
		{"V27 source is not a trigger node", "V27", CodeInvalidFlow,
			head + "on: [ { calendar: { source: ctxt/find@1 } } ]\nprivacy: local-only\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n", false, "not a trigger node"},
		{"V27 unknown source", "V27", CodeNodeNotFound,
			head + "on: [ { event: { source: nope/events@1 } } ]\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n", false, "not found"},
		{"V27 when reads outside trigger", "V27", CodeInvalidFlow,
			head + "on: [ { calendar: { source: calendar/events@1, when: '{{ vars.x }}' } } ]\nvars: { x: {} }\nsteps: [ { id: a, kind: exec, exec: { argv: [x] } } ]\n", false, `reads "vars"`},
		{"D5 agent step", "D5", CodeEngineFeature,
			head + "steps:\n  - { id: a, title: think }\n", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := opts(t)
			o.AllowAgent = c.rule != "D5"
			r := Check(parse(t, c.doc), o)
			if !issueWith(r, c.rule, c.code, c.frag) {
				t.Fatalf("want %s/%s %q, got %v", c.rule, c.code, c.frag, r)
			}
			if c.rule == "V22" && r.HasErrors() {
				t.Errorf("V22 is a warning: %v", r)
			}
		})
	}
}

func TestLegalVariants(t *testing.T) {
	cases := map[string]string{
		"approval ancestor guards": head + "privacy: local-only\nsteps:\n" +
			"  - { id: f, uses: inbox/fetch@1 }\n" +
			"  - { id: ap, uses: flo/approval@1, depends_on: [f], with: { items: '{{ results.f.items }}' } }\n" +
			"  - { id: a, uses: inbox/apply@1, depends_on: [ap], with: { run: x } }\n",
		// the guard is found through a data edge, not only depends_on.
		"guard through results edge": head + "steps:\n" +
			"  - { id: ap, uses: flo/approval@1, with: { items: [] } }\n" +
			"  - { id: a, uses: inbox/apply@1, with: { run: \"{{ results.ap.approved }}\" } }\n",
		"local model under local-only":               head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://gemma' } }\n",
		"ollama on a private host":                   head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://10.0.0.4:11434/gemma' } }\n",
		"ollama default daemon, tagged model":        head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://gemma4:e4b' } }\n",
		"ollama dotted model":                        head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'ollama://llama3.1' } }\n",
		"a list with a local model under local-only": head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: ['anthropic://claude', 'ollama://gemma'] } }\n",
		"local then jev under local-only":            head + "steps:\n  - { id: c, uses: flo/ai.classify@1, with: { items: [], labels: [a, b], model: ['ollama://gemma', jev] } }\n",
		"lmstudio default is localhost":              head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'lmstudio://qwen' } }\n",
		"base_url to localhost":                      head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'openai://m?base_url=http://localhost:8000/v1' } }\n",
		"private-network openai":                     head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: 'openai://192.168.1.5:8080/m' } }\n",
		"retry on an idempotent node":                head + "steps:\n  - { id: a, uses: tlc/task.list@1, with: { search: x }, retry: { max_attempts: 3 } }\n",
		"narrowing step privacy":                     head + "privacy: cloud-allowed\nsteps:\n  - { id: a, uses: tlc/task.list@1, with: { search: x }, privacy: local-only }\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if r := Check(parse(t, doc), opts(t)); len(r) > 0 {
				t.Errorf("%v", r)
			}
		})
	}
}

func TestRetryWarningIsNotAnError(t *testing.T) {
	r := Check(parse(t, head+"steps:\n  - { id: a, uses: inbox/fetch@1, retry: { max_attempts: 3 } }\n"), opts(t))
	if r.HasErrors() || len(r.Warnings()) != 1 || r.Warnings()[0].Rule != "V22" {
		t.Errorf("%v", r)
	}
}

func TestPhase1RefusalsAreDefault(t *testing.T) {
	doc := head + "steps:\n  - { id: a, title: think }\n  - { id: b, uses: child@1.0.0 }\n"
	r := Check(parse(t, doc), Options{Nodes: testLocator(t, nil)})
	n := 0
	for _, i := range r {
		if i.Code == CodeEngineFeature {
			n++
		}
	}
	if n != 2 {
		t.Errorf("agent and sub-flow are engine_feature by default: %v", r)
	}
	if run.Check(parse(t, doc)).Has("D5") {
		t.Error("run.Check is flow-only and has no phase-1 refusals")
	}
}

// privacy.local_hosts lifts a tailnet host and a named host to local for V19.
func TestLocalHostsOption(t *testing.T) {
	lh, err := node.NewLocalHosts([]string{"100.64.0.0/10", "gpu.internal"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"ollama://100.64.1.2:11434/gemma", "openai://gpu.internal:8000/m"} {
		doc := head + "steps:\n  - { id: g, uses: flo/ai.generate@1, with: { prompt: p@1, model: '" + m + "' } }\n"
		if r := Check(parse(t, doc), opts(t)); !r.HasErrors() {
			t.Errorf("%s: unlisted host passed", m)
		}
		o := opts(t)
		o.LocalHosts = lh
		if r := Check(parse(t, doc), o); len(r) > 0 {
			t.Errorf("%s: listed host: %v", m, r)
		}
	}
}
