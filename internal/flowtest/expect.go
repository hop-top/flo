package flowtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"hop.top/flo/internal/core"
)

// Expect is one `expect:` entry of a bundle's tests/ documents: what a step
// of the test run must have ended with.
//
//	# tests/findings.yaml
//	state: { seen_adv: [GHSA-aaaa] }   # optional: this case runs on this state
//	expect:
//	  - step: newadv
//	    output: { items: [{ ghsa_id: GHSA-aaaa }] }
//	  - step: findings
//	    status: skipped
//
// Status defaults to succeeded. Output, when present, is compared as a subset:
// every key it names must be in the step's output with that value (recursively);
// lists and scalars must be equal.
type Expect struct {
	Step      string
	Status    string
	Output    any
	HasOutput bool
	// Source is the document the entry came from, for messages.
	Source string
}

type expectDoc struct {
	// State seeds the flow state of the test case this document is.
	State  map[string]any `yaml:"state"`
	Expect []struct {
		Step   string    `yaml:"step"`
		Status string    `yaml:"status"`
		Output yaml.Node `yaml:"output"`
	} `yaml:"expect"`
}

// Case is one tests/ document: its expectations, and the state it seeds the
// flow with when it has a `state:` map (such a case runs the flow on its own).
type Case struct {
	// Name is the file name.
	Name    string
	State   map[string]any
	Expects []Expect
}

// LoadExpect reads the expect blocks of <bundle>/tests/*.yaml, in file order.
// A bundle without tests/ has none; a single-file flow (empty bundle) has none.
func LoadExpect(bundle string) ([]Expect, error) {
	cases, err := LoadCases(bundle)
	var out []Expect
	for _, c := range cases {
		out = append(out, c.Expects...)
	}
	return out, err
}

// LoadCases reads the documents of <bundle>/tests/, in file order.
func LoadCases(bundle string) ([]Case, error) {
	if bundle == "" {
		return nil, nil
	}
	dir := filepath.Join(bundle, "tests")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "tests/: %v", err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var out []Case
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file of the bundle's tests/
		if err != nil {
			return nil, core.Errorf(core.CodeUsage, "tests/%s: %v", name, err)
		}
		var doc expectDoc
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
			return nil, core.Errorf(core.CodeUsage, "tests/%s: %v", name, err).WithSuggestion("a tests document is `expect: [{step, status?, output?}]`")
		}
		c := Case{Name: name, State: doc.State}
		for i, x := range doc.Expect {
			if x.Step == "" {
				return nil, core.Errorf(core.CodeUsage, "tests/%s: expect[%d] has no step", name, i)
			}
			ex := Expect{Step: x.Step, Status: x.Status, Source: "tests/" + name}
			if ex.Status == "" {
				ex.Status = "succeeded"
			}
			if x.Output.Kind != 0 {
				var v any
				if err := x.Output.Decode(&v); err != nil {
					return nil, core.Errorf(core.CodeUsage, "tests/%s: expect[%d].output: %v", name, i, err)
				}
				ex.Output, ex.HasOutput = v, true
			}
			c.Expects = append(c.Expects, ex)
		}
		out = append(out, c)
	}
	return out, nil
}

// Check compares the step records of a test run with the expectations and
// returns how many held and the problems of the ones that did not.
func Check(steps []Step, expects []Expect) (held int, problems []Problem) {
	for _, ex := range expects {
		var got *Step
		for i := range steps {
			if steps[i].ID == ex.Step && steps[i].Index == nil {
				got = &steps[i]
				break
			}
		}
		fail := func(format string, a ...any) {
			problems = append(problems, Problem{Kind: ProblemExpect, Step: ex.Step, Message: ex.Source + ": " + fmt.Sprintf(format, a...)})
		}
		switch {
		case got == nil:
			fail("step %s is not in the run", ex.Step)
		case got.Status != ex.Status:
			fail("step %s ended %s, expected %s", ex.Step, got.Status, ex.Status)
		case ex.HasOutput:
			if where, ok := subset(norm(ex.Output), norm(got.Output), ""); !ok {
				fail("step %s output differs at %s", ex.Step, where)
			} else {
				held++
			}
		default:
			held++
		}
	}
	return held, problems
}

// norm puts a value in JSON's types, so a 2 and a 2.0 are equal.
func norm(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// subset reports whether got holds everything want names, and where it does
// not.
func subset(want, got any, path string) (string, bool) {
	at := func() string {
		if path == "" {
			return "the output"
		}
		return path
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return at(), false
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, ok := g[k]
			if !ok {
				return join(path, k), false
			}
			if where, ok := subset(w[k], gv, join(path, k)); !ok {
				return where, false
			}
		}
		return "", true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return at(), false
		}
		for i := range w {
			if where, ok := subset(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); !ok {
				return where, false
			}
		}
		return "", true
	}
	if reflect.DeepEqual(want, got) {
		return "", true
	}
	return at(), false
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}
