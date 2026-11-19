package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
)

func nest(key, v string) string {
	parts := strings.Split(key, ".")
	var b strings.Builder
	for i, p := range parts[:len(parts)-1] {
		b.WriteString(strings.Repeat("  ", i) + p + ":\n")
	}
	b.WriteString(strings.Repeat("  ", len(parts)-1) + parts[len(parts)-1] + ": " + v + "\n")
	return b.String()
}

func pack(name, key string) func(c serve.Config) string {
	return func(c serve.Config) string {
		m, _ := c.ExprConfig[name].(map[string]any)
		s, _ := m[key].(string)
		return s
	}
}

// settingCases lists every setting of the CLI: its dotted key, environment
// variable, the value each rung sets, and how the engine config shows it.
var settingCases = []struct {
	key             string
	env             string
	file, envv, flg string
	got             func(c serve.Config) string
}{
	{"tenant", EnvTenant, "tfile", "tenv", "tflag", func(c serve.Config) string { return string(c.Tenant) }},
	{"owner_profile", EnvOwnerProfile, "ofile", "oenv", "oflag", func(c serve.Config) string { return c.Owner }},
	{"engine_profile", EnvEngineProfile, "efile", "eenv", "eflag", func(c serve.Config) string { return c.Engine }},
	{"tlc.dir", EnvTLCDir, "/d/file", "/d/env", "/d/flag", func(c serve.Config) string { return c.Tasks.Dir }},
	{"tlc.profile", EnvTLCProfile, "pfile", "penv", "pflag", func(c serve.Config) string { return c.Tasks.Profile }},
	{"engine.pollers", EnvEnginePollers, "2", "3", "4", func(c serve.Config) string { return itoa(c.Pollers) }},
	{"engine.interval", EnvEngineInterval, "1s", "2s", "3s", func(c serve.Config) string { return c.Interval.String() }},
	{"engine.claim_ttl", EnvEngineClaimTTL, "1m", "2m", "3m", func(c serve.Config) string { return c.ClaimTTL.String() }},
	{"engine.shutdown_grace", EnvEngineShutdownGrace, "1m", "2m", "3m", func(c serve.Config) string { return c.ShutdownGrace.String() }},
	// FLO_APPROVAL_POLL is not the derived name: an explicit bind.
	{"engine.approval_poll", EnvApprovalPoll, "1s", "2s", "3s", func(c serve.Config) string { return c.ApprovalPoll.String() }},
	{"ai.classifier", "FLO_AI_CLASSIFIER", "cfile", "cenv", "cflag", func(c serve.Config) string { return c.Classifier }},
	{"ai.generator", "FLO_AI_GENERATOR", "gfile", "genv", "gflag", func(c serve.Config) string { return c.Generator }},
	{"privacy.local_hosts", EnvLocalHosts, "file.example", "env.example,env2.example", "flag.example", func(c serve.Config) string { return strings.Join(c.LocalHosts, ",") }},
	{"eva.url", EnvEvaURL, "http://file", "http://env", "http://flag", func(c serve.Config) string { return pack("eva", "url")(c) }},
	{"packs.ctxt.url", "FLO_PACKS_CTXT_URL", "http://file", "http://env", "http://flag", pack("ctxt", "url")},
	{"packs.calendar.url", "FLO_PACKS_CALENDAR_URL", "/cal/file.ics", "/cal/env.ics", "/cal/flag.ics", pack("calendar", "url")},
	{"packs.calendar.sync_interval", "FLO_PACKS_CALENDAR_SYNC_INTERVAL", "1m", "2m", "3m", pack("calendar", "sync_interval")},
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// loaded runs `flo status` over a root with the given user config file text,
// environment and extra args, so the loader runs as it does for a command,
// and returns the engine config read from that root.
func loaded(t *testing.T, file string, env map[string]string, args ...string) (serve.Config, error) {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("USER", "login")
	for _, c := range settingCases {
		t.Setenv(c.env, "")
	}
	t.Setenv("FLO_CONFIG", "")
	if file != "" {
		if err := os.MkdirAll(filepath.Join(cfgHome, "flo"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgHome, "flo", "config.yaml"), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	root := NewRootWith(func(context.Context) (*core.Services, error) { return &core.Services{}, nil })
	var out bytes.Buffer
	root.Cmd.SetOut(&out)
	root.Cmd.SetErr(&out)
	root.SetArgs(append([]string{"status", "--format", "json"}, args...))
	if err := root.Execute(context.Background()); err != nil {
		return serve.Config{}, err
	}
	return engineConfig(root)
}

// TestConfigPrecedence: -c key=value beats the environment, which beats the
// file, which beats the default, for every setting and every pack key.
func TestConfigPrecedence(t *testing.T) {
	for _, s := range settingCases {
		t.Run(s.key, func(t *testing.T) {
			fileYAML := nest(s.key, s.file)
			flag := []string{"-c", s.key + "=" + s.flg}
			rungs := []struct {
				name string
				file string
				env  map[string]string
				args []string
				want string
			}{
				{"default", "", nil, nil, ""},
				{"file", fileYAML, nil, nil, s.file},
				{"env over file", fileYAML, map[string]string{s.env: s.envv}, nil, s.envv},
				{"env alone", "", map[string]string{s.env: s.envv}, nil, s.envv},
				{"-c over env and file", fileYAML, map[string]string{s.env: s.envv}, flag, s.flg},
				{"-c alone", "", nil, flag, s.flg},
			}
			for _, r := range rungs {
				cfg, err := loaded(t, r.file, r.env, r.args...)
				if err != nil {
					t.Fatalf("%s: %v", r.name, err)
				}
				got := s.got(cfg)
				want := normalize(s.key, r.want)
				if r.want == "" {
					want = defaultOf(s.key)
				}
				if got != want {
					t.Errorf("%s: %s = %q, want %q", r.name, s.key, got, want)
				}
			}
		})
	}
}

// the value a setting shows after parsing (a duration prints as one, a
// number as one), and what it is with nothing set.
func normalize(key, v string) string {
	switch key {
	case "engine.interval", "engine.claim_ttl", "engine.shutdown_grace", "engine.approval_poll":
		d, err := time.ParseDuration(v)
		if err != nil {
			return v
		}
		return d.String()
	}
	return v
}

func defaultOf(key string) string {
	switch key {
	case "tenant":
		return string(core.DefaultTenant)
	case "owner_profile":
		return "login" // $USER
	case "engine.interval", "engine.claim_ttl", "engine.shutdown_grace", "engine.approval_poll":
		return "0s"
	}
	return ""
}

// -c files merge in order over the user file, and a -c file that is not there
// is an error rather than a silent skip.
func TestConfigFiles(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.yaml")
	two := filepath.Join(dir, "two.yaml")
	if err := os.WriteFile(one, []byte("packs:\n  ctxt:\n    url: http://one\n  calendar:\n    url: /one.ics\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("packs:\n  ctxt:\n    url: http://two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	user := "packs:\n  ctxt:\n    url: http://user\n  heo:\n    bin: heo\n"
	cfg, err := loaded(t, user, nil, "-c", one, "-c", two)
	if err != nil {
		t.Fatal(err)
	}
	if pack("ctxt", "url")(cfg) != "http://two" || pack("calendar", "url")(cfg) != "/one.ics" || pack("heo", "bin")(cfg) != "heo" {
		t.Errorf("merge order user < one < two: %v", cfg.ExprConfig)
	}
	_, err = loaded(t, "", nil, "-c", filepath.Join(dir, "missing.yaml"))
	if err == nil {
		t.Fatal("a missing -c file must fail")
	}
	if got := ExitCode(err); got != core.ExitUsage {
		t.Errorf("exit %d for a missing -c file, want %d: %v", got, core.ExitUsage, err)
	}
	if _, err = loaded(t, "packs: [unclosed\n", nil); err == nil || ExitCode(err) != core.ExitUsage {
		t.Errorf("a malformed user file must fail as usage: %v", err)
	}
}

// FLO_CONFIG is the environment form of -c <path>.
func TestConfigEnvPath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(f, []byte("packs:\n  ctxt:\n    url: http://viaenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loaded(t, "", map[string]string{"FLO_CONFIG": f})
	if err != nil {
		t.Fatal(err)
	}
	if got := pack("ctxt", "url")(cfg); got != "http://viaenv" {
		t.Errorf("FLO_CONFIG: packs.ctxt.url = %q", got)
	}
}

// Secrets never come from the config file or the loader: a secret-looking key
// in the file reaches no setting.
func TestConfigHoldsNoSecrets(t *testing.T) {
	cfg, err := loaded(t, "packs:\n  ctxt:\n    url: http://x\nsecrets:\n  ctxt_token: nope\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(strings.Join(mapKeys(cfg.ExprConfig), ",")), "secret") {
		t.Errorf("config namespace of expressions: %v", cfg.ExprConfig)
	}
}

func mapKeys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
