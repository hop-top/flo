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
equality; any other expression is CEL, accepted unparsed, `until`
negated as `!( ... )`, evaluation returns `ErrNeedsCEL`.

## Gaps against the flow spec docs

- No CEL parser/evaluator: V15 (expression syntax, `unknown_function`) and
  typed `{{ }}` rendering (§4.3) are engine work. Non-trivial placeholders
  are neither parsed nor rendered; render leaves them verbatim.
- `x-*` extension keys (§2.2) not accepted; unknown keys stay errors.
- Not implemented: V06 beyond `state.<key>` refs, V07, V11, V12, V14 for
  `item`/`index`/`result` scoping, V16 for `budget`, V17, V18 to V28.
  Run-level values (`on`, `budget`, ...) are not validated.
- Var `type` is validated by name only; no coercion of `--var` strings (§3).
- Plan-level refusals (`engine_node`, `engine_feature`, §9) not implemented:
  the module parses `uses`/`for_each`; the plan implementer must refuse.
- Error codes (`reserved_key`, ...) appear as message prefixes, not as a
  typed code field.
- Bundle layout and pack/bundle search layers (§2.4): `DirLocator` is the
  recipe 0.1 directory + builtin search path only.
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
