package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/viper"
	kitcli "hop.top/kit/go/console/cli"
	kitconfig "hop.top/kit/go/core/config"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
)

// Project config: the nearest ancestor directory of the working directory
// that holds one of projectMarkers contributes its file to the config, over
// the global files and the environment and under -c key=value and flags. Only
// the nearest file is read; a farther one is not merged. The walk is kit's
// (config.PathsForToolWithMarkers): it starts at the working directory, goes
// up, and stops at the filesystem root or, below it, at $HOME: a config in
// $HOME or above it is never read.
var projectMarkers = []string{
	filepath.Join(".flo", "config.yaml"),
	".flo.yaml",
}

const (
	// flowsDirKey is the setting of the directory flows live in.
	flowsDirKey = "flows.dir"
	// defaultFlowsDir is the directory when no source sets one.
	defaultFlowsDir = "flows"

	keyModelsAllow = "ai.models.allow"
	keyModelsDeny  = "ai.models.deny"
)

// projectSecretKey matches a key segment that names a secret. A project config
// is committed next to the code; secrets stay in the secret store.
var projectSecretKey = regexp.MustCompile(`(?i)secret|token|passw|credential|api[_-]?key|private[_-]?key|access[_-]?key`)

// projectForbidden are the keys, by prefix, a project config may not set:
// who owns the install and what counts as local (they loosen what the user
// set), and the access authority with the binary it runs.
var projectForbidden = []string{"owner_profile", "engine_profile", "privacy.local_hosts", "bos"}

// findProjectConfig returns the nearest project config file for cwd and the
// directory it belongs to (the one holding the marker); empty when none.
func findProjectConfig(cwd string) (file, root string) {
	for _, p := range kitconfig.PathsForToolWithMarkers(cwd, configTool, projectMarkers) {
		if (p.Source != "cwd" && p.Source != "project") || !p.Exists {
			continue
		}
		for _, m := range projectMarkers {
			if dir, ok := strings.CutSuffix(p.Path, string(filepath.Separator)+m); ok {
				return p.Path, dir
			}
		}
	}
	return "", ""
}

// loadProject reads the nearest project config over the loaded files and
// environment, as overrides: a key it sets wins over the global file and the
// FLO_ variable. The model policy lists only narrow what is set. It returns
// the keys it set.
func (l *configLoader) loadProject(root *kitcli.Root) error {
	l.projectFile, l.projectRoot = "", ""
	l.projectKeys = map[string]bool{}
	cwd, err := os.Getwd()
	if err != nil {
		return nil //nolint:nilerr // no working directory, no project
	}
	file, dir := findProjectConfig(cwd)
	if file == "" {
		return nil
	}
	pv := viper.New()
	pv.SetConfigFile(file)
	if err := pv.ReadInConfig(); err != nil {
		return core.Errorf(core.CodeUsage, "project config %s: %v", file, err).
			WithSuggestion("fix the file")
	}
	keys := pv.AllKeys()
	slices.Sort(keys)
	for _, k := range keys {
		if err := checkProjectKey(file, k); err != nil {
			return err
		}
	}
	for _, k := range keys {
		v := pv.Get(k)
		switch k {
		case flowsDirKey:
			v = resolveFrom(dir, pv.GetString(k))
		case keyModelsAllow:
			v = narrowAllow(root.Viper.GetStringSlice(k), splitList(pv.GetStringSlice(k)))
		case keyModelsDeny:
			v = unionList(root.Viper.GetStringSlice(k), splitList(pv.GetStringSlice(k)))
		}
		root.Viper.Set(k, v)
		l.projectKeys[k] = true
	}
	// A disjoint allow narrows to nothing, which an empty list would read as
	// no limit: deny every model instead.
	if l.projectKeys[keyModelsAllow] {
		if a, _ := root.Viper.Get(keyModelsAllow).([]string); len(a) == 0 {
			root.Viper.Set(keyModelsDeny, unionList(root.Viper.GetStringSlice(keyModelsDeny), []string{"*"}))
			root.Viper.Set(keyModelsAllow, []string{})
		}
	}
	l.projectFile, l.projectRoot = file, dir
	return nil
}

// checkProjectKey refuses a key a project config may not hold.
func checkProjectKey(file, key string) error {
	for _, seg := range strings.Split(key, ".") {
		if projectSecretKey.MatchString(seg) {
			return core.Errorf(core.CodeUsage, "project config %s: %s looks like a secret", file, key).
				WithSuggestion("keep secrets in the secret store, not in a config file")
		}
	}
	for _, f := range projectForbidden {
		if key == f || strings.HasPrefix(key, f+".") {
			return core.Errorf(core.CodeUsage, "project config %s: %s may only be set in the user config, the environment or -c", file, key).
				WithSuggestion("move it to $XDG_CONFIG_HOME/flo/config.yaml")
		}
	}
	return nil
}

// resolveFrom is p made absolute against dir; an absolute p is cleaned.
func resolveFrom(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

func splitList(in []string) []string {
	var out []string
	for _, r := range in {
		out = append(out, strings.FieldsFunc(r, func(c rune) bool { return c == ',' || c == ' ' || c == '\t' })...)
	}
	return out
}

// unionList is a then b, without repeats: a deny list only grows.
func unionList(a, b []string) []string {
	out := slices.Clone(splitList(a))
	for _, e := range b {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// narrowAllow is the allow list a project's list leaves under the global one:
// no global list means no limit, so the project's list stands; else an entry
// of either list survives when the other list allows it (a project entry the
// global list allows, a global entry the project list allows), so the result
// permits no model the global list did not. The project's list can narrow
// the global one, never widen it. Disjoint lists give an empty result.
func narrowAllow(global, project []string) []string {
	global = splitList(global)
	if len(global) == 0 {
		return project
	}
	out := []string{}
	for _, p := range project {
		if builtin.ModelAllowed(global, p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, g := range global {
		if builtin.ModelAllowed(project, g) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// flowsDir is the directory flows live in, absolute, and where the setting
// came from: "project <file>", "-c", "env", "file" or "default". The project
// value is already absolute (relative to its config's directory); any other
// source is relative to the working directory.
func flowsDir(root *kitcli.Root) (dir, source string) {
	v, source := defaultFlowsDir, "default"
	if root != nil && root.Viper != nil {
		if s := strings.TrimSpace(root.Viper.GetString(flowsDirKey)); s != "" {
			v = s
			source = flowsDirSource(root)
		}
	}
	if abs, err := filepath.Abs(v); err == nil {
		return abs, source
	}
	return v, source
}

func flowsDirSource(root *kitcli.Root) string {
	l := loaderOf(root)
	switch {
	case l != nil && l.cOverride[flowsDirKey]:
		return "-c"
	case l != nil && l.projectKeys[flowsDirKey]:
		return "project " + l.projectFile
	case os.Getenv(EnvPrefix+"_FLOWS_DIR") != "":
		return "env"
	case root.Viper.InConfig(flowsDirKey):
		return "file"
	}
	return "config"
}
