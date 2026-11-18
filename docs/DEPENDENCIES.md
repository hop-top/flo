# Dependency vetting

One entry per dependency added or bumped across a major version, recorded in
the change that edits the manifest. Entries state facts about the pinned
version.

## cel.dev/cel-go v0.33.2

Module `hop.top/flo/spec`, package `hop.top/flo/spec/cel` only (the root
`spec` package does not import it). Exact pin.

- Vetted 2026-11-18. Released 2026-10-30, 19 days before vetting (clears the
  14-day age gate).
- Newer releases v0.33.3 and v0.33.4 (2026-11-17) are not taken: under the age
  gate, and neither carries a security fix.
- Import path `cel.dev/cel-go`. The repository is `cel-expr/cel-go`, formerly
  `google/cel-go`; the `github.com/google/cel-go` path stops at v0.32.0.
- License Apache-2.0, compatible.
- Repository health: 7.0/10, policy result balanced (maintenance 10, license
  10, security 5.3, adoption 1.1); 3158 stars, 100 contributors, last commit
  2026-11-18.
- Advisory GHSA-gcjh-h69q-9w9g / GO-2026-6094 (`ext.NativeTypes` honouring
  `json:"-"`): fixed in v0.30.0; v0.33.2 is unaffected, and `ext.NativeTypes`
  is not used.
- `go` directive 1.23.0.
- Added modules: `cel.dev/expr`, `github.com/antlr4-go/antlr/v4` v4.13.1,
  `go.yaml.in/yaml/v3`, `golang.org/x/exp`, `golang.org/x/text`,
  `google.golang.org/genproto/googleapis/api`,
  `google.golang.org/genproto/googleapis/rpc`, `google.golang.org/protobuf`.
- Binary cost about +9.0 MB stripped (2.2 MB to 12.3 MB for a probe program).
- Used: `cel`, `ext.Lists()` (`flatten`, `sort`, ...), `ext.Strings()`
  (`join`, `split`, ...).

## hop.top/vstar v1.0.0-alpha.1

Module `hop.top/flo/spec` (package `hop.top/flo/spec/run`) and the root
module. Exact pin. Owned dependency (hop.top): exempt from the age gate.

- Used: `rrule.ValidateRRule`, which parses an RFC 5545 recurrence (V26,
  `schedule.rrule`). Nothing else of vstar is imported by the spec module.
- License MIT, compatible. `go` directive 1.25.0.
- Added module to the spec module: `golang.org/x/text` raised to v0.36.0
  (indirect, required by vstar's `go.mod`).
- Root module, calendar pack (`internal/packs/calendar`): same pin, no new
  module. Used: `vstar` (`Calendar`, `Component`, `ParseTime`,
  `ParseTimeWithTZID`), `codec/rfc5545` (`Parse`), `duration` (`Parse`,
  `AddTo`), `rrule` (`ParseRRule`, `Set.Between`). Occurrences expand from a
  DTSTART in its IANA zone, so a series keeps its wall-clock time across DST;
  a TZID the system does not know falls back to the calendar's VTIMEZONE (a
  fixed offset). `rrule.SetFromComponent` is not used: it accepts UTC
  DTSTART/EXDATE/RDATE only.

## hop.top/xrr v0.1.0-alpha.5

Root module only; packages `hop.top/xrr` and `hop.top/xrr/adapters/{exec,http}`
(`internal/flowtest`). Exact pin. Owned dependency (hop.top): exempt from the
age gate.

- Vetted 2026-11-18. Released 2026-08-30 (80 days before vetting).
- Repository: read-only mirror `hop-top/xrr` of `hop-top/poly-xrr` (Go
  component `xrr`). `rsx analyze`: PASS, 5.4/10, policy balanced (activity 1.0,
  maintenance 5.0, security 5.6, docs 8.5, release hygiene 10.0, license
  10.0).
- License MIT, compatible. `go` directive 1.26.1.
- Advisories: OSV has none for `hop.top/xrr` 0.1.0-alpha.5.
- Added modules: none; its requirements (`gopkg.in/yaml.v3`,
  `google.golang.org/protobuf`, `golang.org/x/net`) were already in the build
  list. The gRPC, Redis and SQL adapters are not imported.
- Used: file cassettes (`NewSession`, `NewFileCassette`, default redaction on)
  at the exec and HTTP seams of the engine, to record and replay flow tests.
  Cassettes are written only by `flo flow test --record`.
- `gopkg.in/yaml.v3` v3.0.1 was already in the build list; `internal/flowtest`
  imports it directly to read the `expect:` documents of a bundle's `tests/`.
