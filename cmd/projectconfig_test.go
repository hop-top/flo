package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

// sandbox is a fake HOME with a global config home and a working directory,
// every path resolved so symlinked temp roots (macOS) compare equal.
type sandbox struct {
	t    *testing.T
	home string
	xdg  string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &sandbox{t: t, home: filepath.Join(base, "home"), xdg: filepath.Join(base, "xdg")}
	for _, d := range []string{s.home, s.xdg} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", s.home)
	t.Setenv("XDG_CONFIG_HOME", s.xdg)
	t.Setenv("USER", "login")
	for _, c := range settingCases {
		t.Setenv(c.env, "")
	}
	t.Setenv("FLO_CONFIG", "")
	t.Setenv("FLO_FLOWS_DIR", "")
	return s
}

func (s *sandbox) write(path, text string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) global(text string) { s.write(filepath.Join(s.xdg, "flo", "config.yaml"), text) }

// project writes .flo/config.yaml under dir (relative to the home).
func (s *sandbox) project(dir, text string) string {
	d := filepath.Join(s.home, dir)
	s.write(filepath.Join(d, ".flo", "config.yaml"), text)
	return d
}

// run loads the config as a command does, with cwd as the working directory.
func (s *sandbox) run(cwd string, args ...string) (*kitcli.Root, error) {
	s.t.Helper()
	dir := filepath.Join(s.home, cwd)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.t.Fatal(err)
	}
	s.t.Chdir(dir)
	root := NewRootWith(func(context.Context) (*core.Services, error) { return &core.Services{}, nil })
	var out bytes.Buffer
	root.Cmd.SetOut(&out)
	root.Cmd.SetErr(&out)
	root.SetArgs(append([]string{"status", "--format", "json"}, args...))
	return root, root.Execute(context.Background())
}

func (s *sandbox) dir(cwd string, args ...string) (string, string) {
	s.t.Helper()
	root, err := s.run(cwd, args...)
	if err != nil {
		s.t.Fatal(err)
	}
	return flowsDir(root)
}

func TestFlowsDirDefault(t *testing.T) {
	s := newSandbox(t)
	got, src := s.dir("work")
	if want := filepath.Join(s.home, "work", "flows"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if src != "default" {
		t.Errorf("source = %q", src)
	}
}

func TestFlowsDirGlobalRelativeToCwd(t *testing.T) {
	s := newSandbox(t)
	s.global("flows:\n  dir: wf\n")
	got, src := s.dir("work")
	if want := filepath.Join(s.home, "work", "wf"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if want := "file " + filepath.Join(s.xdg, "flo", "config.yaml"); src != want {
		t.Errorf("source = %q, want %q", src, want)
	}
	s.global("flows:\n  dir: /abs/flows\n")
	if got, _ := s.dir("work"); got != "/abs/flows" {
		t.Errorf("absolute global dir = %q", got)
	}
}

func TestFlowsDirEnvOverGlobalFile(t *testing.T) {
	s := newSandbox(t)
	s.global("flows:\n  dir: fromfile\n")
	t.Setenv("FLO_FLOWS_DIR", "fromenv")
	got, src := s.dir("work")
	if want := filepath.Join(s.home, "work", "fromenv"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if src != "env" {
		t.Errorf("source = %q", src)
	}
}

func TestFlowsDirProjectOverGlobalFile(t *testing.T) {
	s := newSandbox(t)
	s.global("flows:\n  dir: fromfile\n")
	proj := s.project("proj", "flows:\n  dir: pflows\n")
	got, src := s.dir("proj")
	if want := filepath.Join(proj, "pflows"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if !strings.HasPrefix(src, "project ") || !strings.Contains(src, filepath.Join(proj, ".flo", "config.yaml")) {
		t.Errorf("source = %q", src)
	}
}

// The environment beats the project config (kit order: project < -c files <
// env < -c key=value).
func TestFlowsDirEnvOverProject(t *testing.T) {
	s := newSandbox(t)
	t.Setenv("FLO_FLOWS_DIR", "fromenv")
	s.project("proj", "flows:\n  dir: pflows\n")
	got, src := s.dir("proj/sub")
	if want := filepath.Join(s.home, "proj", "sub", "fromenv"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if src != "env" {
		t.Errorf("source = %q", src)
	}
}

// A -c file beats the project config, and its relative flows.dir is relative
// to the working directory like any non-project file.
func TestFlowsDirCFileOverProject(t *testing.T) {
	s := newSandbox(t)
	s.project("proj", "flows:\n  dir: pflows\n")
	extra := filepath.Join(s.home, "extra.yaml")
	s.write(extra, "flows:\n  dir: xflows\n")
	got, src := s.dir("proj", "-c", extra)
	if want := filepath.Join(s.home, "proj", "xflows"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if src != "file "+extra {
		t.Errorf("source = %q", src)
	}
}

// A config in $HOME is read when the working directory is $HOME itself.
func TestProjectConfigInHomeWhenCwdIsHome(t *testing.T) {
	s := newSandbox(t)
	s.write(filepath.Join(s.home, ".flo", "config.yaml"), "flows:\n  dir: inhome\n")
	got, _ := s.dir("")
	if want := filepath.Join(s.home, "inhome"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
}

// A relative project value resolves against the directory the project config
// belongs to, not the cwd the command runs from.
func TestFlowsDirProjectRelativeToProjectRoot(t *testing.T) {
	s := newSandbox(t)
	proj := s.project("proj", "flows:\n  dir: ./deep/flows\n")
	got, _ := s.dir("proj/a/b")
	if want := filepath.Join(proj, "deep", "flows"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	s.project("proj", "flows:\n  dir: /elsewhere\n")
	if got, _ := s.dir("proj/a"); got != "/elsewhere" {
		t.Errorf("absolute project dir = %q", got)
	}
}

func TestFlowsDirNearestParentWins(t *testing.T) {
	s := newSandbox(t)
	s.project("proj", "flows:\n  dir: far\n")
	near := s.project("proj/sub", "flows:\n  dir: near\n")
	got, _ := s.dir("proj/sub/leaf")
	if want := filepath.Join(near, "near"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	// From a directory under the farther one only, the farther one applies.
	got, _ = s.dir("proj/other")
	if want := filepath.Join(s.home, "proj", "far"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
}

// The nearest project config replaces the farther ones: a key only the
// farther sets is not read.
func TestProjectConfigNearestIsWhole(t *testing.T) {
	s := newSandbox(t)
	s.project("proj", "ai:\n  generator: far\n")
	s.project("proj/sub", "ai:\n  classifier: near\n")
	cfg := mustConfig(t, s, "proj/sub")
	if cfg.Classifier != "near" || cfg.Generator == "far" {
		t.Errorf("classifier = %q generator = %q", cfg.Classifier, cfg.Generator)
	}
}

func mustConfig(t *testing.T, s *sandbox, cwd string, args ...string) serve.Config {
	t.Helper()
	root, err := s.run(cwd, args...)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := engineConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProjectConfigOverridesAnyKey(t *testing.T) {
	s := newSandbox(t)
	s.global("ai:\n  classifier: global\n  generator: g\n")
	s.project("proj", "ai:\n  classifier: proj\n")
	cfg := mustConfig(t, s, "proj/x")
	if cfg.Classifier != "proj" {
		t.Errorf("classifier = %q", cfg.Classifier)
	}
	// The environment beats the project.
	t.Setenv("FLO_AI_CLASSIFIER", "env")
	if cfg = mustConfig(t, s, "proj/x"); cfg.Classifier != "env" {
		t.Errorf("classifier = %q, want env over project", cfg.Classifier)
	}
	t.Setenv("FLO_AI_CLASSIFIER", "")
	if cfg.Generator != "g" {
		t.Errorf("generator = %q, a key the project does not set keeps the global value", cfg.Generator)
	}
	// -c key=value beats the project, and leaves its other keys under the
	// same section alone.
	s.project("proj", "ai:\n  classifier: proj\n  generator: pgen\n")
	cfg = mustConfig(t, s, "proj/x", "-c", "ai.classifier=flag")
	if cfg.Classifier != "flag" || cfg.Generator != "pgen" {
		t.Errorf("classifier = %q generator = %q", cfg.Classifier, cfg.Generator)
	}
}

func TestFlowsDirCOverProject(t *testing.T) {
	s := newSandbox(t)
	s.project("proj", "flows:\n  dir: pflows\n")
	got, src := s.dir("proj", "-c", "flows.dir=cflows")
	if want := filepath.Join(s.home, "proj", "cflows"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if src != "-c" {
		t.Errorf("source = %q", src)
	}
}

// The walk stops below $HOME: a config in $HOME or above is not read, one
// below it is. A project outside $HOME is found up to the filesystem root.
func TestProjectWalkBoundary(t *testing.T) {
	s := newSandbox(t)
	s.write(filepath.Join(s.home, ".flo", "config.yaml"), "flows:\n  dir: inhome\n")
	s.write(filepath.Join(filepath.Dir(s.home), ".flo", "config.yaml"), "flows:\n  dir: abovehome\n")
	got, src := s.dir("work/deep")
	if want := filepath.Join(s.home, "work", "deep", "flows"); got != want || src != "default" {
		t.Errorf("dir = %q (%s), want the default %q: no config at or above $HOME is read", got, src, want)
	}
	s.project("work", "flows:\n  dir: inwork\n")
	if got, _ := s.dir("work/deep"); got != filepath.Join(s.home, "work", "inwork") {
		t.Errorf("dir = %q", got)
	}
}

func TestProjectConfigDotFileForm(t *testing.T) {
	s := newSandbox(t)
	s.write(filepath.Join(s.home, "p", ".flo.yaml"), "flows:\n  dir: dotfile\n")
	got, _ := s.dir("p")
	if want := filepath.Join(s.home, "p", "dotfile"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
}

func TestProjectConfigRefusesSecrets(t *testing.T) {
	for _, text := range []string{
		"packs:\n  ctxt:\n    api_key: x\n",
		"eva:\n  token: x\n",
		"db:\n  password: x\n",
		"client_secret: x\n",
	} {
		s := newSandbox(t)
		s.project("proj", text)
		_, err := s.run("proj")
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: err = %v, want a refusal naming the secret store", text, err)
		}
	}
}

func TestProjectConfigRefusesAuthorityKeys(t *testing.T) {
	for _, text := range []string{
		"privacy:\n  local_hosts: [evil.example]\n",
		"owner_profile: x\n",
		"bos:\n  bin: /tmp/x\n",
	} {
		s := newSandbox(t)
		s.project("proj", text)
		if _, err := s.run("proj"); err == nil || !strings.Contains(err.Error(), "project config") {
			t.Errorf("%q: err = %v", text, err)
		}
	}
}

func TestProjectModelPolicyNarrowsOnly(t *testing.T) {
	cases := []struct {
		name                string
		global, project     string
		allow, deny         string
		envAllow            string
		wantAllow, wantDeny string
	}{
		{
			name:      "deny lists union",
			global:    "ai:\n  models:\n    deny: [\"ollama://a*\"]\n",
			project:   "ai:\n  models:\n    deny: [\"ollama://b*\"]\n",
			wantDeny:  "ollama://a*,ollama://b*",
			wantAllow: "",
		},
		{
			name:      "project deny alone",
			project:   "ai:\n  models:\n    deny: [\"ollama://b*\"]\n",
			wantDeny:  "ollama://b*",
			wantAllow: "",
		},
		{
			name:      "project cannot widen allow",
			global:    "ai:\n  models:\n    allow: [\"ollama://qwen3:4b\"]\n",
			project:   "ai:\n  models:\n    allow: [\"ollama://qwen3:4b\", \"ollama://llama3\"]\n",
			wantAllow: "ollama://qwen3:4b",
		},
		{
			name:      "project allow narrows a glob",
			global:    "ai:\n  models:\n    allow: [\"ollama://qwen*\"]\n",
			project:   "ai:\n  models:\n    allow: [\"ollama://qwen3:4b\", \"ollama://llama3\"]\n",
			wantAllow: "ollama://qwen3:4b",
		},
		{
			name:      "project wildcard keeps the global list",
			global:    "ai:\n  models:\n    allow: [\"ollama://qwen*\"]\n",
			project:   "ai:\n  models:\n    allow: [\"*\"]\n",
			wantAllow: "ollama://qwen*",
		},
		{
			name:      "allow where global has none restricts",
			project:   "ai:\n  models:\n    allow: [\"ollama://qwen3:4b\"]\n",
			wantAllow: "ollama://qwen3:4b",
		},
		{
			// Env is above the project layer: it replaces the list, the
			// project's narrowing does not apply to it.
			name:      "env list wins over project",
			envAllow:  "ollama://a,ollama://b",
			project:   "ai:\n  models:\n    allow: [\"ollama://b\", \"ollama://c\"]\n",
			wantAllow: "ollama://a,ollama://b",
		},
		{
			name:      "project without models keeps global",
			global:    "ai:\n  models:\n    allow: [\"ollama://a\"]\n    deny: [\"ollama://x\"]\n",
			project:   "ai:\n  classifier: rules\n",
			wantAllow: "ollama://a",
			wantDeny:  "ollama://x",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t)
			if c.global != "" {
				s.global(c.global)
			}
			if c.envAllow != "" {
				t.Setenv("FLO_AI_MODELS_ALLOW", c.envAllow)
			}
			s.project("proj", c.project)
			cfg := mustConfig(t, s, "proj")
			if got := strings.Join(cfg.ModelsAllow, ","); got != c.wantAllow {
				t.Errorf("allow = %q, want %q", got, c.wantAllow)
			}
			if got := strings.Join(cfg.ModelsDeny, ","); got != c.wantDeny {
				t.Errorf("deny = %q, want %q", got, c.wantDeny)
			}
		})
	}
}

// -c key=value is the user's own word on the command line: it is not narrowed.
func TestModelPolicyCOverridesProject(t *testing.T) {
	s := newSandbox(t)
	s.project("proj", "ai:\n  models:\n    allow: [\"ollama://a\"]\n")
	cfg := mustConfig(t, s, "proj", "-c", "ai.models.allow=ollama://z")
	if got := strings.Join(cfg.ModelsAllow, ","); got != "ollama://z" {
		t.Errorf("allow = %q", got)
	}
}

// A project allow list that leaves no globally allowed model is a usage error
// naming the file and key: never "no limit", never a silent deny-all.
func TestProjectModelAllowDisjointIsUsageError(t *testing.T) {
	s := newSandbox(t)
	s.global("ai:\n  models:\n    allow: [\"ollama://a\"]\n")
	proj := s.project("proj", "ai:\n  models:\n    allow: [\"ollama://b\"]\n")
	_, err := s.run("proj")
	if !core.IsCode(err, core.CodeUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	for _, w := range []string{filepath.Join(proj, ".flo", "config.yaml"), "ai.models.allow", "ollama://a", "ollama://b"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err %q lacks %q", err, w)
		}
	}
}

// Kit's order puts -c files below the environment.
func TestEnvOverCFile(t *testing.T) {
	s := newSandbox(t)
	extra := filepath.Join(s.home, "extra.yaml")
	s.write(extra, "ai:\n  generator: fromfile\n")
	t.Setenv("FLO_AI_GENERATOR", "fromenv")
	if cfg := mustConfig(t, s, "work", "-c", extra); cfg.Generator != "fromenv" {
		t.Errorf("generator = %q, want env over a -c file", cfg.Generator)
	}
}

// Keys are matched case-sensitively by kit but folded to lower case by the
// config: a project may not spell a forbidden or narrowed key in another case.
func TestProjectConfigRefusesUpperCaseKeys(t *testing.T) {
	for _, text := range []string{
		"Owner_Profile: evil\n",
		"BOS:\n  bin: /tmp/x\n",
		"ai:\n  MODELS:\n    allow: [\"*\"]\n",
		"1:\n  BOS:\n    bin: x\n",
		"true:\n  BOS:\n    bin: x\n",
		"? [a]\n: 1\n",
		"x:\n  1:\n    BOS: {bin: x}\n",
		"\"owner_profile.x\": evil\n",
		"ai.models.allow: [\"*\"]\n",
	} {
		s := newSandbox(t)
		s.project("proj", text)
		if _, err := s.run("proj"); !core.IsCode(err, core.CodeUsage) {
			t.Errorf("%q: err = %v, want a usage error", text, err)
		}
	}
}
