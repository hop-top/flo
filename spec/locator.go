package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FlowInfo is a flow's header as seen by List: enough to pick one
// without parsing every file fully.
type FlowInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"` // directory or "builtin"
	Path        string `json:"path"`
}

// FlowNotFoundError reports a reference no layer could resolve.
type FlowNotFoundError struct {
	Ref  string
	Dirs []string
}

func (e *FlowNotFoundError) Error() string {
	if len(e.Dirs) == 0 {
		return fmt.Sprintf("flow %q not found (no flow directories configured)", e.Ref)
	}
	return fmt.Sprintf("flow %q not found in %s", e.Ref, strings.Join(e.Dirs, ", "))
}

// IsFlowNotFound reports whether err wraps a FlowNotFoundError.
func IsFlowNotFound(err error) bool {
	var nf *FlowNotFoundError
	return asFlowNotFound(err, &nf)
}

func asFlowNotFound(err error, target **FlowNotFoundError) bool {
	return errors.As(err, target)
}

// BuiltinSource is the Source of flows served from the embedded set.
const BuiltinSource = "builtin"

// DirLocator resolves flows from an ordered list of directories, then
// from an optional embedded set. An unpinned name takes the first
// directory that has it (highest version there); a pinned name@version is
// searched across every layer in order. A path is opened directly.
type DirLocator struct {
	Dirs    []string
	Builtin fs.FS
}

// Locate resolves ref: a file path, a name, or name@version.
func (l *DirLocator) Locate(ref string) (*Flow, error) {
	if isFlowPath(ref) {
		return loadFlowFile(ref, filepath.Dir(ref))
	}
	name, version, _ := strings.Cut(ref, "@")
	if !flowNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid flow reference %q: want a name, name@version or a file path", ref)
	}
	for _, dir := range l.Dirs {
		if info, ok := pickFlow(scanFlowDir(dir), name, version); ok {
			return loadFlowFile(info.Path, dir)
		}
	}
	if l.Builtin != nil {
		if info, ok := pickFlow(scanFlowFS(l.Builtin), name, version); ok {
			return loadFlowFS(l.Builtin, info.Path)
		}
	}
	dirs := append([]string(nil), l.Dirs...)
	if l.Builtin != nil {
		dirs = append(dirs, BuiltinSource)
	}
	return nil, &FlowNotFoundError{Ref: ref, Dirs: dirs}
}

// List returns every flow header, in layer order, then by name and
// descending version within a layer. Missing directories are skipped.
func (l *DirLocator) List() ([]FlowInfo, error) {
	var out []FlowInfo
	for _, dir := range l.Dirs {
		out = append(out, scanFlowDir(dir)...)
	}
	if l.Builtin != nil {
		out = append(out, scanFlowFS(l.Builtin)...)
	}
	return out, nil
}

// isFlowPath treats anything with a separator, a flow extension, or
// an existing file behind it as a path rather than a name.
func isFlowPath(ref string) bool {
	if strings.ContainsAny(ref, `/\`) {
		return true
	}
	switch strings.ToLower(filepath.Ext(ref)) {
	case extYAML, extYML, extJSON:
		return true
	}
	_, err := os.Stat(ref)
	return err == nil
}

func loadFlowFile(path, source string) (*Flow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open flow: %w", err)
	}
	defer f.Close()
	r, err := ParseFlow(f, path)
	if err != nil {
		return nil, err
	}
	r.Source = source
	return r, nil
}

func loadFlowFS(fsys fs.FS, path string) (*Flow, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("read builtin flow: %w", err)
	}
	r, err := ParseFlow(bytes.NewReader(data), BuiltinSource+":"+path)
	if err != nil {
		return nil, err
	}
	r.Source = BuiltinSource
	return r, nil
}

// pickFlow chooses the highest version of name, or the exact version
// when pinned, from one layer's headers.
func pickFlow(infos []FlowInfo, name, version string) (FlowInfo, bool) {
	var best FlowInfo
	found := false
	for _, info := range infos {
		if info.Name != name {
			continue
		}
		if version != "" {
			if info.Version == version {
				return info, true
			}
			continue
		}
		if !found || CompareVersions(info.Version, best.Version) > 0 {
			best, found = info, true
		}
	}
	return best, found
}

// flowHeader is the lenient header decode used for scanning.
type flowHeader struct {
	Name        string `yaml:"flow"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
}

func scanFlowDir(dir string) []FlowInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var infos []FlowInfo
	for _, e := range entries {
		if e.IsDir() || !isFlowExt(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if info, ok := headerInfo(data, dir, path); ok {
			infos = append(infos, info)
		}
	}
	sortFlowInfos(infos)
	return infos
}

func scanFlowFS(fsys fs.FS) []FlowInfo {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil
	}
	var infos []FlowInfo
	for _, e := range entries {
		if e.IsDir() || !isFlowExt(e.Name()) {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			continue
		}
		if info, ok := headerInfo(data, BuiltinSource, e.Name()); ok {
			infos = append(infos, info)
		}
	}
	sortFlowInfos(infos)
	return infos
}

// Flow file extensions; directory scans only pick up the YAML ones.
const (
	extYAML = ".yaml"
	extYML  = ".yml"
	extJSON = ".json"
)

func isFlowExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == extYAML || ext == extYML
}

func headerInfo(data []byte, source, path string) (FlowInfo, bool) {
	var h flowHeader
	if err := yaml.Unmarshal(data, &h); err != nil || h.Name == "" {
		return FlowInfo{}, false
	}
	return FlowInfo{Name: h.Name, Version: h.Version, Description: h.Description, Source: source, Path: path}, true
}

func sortFlowInfos(infos []FlowInfo) {
	sort.SliceStable(infos, func(i, j int) bool {
		if infos[i].Name != infos[j].Name {
			return infos[i].Name < infos[j].Name
		}
		return CompareVersions(infos[i].Version, infos[j].Version) > 0
	})
}

// CompareVersions orders two versions semver-style without a
// dependency: numeric core parts (missing parts are zero, so 0.1 == 0.1.0),
// a release above any of its pre-releases, and pre-release parts compared
// numerically when both are numbers.
func CompareVersions(a, b string) int {
	coreA, preA, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	coreB, preB, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	if c := compareVersionCore(coreA, coreB); c != 0 {
		return c
	}
	switch {
	case preA == "" && preB == "":
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	}
	return compareVersionParts(strings.Split(preA, "."), strings.Split(preB, "."))
}

func compareVersionCore(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for len(pa) < len(pb) {
		pa = append(pa, "0")
	}
	for len(pb) < len(pa) {
		pb = append(pb, "0")
	}
	return compareVersionParts(pa, pb)
}

func compareVersionParts(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareVersionPart(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

func compareVersionPart(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return cmpInt(na, nb)
	case errA == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
