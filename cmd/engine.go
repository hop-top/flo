package cmd

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/serve"
	"hop.top/flo/internal/store/tenants"
)

const engineSchemaVersion = "1.0"

// engineStopped is what `flo engine serve` prints when it stops.
type engineStopped struct {
	Status string `json:"status" yaml:"status"`
	Root   string `json:"root" yaml:"root"`
	Tenant string `json:"tenant" yaml:"tenant"`
}

// addEngine registers the `flo engine` noun: serve.
func addEngine(root *kitcli.Root, eng EngineFunc) {
	e := &cobra.Command{Use: "engine", Short: "the engine that runs flows"}
	e.AddCommand(engineServeCmd(root, eng))
	root.Cmd.AddCommand(e)
}

func engineServeCmd(root *kitcli.Root, eng EngineFunc) *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the engine: scheduler, workers and approval polling",
		Long: "Run the engine until it gets SIGINT or SIGTERM: claim run, trigger and approval jobs from " +
			"the queue and run them. On start it queues what a crash left unqueued (a lost run job, a " +
			"schedule's next fire) and releases the claims of workers that stopped. On a signal it stops " +
			"claiming and lets running steps finish, up to FLO_ENGINE_SHUTDOWN_GRACE (30s); what is " +
			"interrupted is picked up by the next engine. One engine serves a data root. Logs go to " +
			"stderr; the data root is $XDG_DATA_HOME/flo.",
		Args: cobra.NoArgs,
	}
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		if eng == nil {
			return core.New(core.CodeEngineUnreachable, "the engine is not wired in this build")
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		e, err := eng(ctx)
		if err != nil {
			return err
		}
		if err := e.Serve(ctx); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		return output.Dispatch(cmd, root.Viper, engineStopped{Status: "stopped", Root: e.Config.Root, Tenant: string(e.Config.Tenant)})
	}
	kitcli.SetSideEffect(c, kitcli.SideEffectWriteLocal)
	kitcli.SetIdempotency(c, kitcli.IdempotencyYes)
	kitcli.SetRetryable(c, true)
	_ = kitcli.SetOutputSchema(c, kitcli.OutputSchema{Type: &engineStopped{}, Version: engineSchemaVersion})
	_ = kitcli.SetExamples(c, []kitcli.Example{
		{Title: "run the engine", Command: "flo engine serve"},
		{Title: "see whether it runs", Command: "flo status"},
	})
	_ = kitcli.SetNextSteps(c, []kitcli.NextStep{
		{When: "the engine runs", Suggest: "flo run create --flow <flow>", Reason: "start a run for it to advance"},
	})
	return c
}

// engineStatus is the `engine` section of `flo status`.
type engineStatus struct {
	State   string `json:"state" yaml:"state"`
	Root    string `json:"root" yaml:"root"`
	PID     int    `json:"pid,omitempty" yaml:"pid,omitempty"`
	Host    string `json:"host,omitempty" yaml:"host,omitempty"`
	Since   string `json:"since,omitempty" yaml:"since,omitempty"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	Tenant  string `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Pollers int    `json:"pollers,omitempty" yaml:"pollers,omitempty"`
}

// registerEngineStatus adds the engine section to `flo status` (12fc F8):
// running, stopped, or a stale file left by a killed engine.
func registerEngineStatus(root *kitcli.Root) {
	root.RegisterStatusProvider("engine", func(context.Context) (kitcli.StatusSection, error) {
		dir, err := tenants.DefaultRoot()
		if err != nil {
			return kitcli.StatusSection{Title: "engine", Status: kitcli.StatusError, ErrorMessage: err.Error(), Priority: 1000}, nil
		}
		data := engineStatus{State: "stopped", Root: dir}
		st, ok, err := serve.ReadState(dir)
		if err != nil {
			return kitcli.StatusSection{Title: "engine", Status: kitcli.StatusError, ErrorMessage: err.Error(), Priority: 1000}, nil
		}
		if !ok {
			return kitcli.StatusSection{Title: "engine", Status: kitcli.StatusEmpty, Data: data, Priority: 1000}, nil
		}
		data.PID, data.Host, data.Version, data.Tenant, data.Pollers = st.PID, st.Host, st.Version, st.Tenant, st.Pollers
		data.Since = st.Since.UTC().Format("2006-01-02T15:04:05Z")
		if !st.Alive() {
			data.State = "stale"
			return kitcli.StatusSection{Title: "engine", Status: kitcli.StatusDegraded, Data: data, ErrorMessage: "the engine that wrote the state file is gone", Priority: 1000}, nil
		}
		data.State = "running"
		return kitcli.StatusSection{Title: "engine", Status: kitcli.StatusOK, Data: data, Priority: 1000}, nil
	})
}
