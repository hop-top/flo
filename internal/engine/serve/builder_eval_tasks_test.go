//go:build !windows

package serve_test

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// The drafting tasks of the builder model comparison (builder_eval_test.go).
// Each one is an instruction over a synthetic flow, a scripted check of the
// document a model proposes, and a reference solution that proves the check:
// the hermetic test applies the solution (it must pass) and the base (it must
// not). Fixtures: testdata/builder-eval, testdata/builder/greeter and
// testdata/flows/dependency-watch.

type evalTask struct {
	ID          string
	Fixture     string // the flow's name; where it lives is fixtureDir
	Instruction string
	// Check returns nil when the proposed document did what was asked.
	Check func(doc map[string]any) error
	// Solution is the reference next version of the base document.
	Solution func(base string) string
	// Dry marks a task replayed on a recording of the base: the change does not
	// alter the outcome, so the replay is expected to match.
	Dry bool
}

// fixtureDir is where a fixture's bundle is, relative to testdata.
func fixtureDir(name string) []string {
	switch name {
	case "greeter":
		return []string{"builder", "greeter"}
	case "dependency-watch":
		return []string{"flows", "dependency-watch"}
	}
	return []string{"builder-eval", name}
}

// edit replaces each old with its new, once, and bumps the version.
func evEdit(pairs ...string) func(string) string {
	return func(base string) string {
		doc := strings.Replace(base, "version: 0.1.0\n", "version: 0.1.1\n", 1)
		for i := 0; i+1 < len(pairs); i += 2 {
			if !strings.Contains(doc, pairs[i]) {
				panic("reference edit does not apply: " + pairs[i])
			}
			doc = strings.Replace(doc, pairs[i], pairs[i+1], 1)
		}
		return doc
	}
}

const (
	rateStep = `  - id: rate
    uses: flo/ai.classify@1
    depends_on: [keep]
    with:
      model: ollama://gemma4:e4b
      labels: [must-read, skip]
      items: "{{ results.keep.items }}"

  - id: publish
`
	signoffStep = `  - id: signoff
    kind: human
    title: Approve the newsletter
    depends_on: [keep]

  - id: publish
    uses: flo/output@1
    depends_on: [signoff]
`
	farewellStep = `      items: []

  - id: farewell
    uses: flo/output@1
    depends_on: [hello]
    with:
      kind: findings
      title: goodbye
      items: []
`
	splitSteps = `steps:
  - id: publish-errors
    uses: flo/output@1
    with:
      kind: findings
      title: digest
      items: "{{ vars.errors.map(e, {'title': e.title, 'label': 'error'}) }}"

  - id: publish-warnings
    uses: flo/output@1
    with:
      kind: findings
      title: digest
      items: "{{ vars.warnings.map(w, {'title': w.title, 'label': 'warning'}) }}"
`
)

var evalTasks = []evalTask{
	{
		ID: "add-step", Fixture: "greeter",
		Instruction: "Add a step farewell, after hello, that publishes a findings output titled goodbye with no items.",
		Solution:    evEdit("      items: []\n", farewellStep),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "farewell")
			if err != nil {
				return err
			}
			if got := evStr(s["uses"]); got != "flo/output@1" {
				return fmt.Errorf("farewell uses %q", got)
			}
			if w := evWith(s); evStr(w["kind"]) != "findings" || evStr(w["title"]) != "goodbye" {
				return fmt.Errorf("farewell with = %v", w)
			}
			if !evAfter(d, "farewell", "hello") {
				return fmt.Errorf("farewell does not come after hello")
			}
			h, err := stepOf(d, "hello")
			if err != nil {
				return err
			}
			if evStr(evWith(h)["title"]) != "hello" {
				return fmt.Errorf("hello changed: %v", evWith(h))
			}
			return nil
		},
	},
	{
		ID: "schedule", Fixture: "dependency-watch", Dry: true,
		Instruction: "Run the schedule at 06:00 UTC instead of 07:00.",
		Solution:    evEdit("BYHOUR=7", "BYHOUR=6"),
		Check: func(d map[string]any) error {
			var rr string
			manual := false
			for _, t := range evList(d["on"]) {
				m, _ := t.(map[string]any)
				if s, ok := m["schedule"].(map[string]any); ok {
					rr = evStr(s["rrule"])
				}
				if _, ok := m["manual"]; ok {
					manual = true
				}
			}
			switch {
			case !strings.Contains(rr, "BYHOUR=6") || strings.Contains(rr, "BYHOUR=7"):
				return fmt.Errorf("rrule = %q", rr)
			case !manual:
				return fmt.Errorf("the manual trigger was dropped")
			}
			return nil
		},
	},
	{
		ID: "rename-output", Fixture: "dependency-watch", Dry: true,
		Instruction: "Change the title of the findings output to dep-watch.",
		Solution:    evEdit("title: dependency-watch\n", "title: dep-watch\n"),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "findings")
			if err != nil {
				return err
			}
			if got := evStr(evWith(s)["title"]); got != "dep-watch" {
				return fmt.Errorf("title = %q", got)
			}
			if evStr(s["when"]) == "" {
				return fmt.Errorf("the when of findings was dropped")
			}
			return nil
		},
	},
	{
		ID: "when-condition", Fixture: "dependency-watch", Dry: true,
		Instruction: "Run the releases step only when the repo var is not empty.",
		Solution: evEdit("    depends_on: [newadv]\n    with: { repo: \"{{ vars.repo }}\" }",
			"    depends_on: [newadv]\n    when: \"{{ vars.repo != '' }}\"\n    with: { repo: \"{{ vars.repo }}\" }"),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "releases")
			if err != nil {
				return err
			}
			if w := evStr(s["when"]); !strings.Contains(w, "vars.repo") {
				return fmt.Errorf("when = %q", w)
			}
			if evStr(s["uses"]) != "gh/releases@1" || !evHas(s["depends_on"], "newadv") {
				return fmt.Errorf("releases changed beyond the condition: %v", s)
			}
			return nil
		},
	},
	{
		ID: "repoint", Fixture: "newsletter",
		Instruction: "Make the publish step list the posts of the keep step, not of all.",
		Solution: evEdit("    depends_on: [all]\n    with:\n      kind: findings", "    depends_on: [keep]\n    with:\n      kind: findings",
			"results.all.items.map", "results.keep.items.map"),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "publish")
			if err != nil {
				return err
			}
			items := evStr(evWith(s)["items"])
			if !strings.Contains(items, "results.keep") || strings.Contains(items, "results.all") {
				return fmt.Errorf("items = %q", items)
			}
			if !evHas(s["depends_on"], "keep") {
				return fmt.Errorf("publish does not depend on keep")
			}
			return nil
		},
	},
	{
		ID: "privacy", Fixture: "newsletter",
		Instruction: "The newsletter must stay on this machine: make it local-only.",
		Solution:    evEdit("privacy: cloud-allowed", "privacy: local-only"),
		Check: func(d map[string]any) error {
			if got := evStr(d["privacy"]); got != "local-only" {
				return fmt.Errorf("privacy = %q", got)
			}
			if len(evList(d["steps"])) != 3 {
				return fmt.Errorf("steps = %d, want the 3 it had", len(evList(d["steps"])))
			}
			return nil
		},
	},
	{
		ID: "add-vars", Fixture: "newsletter",
		Instruction: "Add a number var min_score with default 5 and use it in the keep step instead of the literal 5.",
		Solution: evEdit("steps:\n", "  min_score: { type: number, default: 5 }\nsteps:\n",
			"item.score >= 5", "item.score >= vars.min_score"),
		Check: func(d map[string]any) error {
			vs, _ := d["vars"].(map[string]any)
			v, _ := vs["min_score"].(map[string]any)
			if v == nil || evStr(v["type"]) != "number" || evStr(v["default"]) != "5" {
				return fmt.Errorf("vars.min_score = %v", v)
			}
			if _, ok := vs["posts"]; !ok {
				return fmt.Errorf("vars.posts was dropped")
			}
			s, err := stepOf(d, "keep")
			if err != nil {
				return err
			}
			if w := evStr(evWith(s)["where"]); !strings.Contains(w, "vars.min_score") || strings.Contains(w, ">= 5") {
				return fmt.Errorf("keep where = %q", w)
			}
			return nil
		},
	},
	{
		ID: "classify", Fixture: "newsletter",
		Instruction: "Add a step rate, after keep, that uses flo/ai.classify@1 with model ollama://gemma4:e4b to label the items of keep with the labels must-read and skip.",
		Solution:    evEdit("  - id: publish\n", rateStep),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "rate")
			if err != nil {
				return err
			}
			w := evWith(s)
			labels := map[string]bool{}
			for _, l := range evList(w["labels"]) {
				labels[evStr(l)] = true
			}
			switch {
			case evStr(s["uses"]) != "flo/ai.classify@1":
				return fmt.Errorf("rate uses %q", evStr(s["uses"]))
			case !strings.HasPrefix(evStr(w["model"]), "ollama://"):
				return fmt.Errorf("model = %q", evStr(w["model"]))
			case len(labels) != 2 || !labels["must-read"] || !labels["skip"]:
				return fmt.Errorf("labels = %v", w["labels"])
			case !strings.Contains(evStr(w["items"]), "results.keep") || !evHas(s["depends_on"], "keep"):
				return fmt.Errorf("rate does not read keep: items %q depends_on %v", evStr(w["items"]), s["depends_on"])
			}
			return nil
		},
	},
	{
		ID: "approval", Fixture: "newsletter",
		Instruction: "Add a human approval step signoff, titled Approve the newsletter, and make publish wait for it.",
		Solution:    evEdit("  - id: publish\n    uses: flo/output@1\n    depends_on: [all]\n", signoffStep),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "signoff")
			if err != nil {
				return err
			}
			// a human step or the approval node: both park for a person
			h, _ := s["human"].(map[string]any)
			title := evStr(s["title"]) + evStr(h["title"]) + evStr(evWith(s)["title"])
			if (evStr(s["kind"]) != "human" && evStr(s["uses"]) != "flo/approval@1") || !strings.Contains(title, "Approve the newsletter") {
				return fmt.Errorf("signoff = %v", s)
			}
			p, err := stepOf(d, "publish")
			if err != nil {
				return err
			}
			if !evHas(p["depends_on"], "signoff") {
				return fmt.Errorf("publish does not wait for signoff: %v", p["depends_on"])
			}
			return nil
		},
	},
	{
		ID: "publish-when", Fixture: "newsletter",
		Instruction: "Publish only when the keep step found at least one post.",
		Solution: evEdit("    depends_on: [all]\n    with:\n      kind: findings",
			"    depends_on: [all, keep]\n    when: \"{{ size(results.keep.items) > 0 }}\"\n    with:\n      kind: findings"),
		Check: func(d map[string]any) error {
			s, err := stepOf(d, "publish")
			if err != nil {
				return err
			}
			w := strings.ReplaceAll(evStr(s["when"]), " ", "")
			sized := strings.Contains(w, "size(") || strings.Contains(w, ".size()")
			positive := strings.Contains(w, ">0") || strings.Contains(w, "!=0") || strings.Contains(w, ">=1")
			if !strings.Contains(w, "results.keep") || !sized || !positive {
				return fmt.Errorf("when = %q", evStr(s["when"]))
			}
			if !evHas(s["depends_on"], "keep") {
				return fmt.Errorf("publish does not depend on keep")
			}
			return nil
		},
	},
	{
		ID: "split-step", Fixture: "digest",
		Instruction: "Split publish into two steps, publish-errors for the errors and publish-warnings for the warnings, each a findings output.",
		Solution: func(base string) string {
			doc := strings.Replace(base, "version: 0.1.0\n", "version: 0.1.1\n", 1)
			return doc[:strings.Index(doc, "steps:\n")] + splitSteps
		},
		Check: func(d map[string]any) error {
			if _, err := stepOf(d, "publish"); err == nil {
				return fmt.Errorf("publish is still there")
			}
			for _, c := range []struct{ id, own, other string }{
				{"publish-errors", "vars.errors", "vars.warnings"},
				{"publish-warnings", "vars.warnings", "vars.errors"},
			} {
				s, err := stepOf(d, c.id)
				if err != nil {
					return err
				}
				w := evWith(s)
				items := evStr(w["items"])
				if evStr(s["uses"]) != "flo/output@1" || evStr(w["kind"]) != "findings" ||
					!strings.Contains(items, c.own) || strings.Contains(items, c.other) {
					return fmt.Errorf("%s = %v", c.id, s)
				}
			}
			return nil
		},
	},
}

// parseDoc reads a document for a check.
func parseDoc(src string) (map[string]any, error) {
	var d map[string]any
	if err := yaml.Unmarshal([]byte(src), &d); err != nil {
		return nil, fmt.Errorf("not yaml: %w", err)
	}
	return d, nil
}

func stepOf(d map[string]any, id string) (map[string]any, error) {
	for _, s := range evList(d["steps"]) {
		if m, ok := s.(map[string]any); ok && evStr(m["id"]) == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no step %q", id)
}

// after is true when step a is listed after step b.
func evAfter(d map[string]any, a, b string) bool {
	ia, ib := -1, -1
	for i, s := range evList(d["steps"]) {
		m, _ := s.(map[string]any)
		switch evStr(m["id"]) {
		case a:
			ia = i
		case b:
			ib = i
		}
	}
	return ia > ib && ib >= 0
}

func evWith(s map[string]any) map[string]any { m, _ := s["with"].(map[string]any); return m }

func evList(v any) []any { l, _ := v.([]any); return l }

func evStr(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func evHas(v any, id string) bool {
	for _, x := range evList(v) {
		if evStr(x) == id {
			return true
		}
	}
	return false
}
