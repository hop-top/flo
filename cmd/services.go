package cmd

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/store/tenants"
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
)

// setting reads one engine setting: the environment variable, else the config
// key.
func setting(root *kitcli.Root, env, key string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	if root != nil && root.Viper != nil {
		return strings.TrimSpace(root.Viper.GetString(key))
	}
	return ""
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
	if root != nil && root.Viper != nil {
		if p := strings.TrimSpace(root.Viper.GetString(ConfigOwnerProfile)); p != "" {
			return p
		}
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
	cfg.Generator = setting(root, builtin.EnvGenerator, "ai.generator")
	cfg.ExprConfig = exprConfig(root)
	return cfg, nil
}

// exprConfig is the `config` namespace of expressions: config.<pack>.* from
// the `packs` section of the config, and config.eva.url.
func exprConfig(root *kitcli.Root) map[string]any {
	out := map[string]any{}
	if root != nil && root.Viper != nil {
		for name, v := range root.Viper.GetStringMap("packs") {
			out[name] = v
		}
	}
	if u := setting(root, EnvEvaURL, "eva.url"); u != "" {
		out["eva"] = map[string]any{"url": u}
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
