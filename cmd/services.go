package cmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/bos"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/store/tenants"
	"hop.top/flo/spec/node"
)

// ServicesFunc opens the ports a command works against. Commands call it
// when they run, not when the tree is built, so building the tree (help,
// conformance, spec) touches no database.
type ServicesFunc func(ctx context.Context) (*core.Services, error)

// EngineFunc opens the assembled engine: the ports plus the pieces that serve
// them. Only `flo engine serve` and the flow commands need more than the
// ports.
type EngineFunc func(ctx context.Context) (*serve.Engine, error)

// Settings of the engine: environment variable, then the config key of the
// same meaning.
const (
	EnvEngineProfile = "FLO_ENGINE_PROFILE"
	// ConfigEngineProfile is the profile triggers start runs as.
	ConfigEngineProfile = "engine_profile"

	// Worker tuning: how many pollers claim jobs, how often, how long a
	// claim lives without a heartbeat, how long a stopping engine lets steps
	// finish, how often open approvals are polled.
	EnvEnginePollers       = "FLO_ENGINE_POLLERS"
	EnvEngineInterval      = "FLO_ENGINE_INTERVAL"
	EnvEngineClaimTTL      = "FLO_ENGINE_CLAIM_TTL"
	EnvEngineShutdownGrace = "FLO_ENGINE_SHUTDOWN_GRACE"
	EnvApprovalPoll        = "FLO_APPROVAL_POLL"

	// The tlc project and profile the tasks port reaches, and the eva
	// gateway of `check:` (config.eva.url).
	EnvTLCDir     = "FLO_TLC_DIR"
	EnvTLCProfile = "FLO_TLC_PROFILE"
	EnvEvaURL     = "FLO_EVA_URL"

	// EnvLocalHosts lists hosts and CIDRs read as local by the model privacy
	// rule (key privacy.local_hosts; comma-separated in the environment).
	EnvLocalHosts = "FLO_PRIVACY_LOCAL_HOSTS"

	// EnvBuilderModel is the model of the chat builder (key ai.builder.model).
	EnvBuilderModel = "FLO_AI_BUILDER_MODEL"
)

// setting reads one setting of the config of root, which holds every source in
// kit's order (-c override, flag, environment, file, default; see config.go).
// Without a root only the environment variable env is read.
func setting(root *kitcli.Root, env, key string) string {
	if root != nil && root.Viper != nil {
		return strings.TrimSpace(root.Viper.GetString(key))
	}
	return strings.TrimSpace(os.Getenv(env))
}

// listSetting reads a list setting: a file list, or a comma- or
// space-separated environment value.
func listSetting(root *kitcli.Root, env, key string) []string {
	var raw []string
	if root != nil && root.Viper != nil {
		raw = root.Viper.GetStringSlice(key)
	} else if v := os.Getenv(env); v != "" {
		raw = []string{v}
	}
	var out []string
	for _, r := range raw {
		out = append(out, strings.FieldsFunc(r, func(c rune) bool { return c == ',' || c == ' ' || c == '\t' })...)
	}
	return out
}

func durationSetting(root *kitcli.Root, env, key string) (time.Duration, error) {
	s := setting(root, env, key)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, core.Errorf(core.CodeUsage, "%s: %q is not a duration", env, s)
	}
	return d, nil
}

// ownerProfile is the profile that owns the install: owner_profile of the
// config, else the login name. It holds the wildcard grant. It is not the
// caller (FLO_PROFILE names who is acting now).
func ownerProfile(root *kitcli.Root) string {
	if p := setting(root, EnvOwnerProfile, ConfigOwnerProfile); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("USER"))
}

// engineConfig reads the engine's configuration from the environment and the
// config of root.
func engineConfig(root *kitcli.Root) (serve.Config, error) {
	tenant, err := accessTenant(root)
	if err != nil {
		return serve.Config{}, err
	}
	cfg := serve.Config{
		Tenant: tenant,
		Owner:  ownerProfile(root),
		Engine: setting(root, EnvEngineProfile, ConfigEngineProfile),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	cfg.Tasks.Dir = setting(root, EnvTLCDir, "tlc.dir")
	cfg.Tasks.Profile = setting(root, EnvTLCProfile, "tlc.profile")
	if n := setting(root, EnvEnginePollers, "engine.pollers"); n != "" {
		if cfg.Pollers, err = strconv.Atoi(n); err != nil || cfg.Pollers < 1 {
			return serve.Config{}, core.Errorf(core.CodeUsage, "%s: %q is not a positive number", EnvEnginePollers, n)
		}
	}
	for _, d := range []struct {
		dst      *time.Duration
		env, key string
	}{
		{&cfg.Interval, EnvEngineInterval, "engine.interval"},
		{&cfg.ClaimTTL, EnvEngineClaimTTL, "engine.claim_ttl"},
		{&cfg.ShutdownGrace, EnvEngineShutdownGrace, "engine.shutdown_grace"},
		{&cfg.ApprovalPoll, EnvApprovalPoll, "engine.approval_poll"},
	} {
		if *d.dst, err = durationSetting(root, d.env, d.key); err != nil {
			return serve.Config{}, err
		}
	}
	cfg.Classifier = setting(root, builtin.EnvClassifier, "ai.classifier")
	cfg.JudgeEngine = setting(root, builtin.EnvJudgeEngine, "ai.judge.engine")
	cfg.Jev = builtin.KitJev
	cfg.ModelsAllow = listSetting(root, builtin.EnvModelsAllow, "ai.models.allow")
	cfg.ModelsDeny = listSetting(root, builtin.EnvModelsDeny, "ai.models.deny")
	for _, l := range []struct {
		key      string
		list     []string
		validate func([]string) error
	}{
		{"ai.models.allow", cfg.ModelsAllow, builtin.ValidateModelPatterns},
		{"ai.models.deny", cfg.ModelsDeny, builtin.ValidateDenyPatterns},
	} {
		if err := l.validate(l.list); err != nil {
			var ce *core.Error
			if errors.As(err, &ce) {
				return serve.Config{}, core.Errorf(core.CodeUsage, "%s: %s", l.key, ce.Message).WithSuggestion(ce.Suggestion)
			}
			return serve.Config{}, err
		}
	}
	cfg.Generator = setting(root, builtin.EnvGenerator, "ai.generator")
	cfg.BuilderModel = setting(root, EnvBuilderModel, "ai.builder.model")
	cfg.LocalHosts = listSetting(root, EnvLocalHosts, "privacy.local_hosts")
	if _, err := node.NewLocalHosts(cfg.LocalHosts); err != nil {
		return serve.Config{}, core.Errorf(core.CodeUsage, "privacy.local_hosts: %v", err).
			WithSuggestion("list host names, addresses or CIDRs such as 100.64.0.0/10")
	}
	cfg.ExprConfig = exprConfig(root)
	bc, err := bosSettings(root)
	if err != nil {
		return serve.Config{}, err
	}
	if bc.Scope != "" { // a business scope turns bos on
		cfg.Bos = &bc
	}
	if cfg.Bos != nil {
		if cfg.BosPollEvery, err = durationSetting(root, EnvBosPoll, "bos.poll"); err != nil {
			return serve.Config{}, err
		}
	}
	return cfg, nil
}

// Settings of the bos port: environment variable, then the config key.
const (
	// EnvBosScope is the bos business scope id the tenant is bound to; setting
	// it (or bos.scope) turns bos on as the governance backend.
	EnvBosScope = "FLO_BOS_SCOPE"
	// EnvBosPrincipals maps flo profiles to bos principal ids:
	// "noor=principals_01..,jad=principals_01.." (config: bos.principals, a map).
	EnvBosPrincipals = "FLO_BOS_PRINCIPALS"
	// EnvBosActingFor is the principal a run acts for when its starter has none.
	EnvBosActingFor = "FLO_BOS_ACTING_FOR"
	EnvBosBin       = "FLO_BOS_BIN"
	EnvBosTimeout   = "FLO_BOS_TIMEOUT"
	EnvBosPoll      = "FLO_BOS_POLL"
	// EnvBosMaxStaleness is the age of the applied access set past which flo
	// denies what it provisioned (config: bos.max_staleness; default 24h).
	EnvBosMaxStaleness = "FLO_BOS_MAX_STALENESS"
	// EnvBosNarrowOwner stops the owner profile's wildcard being seeded.
	EnvBosNarrowOwner = "FLO_BOS_NARROW_OWNER"
)

// bosSettings reads the bos port's settings. An empty Scope means no bos: flo
// stands alone. The credential of a profile's bos principal is not a setting:
// the command runs under the profile (aps), whose environment holds it.
func bosSettings(root *kitcli.Root) (bos.Config, error) {
	cfg := bos.Config{
		Bin: setting(root, EnvBosBin, "bos.bin"), Scope: setting(root, EnvBosScope, "bos.scope"),
		ActingFor:   setting(root, EnvBosActingFor, "bos.acting_for"),
		Principals:  map[string]string{},
		NarrowOwner: setting(root, EnvBosNarrowOwner, "bos.narrow_owner") == "true",
	}
	var err error
	if cfg.Timeout, err = durationSetting(root, EnvBosTimeout, "bos.timeout"); err != nil {
		return bos.Config{}, err
	}
	// bos.principals is a map in a file or -c bos.principals.<profile>=<id>, and
	// "profile=id,profile=id" in FLO_BOS_PRINCIPALS, which the config binds to
	// the same key: one source wins as a whole, in the order of the config.
	var raw any
	if root != nil && root.Viper != nil {
		raw = root.Viper.Get("bos.principals")
	} else if v := os.Getenv(EnvBosPrincipals); v != "" {
		raw = v
	}
	switch v := raw.(type) {
	case map[string]any:
		for k, id := range v {
			if s, ok := id.(string); ok {
				cfg.Principals[k] = s
			}
		}
	case map[string]string:
		for k, id := range v {
			cfg.Principals[k] = id
		}
	case string:
		for _, pair := range strings.FieldsFunc(v, func(c rune) bool { return c == ',' || c == ' ' || c == '\t' }) {
			k, id, ok := strings.Cut(pair, "=")
			if !ok || k == "" || id == "" {
				return bos.Config{}, core.Errorf(core.CodeUsage, "%s: %q is not profile=principal", EnvBosPrincipals, pair)
			}
			cfg.Principals[k] = id
		}
	}
	return cfg, nil
}

// exprConfig is the `config` namespace of expressions: config.<pack>.* from
// the `packs` section of the config (file, FLO_PACKS_<PACK>_<KEY>, -c), and
// config.eva.url.
func exprConfig(root *kitcli.Root) map[string]any {
	out := map[string]any{}
	if root != nil && root.Viper != nil {
		// AllSettings, not GetStringMap: only it merges the keys that come from
		// the environment.
		packs, _ := root.Viper.AllSettings()[packsKey].(map[string]any)
		for name, v := range packs {
			out[name] = v
		}
	}
	if u := setting(root, EnvEvaURL, "eva.url"); u != "" {
		out["eva"] = map[string]any{"url": u}
	}
	// config.bos.scope: the business scope the nodes of the bos pack pass to
	// the bos CLI, the one setting bos.scope names.
	if sc := setting(root, EnvBosScope, "bos.scope"); sc != "" {
		ns, _ := out["bos"].(map[string]any)
		if ns == nil {
			ns = map[string]any{}
		}
		ns["scope"] = sc
		out["bos"] = ns
	}
	return out
}

// opener memoizes opening the engine: a command asks for the services
// several times and must get one set of databases.
type opener struct {
	open func(context.Context) (*serve.Engine, error)
	once sync.Once
	eng  *serve.Engine
	err  error
}

func (o *opener) engine(ctx context.Context) (*serve.Engine, error) {
	o.once.Do(func() { o.eng, o.err = o.open(context.WithoutCancel(ctx)) })
	return o.eng, o.err
}

func (o *opener) services(ctx context.Context) (*core.Services, error) {
	e, err := o.engine(ctx)
	if err != nil {
		return nil, err
	}
	return e.Services, nil
}

// engineOf returns the opener of the real engine over the config of root.
func engineOf(root func() *kitcli.Root) *opener {
	return &opener{open: func(ctx context.Context) (*serve.Engine, error) {
		cfg, err := engineConfig(root())
		if err != nil {
			return nil, err
		}
		return serve.Open(ctx, cfg)
	}}
}

// engineAlive reports whether an engine serves the data root: the check
// `run create --wait` makes before it waits for one.
func engineAlive() error {
	dir, err := tenants.DefaultRoot()
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	st, ok, err := serve.ReadState(dir)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if !ok || !st.Alive() {
		return core.New(core.CodeEngineUnreachable, "no engine is serving, so nothing would advance the run").
			WithSuggestion("start one with `flo engine serve`, or run without --wait")
	}
	return nil
}
