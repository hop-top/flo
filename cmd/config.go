package cmd

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	kitcli "hop.top/kit/go/console/cli"
	kitconfig "hop.top/kit/go/core/config"

	"hop.top/flo/internal/core"
)

// The configuration of flo is kit's: a viper on the root, filled once per
// process before a command runs, in the order every kit CLI uses.
//
//	default < system file < user file < project file < -c files
//	  < FLO_* environment < -c key=value < flag
//
// The files are merged by kit (config.Load): /etc/flo/config.yaml,
// $XDG_CONFIG_HOME/flo/config.yaml, the project config, then each -c <path> in
// argument order, later over earlier. A system, user or project file that does
// not exist is skipped; a -c file that does not exist is an error. The project
// config is the nearest .flo/config.yaml (or .flo.yaml) from the working
// directory up, below $HOME (a config in $HOME is read when it is the working
// directory); it may not set a secret or authority key and only narrows the
// model policy lists, see projectconfig.go. The files sit in the config layer
// of the viper, so the environment and -c key=value beat them. The
// environment name of a key is FLO_ plus the key upper-cased, dots as
// underscores (packs.ctxt.url is FLO_PACKS_CTXT_URL). FLO_CONFIG is the
// environment form of -c <path>.
//
// Secrets are not settings: nothing reads one from a file or the environment
// of this loader; they stay in the secret store.
const (
	// EnvPrefix is the prefix of the environment name of every key.
	EnvPrefix = "FLO"
	// configTool names the user and system config directory of flo.
	configTool = "flo"
	// packsKey is the config section of the packs: packs.<pack>.<key>.
	packsKey = "packs"
	// packsEnvPrefix starts the variables of the packs section.
	packsEnvPrefix = EnvPrefix + "_PACKS_"
)

// envBinds are the keys whose environment variable is not the derived one.
var envBinds = []kitconfig.EnvBind{
	{Key: "engine.approval_poll", Env: EnvApprovalPoll},
}

// configLoader loads the files and overrides into the viper of a root and
// remembers what it read, for `flo status`.
type configLoader struct {
	mu    sync.Mutex
	files []string
	// projectFile is the project config read (empty when none); flowsFrom is
	// where flows.dir came from.
	projectFile string
	flowsFrom   string
}

// loaders maps a root to its loader, for the readers of the config that
// report where a value came from.
var loaders sync.Map

func loaderOf(root *kitcli.Root) *configLoader {
	l, _ := loaders.Load(root)
	cl, _ := l.(*configLoader)
	return cl
}

// bindEnv makes the environment a source of every key of root.Viper.
func bindEnv(root *kitcli.Root) {
	if root == nil || root.Viper == nil {
		return
	}
	// BindEnv fails only for a nil viper or an empty bind.
	_ = kitconfig.BindEnv(root.Viper, EnvPrefix, envBinds...)
}

// bindPackEnv binds FLO_PACKS_<PACK>_<KEY> to packs.<pack>.<key> for each such
// variable of the environment. A pack name has no underscore (spec/node: a
// lower-case letter, then letters, digits and hyphens) and its keys are flat,
// so the first underscore after the section ends the pack name; the rest, in
// lower case, is the key. A hyphenated pack name has no environment form.
func bindPackEnv(root *kitcli.Root) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		rest, ok := strings.CutPrefix(name, packsEnvPrefix)
		if !ok {
			continue
		}
		pack, key, ok := strings.Cut(rest, "_")
		if !ok || pack == "" || key == "" {
			continue
		}
		_ = root.Viper.BindEnv(packsKey+"."+strings.ToLower(pack)+"."+strings.ToLower(key), name)
	}
}

// load reads the config files and the -c overrides of root into root.Viper.
func (l *configLoader) load(root *kitcli.Root) error {
	if root == nil || root.Viper == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	paths, overrides, err := root.ConfigArgs()
	if err != nil {
		return core.Errorf(core.CodeUsage, "-c/--config: %v", err).
			WithSuggestion("-c takes a file path or a key=value pair")
	}
	opts := kitconfig.OptionsForToolWithMarkers(configTool, projectMarkers)
	opts.ExtraConfigPaths = paths
	doc, origin, set, err := loadFiles(opts)
	if err != nil {
		return err
	}
	// Merge order of the files, for the source of a key.
	merged := []string{opts.SystemConfigPath, opts.UserConfigPath, opts.ProjectConfigPath}
	merged = append(merged, paths...)
	l.files, l.projectFile = nil, opts.ProjectConfigPath
	for _, f := range merged {
		if _, err := os.Stat(f); err == nil && f != opts.ProjectConfigPath {
			l.files = append(l.files, f)
		}
	}
	cSet := flatten("", overrides)
	_, cFlows := cSet[flowsDirKey]
	l.flowsFrom = flowsSource(origin, set, cFlows)
	// The files are the config layer: the environment and -c key=value, set
	// as overrides, sit above them.
	if err := root.Viper.MergeConfigMap(doc); err != nil {
		return core.Errorf(core.CodeUsage, "config: %v", err)
	}
	// Each leaf is set alone: a nested override ("ai": {...}) would replace
	// the whole subtree.
	for k, v := range cSet {
		root.Viper.Set(k, v)
	}
	bindPackEnv(root)
	return nil
}

// hook is the pre-run hook of the root: it loads the config before any
// command reads a setting.
func (l *configLoader) hook(root func() *kitcli.Root) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error { return l.load(root()) }
}

// registerConfigStatus adds the config sources to `flo status`, next to kit's
// effective-config section (the merged values): which files were read and how
// the environment maps.
func (l *configLoader) registerStatus(root *kitcli.Root) {
	root.RegisterStatusProvider("config", func(context.Context) (kitcli.StatusSection, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		opts := kitconfig.OptionsForTool(configTool)
		dir, src := flowsDir(root)
		return kitcli.StatusSection{
			Title: "config", Status: kitcli.StatusOK, Priority: 450,
			Data: map[string]any{
				"files":          append([]string{}, l.files...),
				"user_file":      opts.UserConfigPath,
				"project_file":   l.projectFile,
				"env_prefix":     EnvPrefix + "_",
				"precedence":     "flag > -c key=value > environment > -c files > project config > user file > system file > default",
				"flows_dir":      dir,
				"flows_dir_from": src,
			},
		}, nil
	})
}

// flatten turns nested maps into dotted leaf keys.
func flatten(prefix string, m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
			for kk, vv := range flatten(key, sub) {
				out[kk] = vv
			}
			continue
		}
		out[key] = v
	}
	return out
}
