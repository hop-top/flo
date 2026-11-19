package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
// access authority with the binary it runs, nor how long finished jobs live
// (it deletes install-wide history); and it only narrows the model
// policy lists, never widens them. The model matcher is that of the policy.
var projectRules = &kitconfig.ProjectRules{
	SecretKey: regexp.MustCompile(`(?i)secret|token|passw|credential|api[_-]?key|private[_-]?key|access[_-]?key`),
	Forbidden: []string{"owner_profile", "engine_profile", "privacy.local_hosts", "bos", "jobs.retention"},
	Narrow: []kitconfig.NarrowKey{
		{Key: keyModelsDeny, Mode: kitconfig.Union},
		{Key: keyModelsAllow, Mode: kitconfig.Intersect, Match: func(pattern, model string) bool {
			return builtin.ModelAllowed([]string{pattern}, model)
		}},
	},
}

// configDoc is the merged config files.
type configDoc = map[string]any

// loadFiles merges the system, user, project and -c files with kit, in that
// order and under the project rules, into one document. Maps merge key by key
// at any depth (a file sets one key under a section without dropping its
// siblings). origin is where the winning flows.dir came from, if any file set
// one.
func loadFiles(opts kitconfig.Options) (doc configDoc, origin kitconfig.KeyOrigin, set bool, err error) {
	opts.Project = projectRules
	opts.DeepMergeMaps = true
	opts.PathKeys = []string{flowsDirKey}
	origins, err := kitconfig.LoadWithOrigins(&doc, opts, flowsDirKey)
	if err != nil {
		var ne *kitconfig.ErrNarrowEmpty
		if errors.As(err, &ne) {
			// Never "no limit": the project would permit no model the user
			// config allows, so the project's list is the thing to fix.
			return nil, origin, false, core.Errorf(core.CodeUsage, "%v", err).
				WithSuggestion("make the project's " + ne.Key + " list name at least one model the user config allows, or drop it")
		}
		return nil, origin, false, core.Errorf(core.CodeUsage, "%v", err).
			WithSuggestion("fix the file")
	}
	origin, set = origins[flowsDirKey]
	return doc, origin, set, nil
}

// flowsSource is where the effective flows.dir came from, highest layer
// first: -c key=value, the environment, then the file kit found it in.
func flowsSource(origin kitconfig.KeyOrigin, set, cOverride bool) string {
	if cOverride {
		return "-c"
	}
	if os.Getenv(EnvPrefix+"_FLOWS_DIR") != "" {
		return "env"
	}
	if !set {
		return "default"
	}
	if origin.Layer == kitconfig.LayerProject {
		return "project " + origin.File
	}
	return "file " + origin.File
}

// flowsDir is the directory flows live in, absolute, and where the setting
// came from (see flowsSource). A file value is already absolute, against the
// file that set it (a project .flo/config.yaml: the project root); env and -c
// key=value are relative to the working directory.
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
