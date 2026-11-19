module hop.top/flo

go 1.27.4

require (
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1
	github.com/spf13/cobra v1.10.2
	github.com/spf13/viper v1.21.0
	gopkg.in/yaml.v3 v3.0.1
	hop.top/flo/spec v0.0.0
	hop.top/kit v0.5.0-beta.14
	hop.top/vstar v1.0.0-alpha.1
	hop.top/xrr v0.1.0-alpha.5
)

require (
	cel.dev/cel-go v0.33.2 // indirect
	cel.dev/expr v0.25.3 // indirect
	charm.land/lipgloss/v2 v2.0.6 // indirect
	charm.land/log/v2 v2.0.1 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/araddon/dateparse v0.0.0-20210429162001-6b43995a97de // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.26.0 // indirect
	github.com/buger/jsonparser v1.6.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20261008173134-6b8d4baf91b4 // indirect
	github.com/charmbracelet/x/ansi v0.11.9 // indirect
	github.com/charmbracelet/x/exp/charmtone v0.1.0 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/failsafe-go/failsafe-go v0.9.8 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-logfmt/logfmt v0.6.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gofrs/uuid/v5 v5.5.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hop-top/fang/v2 v2.0.2-hop.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.31 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/mango v0.2.0 // indirect
	github.com/muesli/mango-cobra v1.3.0 // indirect
	github.com/muesli/mango-pflag v0.2.0 // indirect
	github.com/muesli/roff v0.1.0 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/pb33f/go-yaml v0.1.1 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.2 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/sagikazarmark/locafero v0.12.0 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/tj/go-naturaldate v1.3.0 // indirect
	github.com/xo/terminfo v1.2.0 // indirect
	go.jetify.com/typeid v1.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261026165753-2ffab32bce7e // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261019180522-ce908d29299b // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	hop.top/aim v0.1.0-alpha.6 // indirect
	hop.top/cite v0.1.1 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.61.0 // indirect
)

replace hop.top/flo/spec => ./spec

// kit pinned to a poly-kit next commit (beta.14 plus the llm transport,
// local-only, structured output, prompt, judge, judge/llmjudge, judge/jev,
// judge/eval, grounding, strategy and llmxrr packages, and the project-layer
// config rules, path keys, origins and deep map merge). The commit is not in
// hop-top/kit, so it is fetched from the poly-kit repository, where it is
// reachable through its next branch.
replace hop.top/kit => github.com/hop-top/poly-kit v0.0.0-20261119074638-ba080b165aa7
