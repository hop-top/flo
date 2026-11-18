# Recipe code extraction map

Measured against tlc `main`, packages `internal/core`, `internal/cli`,
`internal/storage`, `internal/recipes`, `internal/flowtest`. Paths below are
repo-relative to tlc.

## Finding

The recipe engine is already pure at the import level. No recipe file in
`internal/core` imports `internal/storage`, `internal/cli`, `internal/config`
or any other tlc package. The only non-stdlib imports are `gopkg.in/yaml.v3`,
`hop.top/kit/go/core/util` (materialize only), `hop.top/vstar/rrule` (task
reminders only, not recipes). The tangle is **intra-package**: everything lives
in one Go package, `core`, next to `Task`, `Repository`, the executor. The
cut is a symbol cut, not an import cut.

## Pure (movable as-is)

| Files (`internal/core`) | Lines | Role |
|---|---|---|
| `recipe.go` | 261 | document types, grammar regexes, namespaces |
| `recipe_parser.go` | 125 | YAML/JSON decode, unknown keys, hash |
| `recipe_validate.go` | 478 | header, vars, step shape, graph, refs |
| `recipe_expand.go` | 230 | include + repeat expansion, `Locator` iface |
| `recipe_template.go` | 247 | `{{ }}` scan, `Scope`, render, step ref rewrite |
| `recipe_render.go` | 190 | render order, due arithmetic |
| `recipe_select.go` | 174 | `--task` selector, dep closure |
| `recipe_vars.go` | 65 | `--var` parse, var binding |
| `condition.go` | 229 | `when`/`until` grammar (single comparison; no CEL, no expr dep) |
| `recipe_locator.go` | 308 | `DirLocator`, search path, version ordering |
| tests: `recipe_{parser,expand,select,vars,locator}_test.go`, `condition_test.go` | ~950 | |

Subtotal about 2,300 lines of source. All use stdlib + yaml.v3 only.

## Leaf symbols the pure files borrow from the rest of `core`

These are the only edges; each is small and owned by recipes in spirit.

| Symbol | Defined in | Used by | Cut |
|---|---|---|---|
| `TaskKind` (+`Valid`), `TaskKind{Agent,Exec,Human}` | `task_spec.go` | `RecipeStep.Kind`, validate | move type; tlc aliases |
| `ExecSpec`, `HumanSpec`, `RetrySpec`, `StepGate`, `HumanTimeoutAction` | `task_spec.go` | `RecipeStep`, validate | move; `TaskSpec` (embeds them, persisted in `Task.Spec`) stays and aliases |
| `ParseRecipeDuration` | `executor_human.go` | validate, render | move; executor re-imports |
| `BuiltinSource` | `recipe_locator.go` | locator | moves with it |

`Task.EffectiveKind` (method on `Task`, in `task_spec.go`) stays in tlc.

## Coupled (stays in tlc)

| Files | Lines | Why coupled |
|---|---|---|
| `recipe_materialize.go` | 341 | `Repository`, `IDGenerator`, `Task`, `RecipeRunStore`; creates tasks |
| `recipe_reconcile.go` | 178 | ledger reads via `RecipeRunStore` |
| `recipe_run.go` | 79 | `RecipeRun` ledger types + `RecipeRunStore` iface; also holds `TaskClaimer` |
| `recipe_capture.go`, `recipe_capture_vars.go` | 507 | `Repository`, `Track`, `Task`: track to recipe |
| `recipe_importer.go`, `recipe_importer_llm.go` | 414 | LLM + HTTP markdown import; no store, but not spec: tooling around parse |
| `executor*.go`, `exec_runner*.go`, `exec_dispatcher.go`, `executor_ready.go` | ~1,000 | dispatch engine; runs `Task.Spec`; calls `EvalConditionEnv` (`executor_ready.go:161`), `ParseRecipeDuration` |
| `task_provenance.go` | 62 | `Task.Meta` keys `recipe`, `recipe_run`, `recipe_step` |
| `typeid.go` `NewRecipeRunID` | | run id minting |
| `internal/storage/recipe_run_store.go`, `task_codec.go` | ~500 | SQLite ledger, `TaskSpec` JSON blob |
| `internal/cli/recipe*.go`, `track_create_recipe.go`, `track_execute_recipe.go`, `task_create_recipe.go`, `task_execute_recipe.go` | ~2,800 | commands: locate, bind, render, select, then call `Materializer` |
| `internal/recipes/` | | built-in recipes (embedded YAML) and tests; data, stays |
| `internal/flowtest/` | ~2,900 | `recipe test` harness (sandbox, adapter, cassettes); uses `core.Locator` etc.; not spec |
| `internal/uri/recipe.go` | | uri scheme for recipes |

Callers of pure API outside `core`: `cli/{recipe,recipe_bind,recipe_materialize,
recipe_diff,recipe_import,recipe_runs,track_create_recipe,track_execute_recipe,
track_execute,track_graph,track_show,task_human,record_task,formatter,
config_dirs}.go`, `storage/{recipe_run_store,task_codec}.go`,
`flowtest/resolver.go`, `uri/recipe.go`. All reach it as `core.<Name>`.

## Cut lines

1. **Spec module** = the pure table + the four leaf symbols. No store, no
   engine, no task type.
2. **Plan implementer (tlc)** = materialize, reconcile, ledger, capture, import,
   executor, CLI. Imports the spec module; maps `spec.Step` to `Task`.
3. `Task.Spec` (`TaskSpec`) is the seam type: it embeds `ExecSpec`, `HumanSpec`,
   `RetrySpec`, `StepGate`, persisted as JSON. After the move tlc aliases them
   (`type ExecSpec = spec.ExecSpec`) so the JSON shape and DB rows do not change.

## Not found

- No CEL anywhere in tlc: conditions are the hand-rolled one-comparison grammar.
  The flow spec makes `when`/`until` CEL; that is new code, not extraction.
- No track materialization inside the pure files: tracks enter only through
  `RecipeTrack` (title/type/plan, plain data) and `Materializer`.

## Status: moved into this module

Moved: parse, validate, expand, template, render, select, vars, condition,
locator, and the leaf types (`Kind`, `ExecSpec`, `HumanSpec`, `RetrySpec`,
`Check`, `ParseDuration`), with their tests. Package imports stdlib and
yaml.v3 only.

Renames applied (flow spec §1.2): recipe to flow (`flow:` key, `run.flow`,
`Flow`, `ParseFlow`, `ValidateFlow`, `FlowInfo`, ...), gate to check (`check:`
key, `Check`), `RecipeStep` to `Step`, `TaskKind` to `Kind`.

Added so the worked examples load (flow spec §1.3, §3, §4.2): run-level
keys kept untyped and uninterpreted (`on`, `state`, `run_as`, `privacy`,
`budget`, `tasks`, `concurrency`, `returns`; step `uses`, `for_each`,
`on_item_error`, `concurrency`, `outputs`, `timeout`, `run_as`, `privacy`,
`budget`, `task`, `runner`); var `type`; engine namespaces (`trigger`,
`state`, `item`, `index`, `config`, `in`, `result`, `decisions`) accepted by
validation and left verbatim by render; `run.started_at`;
`results.<step>` without a field; results references as implicit edges
(`ImplicitDeps`); reserved keys `subject`, `stages`, nodes `flo/context@1`,
`flo/propose@1`.

Conditions: legacy single comparison keeps its grammar and loose
equality; any other expression is CEL. The root package accepts it unparsed,
negates `until` as `!( ... )`, and returns `ErrNeedsCEL` on evaluation; it
never links cel-go. Compiling and evaluating CEL is `hop.top/flo/spec/cel`
(below).

### CEL subpackage `hop.top/flo/spec/cel`

Same module, so `spec` stays free of `cel.dev/cel-go` (a test asserts it with
`go list -deps`). `cel.ParseFlow` is `spec.ParseFlow` plus `cel.Validate`.

- Env: lists and strings extensions; `vars`, `subject`, `run`, `steps`,
  `results`, `trigger`, `state`, `config`, `in`, `result`, `decisions` as
  `map<string, dyn>`, `item` dyn, `index` int; flow vars by bare name, typed
  for `string`, `integer`, `boolean`, `list`, `map` (`number`, `duration`
  and non-string untyped defaults are dyn).
- Compile: every `when`/`until` (legacy single comparison first, through
  the existing shim; CEL otherwise) and every `{{ }}` placeholder, split by a
  CEL-aware scanner. Errors carry step id, field path, line:col in the field
  text and a stable code.
- Evaluate: pure `Condition.Eval`, `Template.Render`, `Expr.Eval` over an
  activation map; an exact single placeholder returns the typed value.
- Closed: V15 (syntax, types of declared names, `unknown_function`,
  boolean condition, `for_each` is a list); V06 for CEL refs; V13 for CEL
  `results` refs (unknown step, own results, cycles with CEL edges;
  `Env.ImplicitDeps` lists them); V14 for `result` (outputs and check only)
  and `item`/`index` (under `for_each`, or a `uses` step); trigger `when`/
  `source` against `trigger` only; typed `{{ }}` rendering (§4.3).
- Not closed: with-values against node inputs (needs node specs); fields
  read through map namespaces are dyn, so a wrong field type fails at
  evaluation, not at validation; `trigger.event` payload check; absent
  results (§5.7) are the engine's.
- Step ids with `-` or `/` in CEL: `results.a/b.x` is read as step `a/b`; `-`
  only for ids the flow declares; otherwise write `results['a-b']`. Index
  form is not renamed by include/repeat expansion; inside blocks write the
  dotted form.
- A literal `{{` in a field now starts a placeholder under `cel.Validate`
  (an unclosed one is `unterminated_placeholder`).

### Node, run and flowcheck packages

All in the spec module; none imports kit or the engine. `node` and
`flowcheck` link cel-go (`node` parses expressions to find the namespaces they
read); the root package still does not.

- `spec/node`: strict decode (N01, N02) and `Check` for N03..N15 plus the
  structural rules (kind and namespace, input defs, durations, placement);
  `Load` is `Parse` + `Check`. Packs (`ParsePack`, `CheckPack`, `LoadPack`,
  `Constraint`): manifest grammar, paths inside the pack, `requires.flo`,
  config declared and every `config.<ns>.<key>` a template reads declared.
  Trust providers (`ParseTrust`, `CheckTrust`): command or http, `read`
  required, at most write-shared, placeholders over `in`, `config`, `run`.
  `Locator` (bundle `nodes/` > installed packs > built-ins; pinned `ns/name@N`
  searches every layer, unpinned takes the highest N of the first layer with
  the name; the same type twice in one layer, packs included, is
  `node_conflict`). `Lower`/`Spec.Lowering` implement node.md §14: an exec node
  becomes a plain `exec` step through a caller-supplied `RenderFunc`.
- `spec/run`: typed `Decode` (existing), `Check` for V05, V07, V16, V17, V25
  (privacy), V26 (`vstar/rrule`, `time.LoadLocation`), `ResolveVars` /
  `CoerceVar` for typed `--var` (`invalid_var`).
- `spec/flowcheck`: `Check(flow, Options)` = `run.Check` plus V11, V12, V19,
  V20, V22 (warning), V24, V25 (`run_as`), V27 and the phase-1 refusals
  (`engine_feature`: agent steps, sub-flow steps).

## Gaps against the flow spec docs

- The root package has no CEL parser/evaluator: non-trivial placeholders
  are neither parsed nor rendered there; render leaves them verbatim. V15
  and typed `{{ }}` rendering live in `spec/cel`.
- `x-*` extension keys (§2.2) not accepted in flows; unknown keys stay
  errors. Node specs, packs and trust providers accept `x-*` at the document
  root only.
- Not implemented: V14 for `trigger.event`, V18, V21, V23, V28 (V06 for CEL
  refs, V13 for CEL results edges and V14 `item`/`index`/`result` scoping
  are in `spec/cel`; V05 defaults, V07, V16, V17, V25, V26 in `spec/run`;
  V11, V12, V19, V20, V22, V24, V27 in `spec/flowcheck`).
  `run.Decode` stops at the first bad run-level key; `run.Check` reports it
  once, with the rule that owns the key.
- V20 trust guard: `spec.Step` has no `trust` key, so the check reads the
  provider name from `with.trust` of a node that declares `approval: trust`
  and a trust class. An ancestor `flo/approval@1` is the other guard.
- `out` (node `map`/`fail_when`) is declared dyn; `in` (a CEL keyword) is aliased inside `spec/cel`: `normalizeRefs` rewrites
  a namespace `in` (followed by `.` or `[`, not after an operand) to an
  internal alias, `Eval` binds `act["in"]` to it, refs and messages say `in`.
  `node/refs.go` keeps its own alias for its parse-only scan.
- Error codes: `run.Issue`, `node.Error` and `cel.Error` carry typed codes;
  the root package's own errors still carry them as message prefixes.
- Bundle layout and pack/bundle search layers (§2.4): `node.Locator` covers
  nodes and trust providers; prompts and flows keep `DirLocator`.
- Recipe file compat: the `flow:` key replaces `recipe:`; `recipe:` is
  rejected by name. `gate:` is an unknown key.

## For switching tlc over (shims, not built)

tlc keeps the `recipe` command noun and recipe vocabulary in its UI. Forces:

1. Key compat: existing recipe files (`recipe:`, `gate:`, `{{run.recipe}}`)
   need a loader shim that rewrites keys before `ParseFlow`, or an alias
   option in the module. Built-in `decision-record.yaml` and
   `track-consolidate.yaml` use `recipe:`.
2. Messages: module errors say "flow" and "check"; tlc e2e tests and docs
   assert "recipe"/"gate" wording.
3. Type aliases in `core`: `Recipe = spec.Flow`, `RecipeStep = spec.Step`,
   `TaskKind = spec.Kind`, `StepGate = spec.Check`, `ExecSpec`, `HumanSpec`,
   `RetrySpec` (JSON tags unchanged, so `Task.Spec` rows keep their shape).
   `TaskSpec.Gate` JSON key stays `gate`.
4. `Task.EffectiveKind`, `TaskKind*` constants, `ParseRecipeDuration`,
   `CompareRecipeVersions`, `RecipeNotFoundError` become thin wrappers or
   aliases.
5. Behavior changes tlc inherits: a `results.<id>` reference without
   `depends_on` is now an implicit edge, not an error. The materializer
   builds `blocked_by` from `depends_on` only; it must fold in
   `spec.ImplicitDeps` or tlc runs such steps in parallel.
   `when`/`until` text that is not a single comparison is no longer a
   validation error (it is CEL); tlc's executor evaluates only the legacy
   form and now sees `ErrNeedsCEL` instead of a load-time error.
6. `core` keeps `Materializer`, `Reconcile`, `RecipeRun*`, capture, importer,
   executor; they switch to `spec.Step`/`spec.Flow` and take `Hash`, `Key()`.
7. flowtest and `recipe test` call `core.Locator`/`DirLocator`/`Expand`:
   re-point to `spec`.
8. Module wiring: tlc needs `hop.top/flo/spec` in go.mod (replace or tag);
   the spec module has no tag yet.
