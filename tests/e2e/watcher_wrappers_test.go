//go:build !windows

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/flo/spec/node"
)

// The gh wrappers of packs/github/bin against the fake gh. They run the way
// the nodes run them, minus the engine: a command line, JSON in a file.

func TestGitHubPackDeclaresTheSBOMBucket(t *testing.T) {
	lp, err := node.LoadPack(os.DirFS(filepath.Join(repoRoot, "packs")), "github/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]*node.Spec{}
	for _, n := range lp.Nodes {
		byType[n.Node] = n
	}
	for _, ref := range []string{"gh/dependency-list@1", "gh/advisories@1", "gh/releases-since@1"} {
		n := byType[ref]
		if n == nil {
			t.Fatalf("%s is not in the pack", ref)
		}
		if len(n.Capabilities) != 1 || n.Capabilities[0] != "vcs.read" {
			t.Errorf("%s capabilities %v", ref, n.Capabilities)
		}
	}
	rl := byType["gh/dependency-list@1"].RateLimit
	if rl == nil || rl.Bucket != "vcs-dependency-export" || rl.Max != 100 || rl.Per != "1h" {
		t.Errorf("SBOM rate limit: %+v", rl)
	}
}

type wrapRun struct {
	t    *testing.T
	e    *env
	exit int
	err  string
	out  map[string]any
}

// wrap runs a wrapper from the env's shims and reads its --out file.
func wrap(t *testing.T, e *env, stdin string, name string, args ...string) wrapRun {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "out.json")
	cmd := exec.Command(filepath.Join(e.shims, name), append([]string{"--out", outFile}, args...)...) //nolint:gosec // a script of this repository
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	r := wrapRun{t: t, e: e}
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		r.exit = ee.ExitCode()
	}
	r.err = errb.String()
	if b, err := os.ReadFile(outFile); err == nil { //nolint:gosec // a temp file
		if err := json.Unmarshal(b, &r.out); err != nil {
			t.Fatalf("%s wrote invalid JSON: %v\n%s", name, err, b)
		}
	}
	return r
}

func (r wrapRun) list(key string) []map[string]any {
	r.t.Helper()
	var out []map[string]any
	l, _ := r.out[key].([]any)
	for _, x := range l {
		out = append(out, x.(map[string]any))
	}
	return out
}

func versions(rs []map[string]any) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r["version"].(string))
	}
	return out
}

func TestGHDeps(t *testing.T) {
	e := watcherEnv(t)
	r := wrap(t, e, "", "gh-deps", "--mode", "sbom", "--include-actions", "false", "--",
		"example-org/api", "example-org/web", "example-org/off", "example-org/gone")
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.err)
	}
	type key struct{ eco, name, ver string }
	got := map[key]map[string]any{}
	for _, p := range r.list("packages") {
		got[key{p["ecosystem"].(string), p["name"].(string), p["version"].(string)}] = p
	}
	// versions lose v, "= " and purl escapes; the repo itself and actions are out
	for _, k := range []key{
		{"go", "golang.org/x/net", "0.17.0"}, {"go", "github.com/cli/cli/v2", "2.103.3"},
		{"go", "github.com/example-org/oldlib", "2.8.0"}, {"go", "github.com/hop-top/poly-kit/kit", "0.5.0-beta.12"},
		{"npm", "left-pad", "1.3.0"},
	} {
		if got[k] == nil {
			t.Errorf("missing %+v in %v", k, got)
		}
	}
	if len(got) != 5 {
		t.Errorf("%d packages: %v", len(got), got)
	}
	net := got[key{"go", "golang.org/x/net", "0.17.0"}]
	if used := net["used_by"].([]any); len(used) != 2 {
		t.Errorf("x/net is deduplicated across repositories with both users: %v", net)
	}
	if net["source_repo"] != "golang/net" || got[key{"go", "github.com/cli/cli/v2", "2.103.3"}]["source_repo"] != "cli/cli" {
		t.Errorf("source repositories: %v", got)
	}
	kit := got[key{"go", "github.com/hop-top/poly-kit/kit", "0.5.0-beta.12"}]
	if kit["source_repo"] != "hop-top/poly-kit" || kit["tag_prefix"] != "kit/" {
		t.Errorf("a module in a monorepo carries its tag prefix: %v", kit)
	}
	if _, has := got[key{"npm", "left-pad", "1.3.0"}]["source_repo"]; has {
		t.Error("no source repository is guessed for npm")
	}
	skipped := map[string]string{}
	for _, s := range r.list("skipped") {
		skipped[s["repo"].(string)] = s["reason"].(string)
	}
	if skipped["example-org/off"] != "dependency graph disabled" || skipped["example-org/gone"] != "repository not found" {
		t.Errorf("a 404 is told apart from a disabled graph: %v", skipped)
	}

	r = wrap(t, e, "", "gh-deps", "--mode", "sbom", "--include-actions", "true", "--", "example-org/api")
	var checkout map[string]any
	for _, p := range r.list("packages") {
		if p["name"] == "actions/checkout" {
			checkout = p
		}
	}
	if checkout == nil || checkout["ecosystem"] != "actions" || checkout["version"] != "4.*.*" {
		t.Errorf("actions with include_actions: %v", checkout)
	}
}

func TestGHFailuresExitAsDocumented(t *testing.T) {
	e := watcherEnv(t)
	for _, c := range []struct {
		fail string
		exit int
	}{
		{"502 Bad Gateway", 75}, {"429 Too Many Requests", 75},
		{"403 API rate limit exceeded", 75}, {"401 Bad credentials", 1}, {"403 Forbidden", 1},
	} {
		if err := os.WriteFile(filepath.Join(e.scratch, "http-fail"), []byte(c.fail), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, args := range map[string][]string{
			"gh-deps":       {"--", "example-org/api"},
			"gh-releases":   {"--repo", "cli/cli", "--package", "p", "--version", "1.0.0"},
			"gh-advisories": {"--", "-"},
		} {
			stdin := ""
			if name == "gh-advisories" {
				args = []string{"--"}
				stdin = `[{"ecosystem":"go","name":"golang.org/x/net","version":"0.17.0"}]`
			}
			if r := wrap(t, e, stdin, name, args...); r.exit != c.exit {
				t.Errorf("%s on %q exits %d, want %d (%s)", name, c.fail, r.exit, c.exit, r.err)
			}
		}
	}
}

func TestGHAdvisories(t *testing.T) {
	e := watcherEnv(t)
	pk := `[{"ecosystem":"go","name":"golang.org/x/net","version":"0.17.0","used_by":["a/b"]},
{"ecosystem":"go","name":"golang.org/x/net","version":"0.50.0"},
{"ecosystem":"npm","name":"left-pad","version":"1.3.0","used_by":["a/c"]},
{"ecosystem":"actions","name":"actions/checkout","version":"4.*.*"},
{"ecosystem":"other","name":"x","version":"1"}]`
	r := wrap(t, e, pk, "gh-advisories", "--batch", "50", "--")
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.err)
	}
	var ids []string
	byID := map[string]map[string]any{}
	for _, a := range r.list("advisories") {
		id := a["ghsa_id"].(string) + "@" + a["version"].(string)
		ids = append(ids, id)
		byID[id] = a
	}
	slices.Sort(ids)
	want := []string{"GHSA-5cv4-jp36-h3mw@0.17.0", "GHSA-5cv4-jp36-h3mw@0.50.0", "GHSA-dddd-eeee-ffff@1.3.0", "GHSA-vvgc-356p-c3xw@0.17.0", "GHSA-vvgc-356p-c3xw@0.50.0"}
	if !slices.Equal(ids, want) {
		t.Fatalf("advisories %v, want %v (withdrawn ones are dropped)", ids, want)
	}
	lp := byID["GHSA-dddd-eeee-ffff@1.3.0"]
	if lp["severity"] != "medium" || lp["first_patched"] != "1.3.1" || lp["package"] != "left-pad" {
		t.Errorf("severity is lowercase, the patched version a plain string: %v", lp)
	}
	log := strings.Join(readLines(t, filepath.Join(e.scratch, "gh.log")), "\n")
	if strings.Contains(log, "actions/checkout") || strings.Contains(log, "ecosystem=other") {
		t.Errorf("ranges and unknown ecosystems are not asked about:\n%s", log)
	}
	// one version of a name per call, so a match is unambiguous
	if n := strings.Count(log, "ecosystem=go"); n != 2 {
		t.Errorf("two versions of x/net go in two calls, got %d\n%s", n, log)
	}
}

func TestGHReleases(t *testing.T) {
	e := watcherEnv(t)
	rel := func(args ...string) wrapRun {
		t.Helper()
		r := wrap(t, e, "", "gh-releases", append([]string{"--package", "p"}, args...)...)
		if r.exit != 0 {
			t.Fatalf("gh-releases %v: exit %d\n%s", args, r.exit, r.err)
		}
		return r
	}
	// stops at the installed version; order is newest first
	if r := rel("--repo", "cli/cli", "--version", "v2.103.3"); r.out["source"] != "releases" || !slices.Equal(versions(r.list("releases")), []string{"2.105.0", "2.104.0"}) {
		t.Errorf("cli/cli: %v", r.out)
	}
	// a module prefix: poly-kit style tags
	const kit = "0.5.0-beta.12"
	for name, c := range map[string]struct {
		version, prefix, pre string
		want                 []string
	}{
		"match, installed is a prerelease":    {kit, "kit/", "match", []string{"0.5.0-beta.14", "0.5.0-beta.13"}},
		"match, installed is stable":          {"0.4.0", "kit/", "match", nil},
		"exclude never reports prereleases":   {kit, "kit/", "exclude", nil},
		"include reports them":                {"0.4.0", "kit/", "include", []string{"0.5.0-beta.14", "0.5.0-beta.13", "0.5.0-beta.12"}},
		"no prefix skips other modules' tags": {"0.4.0", "", "include", nil},
		"another prefix matches nothing":      {"0.4.0", "kit-py/", "include", nil},
	} {
		r := rel("--repo", "hop-top/poly-kit", "--version", c.version, "--tag-prefix", c.prefix, "--prerelease", c.pre)
		if got := versions(r.list("releases")); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	r := rel("--repo", "hop-top/poly-kit", "--version", kit, "--tag-prefix", "kit/")
	first := r.list("releases")[0]
	if first["tag"] != "kit/v0.5.0-beta.14" || first["prerelease"] != true || first["notes"] == "" || first["url"] == "" {
		t.Errorf("release record: %v", first)
	}
	// no releases at all: tags with commit dates, no notes
	if r := rel("--repo", "golang/net", "--version", "0.62.0"); r.out["source"] != "tags" || !slices.Equal(versions(r.list("releases")), []string{"0.63.0"}) {
		t.Errorf("tags fallback: %v", r.out)
	}
	// since is a point in time; lookback applies when there is none
	if r := rel("--repo", "cli/cli", "--version", "1.0.0", "--since", "2999-01-01T00:00:00Z"); len(r.list("releases")) != 0 {
		t.Errorf("nothing is newer than 2999: %v", r.out)
	}
	e.extra = append(e.extra, "FLO_WATCH_NOW="+time.Now().UTC().AddDate(5, 0, 0).Format(time.RFC3339))
	if r := rel("--repo", "cli/cli", "--version", "1.0.0", "--lookback", "14d"); len(r.list("releases")) != 0 {
		t.Errorf("the releases are older than the lookback: %v", r.out)
	}
	// a repository that does not exist has no releases; it is not a failure
	if r := rel("--repo", "example-org/gone", "--version", "1.0.0"); r.out["source"] != "none" {
		t.Errorf("missing repo: %v", r.out)
	}
}
