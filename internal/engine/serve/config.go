// Package serve assembles the flo engine from the pieces below it and runs
// it: tenants and stores, the job queue and worker, the run log, the node
// registry and built-ins, the dispatcher and local runner, the run executor,
// access (grants, admission), approvals, tasks and triggers. The CLI opens
// the same assembly to read and write (D4: the CLI writes the database
// directly); `flo engine serve` additionally runs the worker.
//
// Layout:
//
//	config.go   Config and its defaults
//	engine.go   Open: wiring; Engine, Close
//	serve.go    Serve: start-up pass, pollers, SIGTERM drain
//	nodes.go    node layers: bundle, installed packs, built-ins; the locator
//	packs.go    installed packs on disk
//	flows.go    validate, apply and set-state of flows
//	gate.go     the ActivationGate: validates, and has a passing test of that version
//	testrun.go  `flo flow test`: a flow version run in a sandbox engine
//	state.go    engine state file, read by `flo status` and `run create --wait`
package serve

import (
	"log/slog"
	"path/filepath"
	"time"

	"hop.top/kit/go/runtime/sideeffect"
	"hop.top/kit/go/storage/secret"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/store/tenants"
	"hop.top/flo/internal/tasks/tlccli"
)

// DefaultEngineProfile is the profile triggers start runs as and the one
// that holds the capabilities installed packs declare.
const DefaultEngineProfile = "flo"

// SecretPrefix is the environment prefix of the default secret store: the
// secret `token` of profile `automation` in tenant `default` is
// FLO_SECRET_DEFAULT_AUTOMATION_TOKEN.
const SecretPrefix = "FLO_SECRET_"

// Config is what Open assembles the engine from. The zero value of every
// field but Owner is usable.
type Config struct {
	// Root is the data root (tenants.DefaultRoot when empty).
	Root string
	// Tenant is the tenant the engine serves and seeds; default tenant when
	// empty. Phase 1 serves one tenant.
	Tenant core.TenantID
	// Owner is the owner's profile: it holds the wildcard grant. Empty seeds
	// nothing.
	Owner string
	// Engine is the engine's own profile; DefaultEngineProfile when empty.
	Engine string

	// Pollers, Interval, StaleInterval and ShutdownGrace tune the worker
	// (jobs.WorkerOptions); zero takes its defaults.
	Pollers       int
	Interval      time.Duration
	StaleInterval time.Duration
	ShutdownGrace time.Duration
	// ClaimTTL is how long a claimed job lives without a heartbeat: the time a
	// killed engine's runs wait before another one picks them up. Zero is
	// jobs.DefaultClaimTTL.
	ClaimTTL time.Duration
	// ApprovalPoll is how often open approvals are polled for a closed task
	// and a timeout; zero is approvals.DefaultPollEvery.
	ApprovalPoll time.Duration
	// MaxParallel bounds the attempts in flight in one run; zero is the
	// executor's default.
	MaxParallel int

	// Tasks says how to reach tlc.
	Tasks tlccli.Config
	// ExprConfig is the read-only `config` namespace of expressions
	// (config.<pack>.*, config.eva.url).
	ExprConfig map[string]any
	// Classifier names the flo/ai.classify@1 backend (`ai.classifier`):
	// "rules" is the deterministic one, "judge" is kit's and not yet
	// available. Empty configures none, and a step using the node fails.
	Classifier string
	// Generator names the flo/ai.generate@1 backend (`ai.generator`):
	// "template" is the deterministic one, "llm" calls the step's model
	// through kit's client. Empty configures none, and a step using the node
	// fails.
	Generator string
	// LocalHosts are the hosts and CIDRs treated as local when a model URI
	// is read (`privacy.local_hosts`); empty lists none.
	LocalHosts []string
	// PacksDir holds the installed packs (<Root>/packs when empty).
	PacksDir string
	// TrustDirs hold trust provider documents, besides the trusts/ of every
	// installed pack and <Root>/trusts.
	TrustDirs []string

	// Exec starts every process; HTTP sends every request of declared HTTP
	// nodes and the eva check; Secrets resolves secret references. Nil takes
	// the real implementation (kit sideeffect real; the environment).
	Exec    sideeffect.Exec
	HTTP    sideeffect.HTTP
	Secrets secret.Store
	// Clock is the engine's time; the system clock when nil.
	Clock core.Clock
	// NoSync turns fsync off, for throwaway test databases only.
	NoSync bool
	// Logger receives worker and queue errors; slog.Default when nil.
	Logger *slog.Logger
}

func (c *Config) defaults() error {
	if c.Root == "" {
		r, err := tenants.DefaultRoot()
		if err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		c.Root = r
	}
	if c.Tenant == "" {
		c.Tenant = core.DefaultTenant
	}
	if err := core.RequireTenant(c.Tenant); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if c.Engine == "" {
		c.Engine = DefaultEngineProfile
	}
	if c.PacksDir == "" {
		c.PacksDir = filepath.Join(c.Root, "packs")
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = DefaultShutdownGrace
	} else if c.ShutdownGrace < 0 {
		c.ShutdownGrace = 0 // wait for ever
	}
	if c.Clock == nil {
		c.Clock = core.SystemClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return nil
}
