package cmd

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	kitcli "hop.top/kit/go/console/cli"
	kitconfig "hop.top/kit/go/core/config"

	"hop.top/flo/internal/core"
)

// The configuration of flo is kit's: a viper on the root, filled once per
// process before a command runs, in the order the tlc and aps CLIs use.
//
//	-c key=value  >  flag  >  project config  >  FLO_* environment  >  files  >  default
//
// The project config is the nearest .flo/config.yaml (or .flo.yaml) in the
// working directory or an ancestor, below $HOME; see projectconfig.go.
//
// Files merge system, user, then each -c <path> in argument order, later over
// earlier: /etc/flo/config.yaml, $XDG_CONFIG_HOME/flo/config.yaml. A system or
// user file that does not exist is skipped; a -c file that does not exist is
// an error. The environment name of a key is FLO_ plus the key upper-cased,
// dots as underscores (packs.ctxt.url is FLO_PACKS_CTXT_URL). FLO_CONFIG is
// the environment form of -c <path>.
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
	// projectFile is the project config read (empty when none), projectRoot the
	// directory it belongs to, projectKeys the keys it set, cOverride the keys
	// -c key=value set.
	projectFile, projectRoot string
	projectKeys              map[string]bool
	cOverride                map[string]bool
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
	opts := kitconfig.OptionsForTool(configTool)
	l.files = l.files[:0]
	for _, f := range []struct {
		path     string
		explicit bool
	}{{opts.SystemConfigPath, false}, {opts.UserConfigPath, false}} {
		if err := l.merge(root, f.path, f.explicit); err != nil {
			return err
		}
	}
	for _, p := range paths {
		if err := l.merge(root, p, true); err != nil {
			return err
		}
	}
	if err := l.loadProject(root); err != nil {
		return err
	}
	l.cOverride = map[string]bool{}
	// Each leaf is set alone: a nested override ("ai": {...}) would replace
	// the whole subtree, the project's keys under it included.
	for k, v := range flatten("", overrides) {
		root.Viper.Set(k, v)
		l.cOverride[strings.ToLower(k)] = true
	}
	bindPackEnv(root)
	return nil
}

// merge reads one file over what is loaded. A missing file is skipped unless
// explicit (a -c path).
func (l *configLoader) merge(root *kitcli.Root, path string, explicit bool) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return nil
		}
		return core.Errorf(core.CodeUsage, "config file %s: %v", path, err).
			WithSuggestion("check the path given to -c")
	}
	root.Viper.SetConfigFile(path)
	if err := root.Viper.MergeInConfig(); err != nil {
		return core.Errorf(core.CodeUsage, "config file %s: %v", path, err).
			WithSuggestion("fix the file, or leave it out of -c")
	}
	l.files = append(l.files, path)
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
				"precedence":     "-c key=value > flag > project config > environment > files > default",
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
