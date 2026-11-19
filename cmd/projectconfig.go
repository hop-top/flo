package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	kitcli "hop.top/kit/go/console/cli"
	kitconfig "hop.top/kit/go/core/config"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
)

// Project config: the nearest ancestor directory of the working directory
// that holds one of projectMarkers contributes its file, found and read by kit
// (config.OptionsForToolWithMarkers, config.Options.Project). Its place in the
// order, and the keys it may not set or may only narrow, are kit's; see
// config.go. Only the nearest file is read.
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

// projectRules is what a project config may not do. A project config is
// committed next to the code: it sets no secret, nothing about who owns the
// install or what counts as local (they loosen what the user set), nor the
// access authority with the binary it runs; and it only narrows the model
// policy lists, never widens them. The model matcher is that of the policy.
var projectRules = &kitconfig.ProjectRules{
	SecretKey: regexp.MustCompile(`(?i)secret|token|passw|credential|api[_-]?key|private[_-]?key|access[_-]?key`),
	Forbidden: []string{"owner_profile", "engine_profile", "privacy.local_hosts", "bos"},
	Narrow: []kitconfig.NarrowKey{
		{Key: keyModelsDeny, Mode: kitconfig.Union},
		{Key: keyModelsAllow, Mode: kitconfig.Intersect, Match: func(pattern, model string) bool {
			return builtin.ModelAllowed([]string{pattern}, model)
		}},
	},
}

// configDoc is the merged config files. Maps merge key by key at any depth (a
// file sets one key under a section without dropping its siblings), which a
// plain map would not: yaml replaces a nested map whole.
type configDoc map[string]any

func (d *configDoc) UnmarshalYAML(n *yaml.Node) error {
	var in map[string]any
	if err := n.Decode(&in); err != nil {
		return err //nolint:wrapcheck // reported with the file by kit
	}
	if *d == nil {
		*d = configDoc{}
	}
	mergeMaps(*d, in)
	return nil
}

func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		sm, sok := v.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if sok && dok {
			mergeMaps(dm, sm)
			continue
		}
		dst[k] = v
	}
}

// loadFiles merges the system, user, project and -c files with kit, in that
// order and under the project rules, into one document.
func loadFiles(opts kitconfig.Options) (configDoc, error) {
	var doc configDoc
	opts.Project = projectRules
	if err := refuseUpperKeys(opts.ProjectConfigPath); err != nil {
		return nil, err
	}
	if err := kitconfig.Load(&doc, opts); err != nil {
		var ne *kitconfig.ErrNarrowEmpty
		if errors.As(err, &ne) {
			// Never "no limit": the project would permit no model the user
			// config allows, so the project's list is the thing to fix.
			return nil, core.Errorf(core.CodeUsage, "%v", err).
				WithSuggestion("make the project's " + ne.Key + " list name at least one model the user config allows, or drop it")
		}
		return nil, core.Errorf(core.CodeUsage, "%v", err).
			WithSuggestion("fix the file")
	}
	return doc, nil
}

// refuseUpperKeys refuses a project config with a key that is not lower case.
// Kit matches the rule keys case-sensitively but viper folds keys to lower
// case, so `Owner_Profile:` or `AI: {Models: ...}` would pass the rules and
// still set the setting. Drop this once kit matches the rules case-insensitively.
func refuseUpperKeys(file string) error {
	data, err := os.ReadFile(file) //nolint:gosec // the path kit found
	if err != nil {
		return nil //nolint:nilerr // no file, or unreadable: kit reports it
	}
	var doc any
	if yaml.Unmarshal(data, &doc) != nil {
		return nil // kit reports it
	}
	var walk func(v any) string
	walk = func(v any) string {
		switch m := v.(type) {
		case []any:
			for _, e := range m {
				if k := walk(e); k != "" {
					return k
				}
			}
		case map[string]any:
			for k, e := range m {
				if k != strings.ToLower(k) {
					return k
				}
				if bad := walk(e); bad != "" {
					return bad
				}
			}
		}
		return ""
	}
	if k := walk(doc); k != "" {
		return core.Errorf(core.CodeUsage, "project config %s: key %q must be lower case", file, k).
			WithSuggestion("write the key in lower case")
	}
	return nil
}

// projectRoot is the directory a project config file belongs to: the one that
// holds the marker.
func projectRoot(file string) string {
	for _, m := range projectMarkers {
		if dir, ok := strings.CutSuffix(file, string(filepath.Separator)+m); ok {
			return dir
		}
	}
	return filepath.Dir(file)
}

// resolveFlowsDir makes the flows.dir of the project config absolute against
// the project root, the one path-typed key a project sets: a relative path in
// a file reads relative to that file, not to the directory a command runs in.
func resolveFlowsDir(doc configDoc, root string) {
	flows, _ := doc["flows"].(map[string]any)
	if p, _ := flows["dir"].(string); p != "" && !filepath.IsAbs(p) {
		flows["dir"] = filepath.Join(root, p)
	}
}

// flowsSource is where the effective flows.dir came from, highest layer
// first: -c key=value, the environment, then the files from the last merged to
// the first (kit's order has no per-key source to ask).
func flowsSource(files []string, project string, cOverride bool) string {
	if cOverride {
		return "-c"
	}
	if os.Getenv(EnvPrefix+"_FLOWS_DIR") != "" {
		return "env"
	}
	for i := len(files) - 1; i >= 0; i-- {
		v := viper.New()
		v.SetConfigFile(files[i])
		if v.ReadInConfig() != nil || !v.IsSet(flowsDirKey) {
			continue
		}
		if files[i] == project {
			return "project " + project
		}
		return "file " + files[i]
	}
	return "default"
}

// flowsDir is the directory flows live in, absolute, and where the setting
// came from (see flowsSource). The project value is already absolute; any
// other source is relative to the working directory.
func flowsDir(root *kitcli.Root) (dir, source string) {
	v, source := defaultFlowsDir, "default"
	if root != nil && root.Viper != nil {
		if s := strings.TrimSpace(root.Viper.GetString(flowsDirKey)); s != "" {
			v = s
			source = "config"
			if l := loaderOf(root); l != nil {
				source = l.flowsFrom
			}
		}
	}
	if abs, err := filepath.Abs(v); err == nil {
		return abs, source
	}
	return v, source
}
