# Flow Specification (FLO-FLOW) v0.1

- Version: 0.1
- Status: draft
- Companion documents: [node.md](node.md) (node specs), [examples.md](examples.md) (worked flows)

## Summary

A flow is a readable YAML document: a graph of typed steps, started by
triggers, run by an engine that enforces approvals, budgets and access
checks, and recorded as a tree of step records.

The format starts from the recipe specification 0.1 of the tlc task
tool. Everything a recipe can express stays valid in a flow, so a tool that
implements only the *plan* level (parse, validate, expand, render,
materialize, run the core step kinds) can load flows that stay within that
subset. Everything about triggers, nodes, state, budgets and approvals is
*run* level and belongs to the flo engine.

This spec OWNS:

- the flow document grammar and its validation rules
- template and expression namespaces
- the step kinds and the node reference (`uses:`)
- trigger declarations (`on:`)
- run identity, concurrency, state, budget, privacy and task semantics
- the plan-level / run-level split

This spec DOES NOT own:

- node specs, packs, the built-in node catalog (see [node.md](node.md))
- the task schema and statuses of the task tool
- the grant, admission and trust backends (ports; see node.md §10)
- the run log encoding and the CLI surface

Terms. A **flow** is the document. A **run** is one execution of a flow
version. A **step** is one entry of `steps`. A **node** is a typed,
versioned unit of work a step can use (`uses:`). A **check** is an
automated pass or fail on a step's output. An **approval** is a human
decision. These words are not interchangeable: "gate" is retired, and
"contract" is not used for node specs.

---

## 1. Relation to recipe spec 0.1

### 1.1 Kept

| Recipe 0.1 | In a flow |
|---|---|
| Encoding rules: one YAML or JSON document, root a mapping, unknown keys are errors, validation always runs, SHA-256 of the raw bytes travels with every run | same (the hash is the run's `flow_hash`) |
| `version`, `description`, `requires.subject`, `vars` (`description`, `required`, `default`), `agent`, `track.*` | same; `track.*` is read by plan-level implementers and ignored by the engine |
| Step `id`, `depends_on`, `when`, `title`, `description`, `assignee`, `due` | same |
| Step kinds `agent`, `exec`, `human`, and their keys (`agent`, `exec.*`, `human.*`) | same, unchanged; a step with neither `kind` nor `uses` is `agent` |
| `retry.max_attempts`, `retry.backoff` | same, on any step |
| `include` + `with` (static expansion with prefixing, cycle check, depth 8) | same |
| `repeat` + `until` + nested `steps` (iteration chaining, negated `until` as `when`) | same |
| `name@version` references, version ordering, search-path precedence | same, extended with pack and bundle layers (§2.4) |
| `results.<step>.<path>` namespace, left unrendered until dispatch | same; `results.<step>` is the step's output value (§4.2) |
| Template placeholders `{{ }}`, bare `{{name}}` as a var, `vars.`, `subject.`, `run.`, `steps.` | same grammar, extended (§4) |
| Due arithmetic, durations (`30s`, `5m`, `48h`, `5d`, `2w`) | same |
| Step selection (`--task`, `--with-deps`) and ordinals | same, plan level only |
| Materialize and reconcile contracts, run ledger for the task tool | plan level; owned by the plan implementer, not restated here |
| Human tasks: `approve` and `reject` decisions, `on_timeout` | same |

### 1.2 Changed

| Recipe 0.1 | Flow | Why |
|---|---|---|
| `recipe:` (name) | `flow:` | the document is no longer generation-only |
| A step is a task | A step is a **core kind** (`kind:`) **or** a **node** (`uses:`), never both | one executor for `agent`/`exec`/`human`, plus typed units resolved through node specs |
| `gate: {contract, eva_url}` | `check: {contract, eva_url}` on any step; approval is a node (`flo/approval@1`) | one word per concept |
| `when`/`until` grammar: one comparison | `when`/`until` are CEL expressions; the old single-comparison grammar still parses and keeps its loose equality when an expression matches it exactly (§4.4) | richer conditions without breaking old documents |
| Placeholders: `ref := IDENT("." SEG)*` | `{{ <CEL> }}`; a bare dotted ref is a valid CEL expression; a field that is exactly one placeholder passes the **typed** value (§4.3) | node inputs are typed |
| `run.recipe` | `run.flow` | rename |
| Reserved names: `subject`, `results`, `run`, `steps`, `vars` | adds `trigger`, `state`, `item`, `index`, `config`, `in`, `result`, `decisions` | new namespaces |
| `results.<step>` of a skipped task: absent, reading it is an error | absent; a step that reads an absent result is skipped, not failed (§5.7) | optional branches |
| A run is a set of tasks in the task tool | A run is a tree of step records in the engine; tasks are optional, accountable copies (`tasks:`) | the engine is the executor |
| Evaluation of every step via tasks | The engine dispatches steps itself; the task tool still runs plan-level flows alone | two levels (§9) |

### 1.3 Added

| Key | Where | Section |
|---|---|---|
| `on` | flow | §6 |
| `run_as` | flow, step | §7.5 |
| `privacy` | flow, step | §7.6 |
| `budget` | flow, step | §7.7 |
| `tasks` (and step `task`) | flow, step | §7.8 |
| `state` | flow (declaration) | §7.4 |
| `concurrency` | flow | §7.3 |
| `retention` | flow | §7.10 |
| `returns` | flow (sub-flow interface) | §5.9 |
| `uses` | step | §5.2 |
| `with` | step (also kept on `include`) | §5.2 |
| `for_each`, `on_item_error`, `concurrency` (step) | step | §5.6 |
| `outputs` (`outputs.state`) | step | §7.4 |
| `check` | step | §5.8 |
| `timeout`, `runner` (reserved) | step | §5.1 |
| `x-*` | any mapping | §2.2 |

Reserved for after phase 1 and rejected by the phase-1 validator with
`reserved_key`: `subject`, `stages`, `until` on stages (not on `repeat`),
and the node names `flo/context@1`, `flo/propose@1`. §10 shows they fit
without a change to anything below.

---

## 2. Document

### 2.1 Encoding

As recipe 0.1 §1.1: YAML or JSON (`.json` by file name), root a mapping,
unknown keys are errors, no extension point skips validation.

### 2.2 Extension keys

Any mapping may carry keys prefixed `x-`. The validator ignores them and
tools MUST preserve them on rewrite. (Open question 4: align with the task
tool's extension namespace.)

### 2.3 Top-level keys

| Key | Type | Required | Meaning |
|---|---|---|---|
| `flow` | string | yes | Name, `^[a-z0-9][a-z0-9-]*$` |
| `version` | string | yes | Compared as in recipe 0.1 §1.4 |
| `description` | string | no | Free text |
| `requires.subject` | `task` \| `track` \| `none` | no | As recipe 0.1; plan level |
| `on` | list of Trigger | no | §6. Absent: the flow starts only through `flo run create` (as if `manual: {}`) |
| `vars` | map of VarDef | no | §3 |
| `state` | map of StateDef | no | §7.4 |
| `agent` | string | no | Default agent for `agent` steps |
| `run_as` | string | no | aps profile the run acts as (§7.5). Default: the profile that started the run, if granted |
| `privacy` | `local-only` \| `cloud-allowed` | no | Egress ceiling (§7.6). Default `local-only` |
| `budget` | Budget | no | §7.7. Default: engine default |
| `tasks` | `none` \| `per-run` \| `per-step` | no | §7.8. Default `per-run` |
| `concurrency` | Concurrency | no | §7.3 |
| `retention` | Retention | no | §7.10. Default: recordings kept 90 days |
| `track` | `{title, type, plan}` | no | Plan level, as recipe 0.1 |
| `returns` | map name → expression | no | §5.9 |
| `steps` | list of Step | yes | At least one |

### 2.4 Bundle layout and references

A flow lives in a directory (a *bundle*) so its supporting files travel
with it:

```text
flows/<flow>/
  flow.yaml              the document
  nodes/                 flow-local node specs (node.md)
  prompts/               versioned prompt files used by AI nodes
  trusts/                flow-local trust providers
  tests/                 expect: blocks for `flo flow test`
  record/                recorded cassettes and plans (written by `--record`)
```

A single `<name>.yaml` file is also a flow (no `nodes/`, `prompts/`, …
beside it unless in a bundle).

References (`name`, `name@version`, path) resolve as in recipe 0.1 §1.3.
Search layers, in order: the bundle itself (for `nodes/`, `prompts/`,
`trusts/`), the project's `flows/`, the user config `flows/`, installed
packs, built-in. A node type reference (`ns/name@N`, node.md §2) resolves
through bundle `nodes/`, installed packs, then built-in; the namespace
`flo/` is reserved for built-ins.

A flow reference never contains `/` unless it is a path; a node type
always does. That is how `uses:` tells a sub-flow from a node (§5.2).

---

## 3. Variables

As recipe 0.1 §2 with one addition: a var may declare `type`
(`string` default, `integer`, `number`, `boolean`, `duration`, `list`,
`map`), and a `default` keeps its declared type. Values bound from
`--var key=value` are strings and are coerced to the declared type;
failing coercion is `invalid_var`. List and map values are bound from a
JSON literal or `--var-file`.

Reserved names (MUST NOT be declared): `subject`, `results`, `run`,
`steps`, `vars`, `trigger`, `state`, `item`, `index`, `config`, `in`,
`result`, `decisions`.

Binding order: provided value, else trigger-supplied (a manual trigger's
`--var`), else default, else missing. `required: true` with a `default` is
an error.

---

## 4. Expressions and templates

### 4.1 Placeholder

```
placeholder := "{{" WS? cel WS? "}}"
```

`cel` is a CEL expression (the engine uses kit `runtime/policy/withcel`).
The scanner is CEL-aware: it tracks string literals and brace depth, and
`}}` closes a placeholder only at depth zero. So a map literal inside
an expression (`{'a': {'b': 1}}`) is safe.

A bare identifier or dotted reference is a CEL expression, so
recipe 0.1 placeholders (`{{repo}}`, `{{results.sec/scan.stdout}}`,
`{{steps.fix/1/test.due}}`) stay valid. Two compatibility rules:

- a bare identifier that is not a CEL binding resolves as a var;
- path segments after the first may contain `/` and `-` when they name an
  expanded step id (`results.sec/scan` is read as the step `sec/scan`).

### 4.2 Namespaces

| Name | Bound | Contents |
|---|---|---|
| `vars` (and bare names) | run start | resolved vars |
| `subject` | run start | as recipe 0.1 §11; plan level |
| `run` | run start | `id`, `flow`, `version`, `started_at` (RFC 3339), `iteration` (inside `repeat` only) |
| `trigger` | run start | `kind` (`schedule`\|`manual`\|`calendar`\|`event`), `at` (the instant the trigger was due), plus the trigger's payload: `event` (calendar, event), `vars` (manual) |
| `state` | run start | snapshot of the flow's state at run start (§7.4); not changed by this run's staged writes |
| `config` | run start | engine and pack configuration (read-only), e.g. `config.ctxt.url` |
| `steps` | render | as recipe 0.1: static fields of already rendered steps (`id`, `title`, `description`, `due`, `assignee`, `when`, `agent`, `kind`) |
| `results` | dispatch | `results.<step>` = the step's output value (§5.5) |
| `item`, `index` | per `for_each` element | the element and its 0-based position (§5.6) |
| `result` | `outputs` and `check` of one step | this step's own output value |
| `decisions` | approval hooks only | the item decisions (node.md §10) |

`in` is bound only inside node specs (node.md §3).

Which fields may hold a placeholder: `title`, `description`, `assignee`,
`due`, `when`, `until`, `agent`, `exec.argv[*]`, `exec.cwd`, `exec.env`
values, `human.assignee`, `check.contract`, `for_each`, and every string
value, at any depth, under `with` and `outputs`. Triggers' `when` and
`source` options are templatable against `trigger` only.

### 4.3 Rendering

- A string field that is **exactly one placeholder** evaluates to its
  typed value and is passed through as such: a list stays a list, a map a
  map, a number a number, null null.
- A string with text around a placeholder renders to a string: a string
  as itself, a list of scalars joined with `,`, anything else as JSON.
- An unresolvable reference is an error (`unresolved_ref`), never a
  literal `{{ }}`. The exception is §5.7 (absent results).
- Every other value in `with` that is a mapping or list is walked and its
  string leaves rendered by the same two rules.

### 4.4 Conditions (`when`, `until`)

A condition is a CEL expression returning a boolean; the value may be
written bare (`when: results.test.exit_code == 0`) or as one placeholder.

Compatibility with recipe 0.1 §13: when the whole expression matches
`lhs OP rhs` of that grammar (a `results.`/`vars.`/bare lhs, a literal or
identifier rhs, one operator), it is evaluated by the 0.1 rules, including
loose equality (`==` compares `%v` renderings, so `1` equals `'1'`).
Every other expression is CEL, which is strictly typed. The negation rule
for `until` (`==`↔`!=`, …) applies to the 0.1 form; for CEL, `until` is
wrapped in `!( … )`.

A condition that errors blocks the step with `when: <error>` as the
reason, as in recipe 0.1 §12.3.

### 4.5 Functions

CEL standard library, plus the lists extension (`flatten`, `slice`,
`sort`, `distinct`, …) and the strings extension (`join`, `split`,
`lowerAscii`, …). Flows rely on `flatten()` (§5.6). If an engine's
CEL environment lacks a function a flow uses, validation fails with
`unknown_function`. No other functions are added in 0.1.

---

## 5. Steps

`steps` is an ordered list. Each entry is exactly one of: a **task step**
(a core kind or a node), an **include**, a **repeat block**. After
expansion every entry is a task step.

### 5.1 Keys common to every step

| Key | Meaning |
|---|---|
| `id` | Required, `^[a-z0-9][a-z0-9-]*$`; `/` reserved for expansion; unique in its list |
| `depends_on` | Step ids of the same list that must be terminal first |
| `when` | Dispatch-time condition (§4.4) |
| `title`, `description`, `assignee`, `due` | As recipe 0.1; used when the step has a task (§7.8) |
| `retry` | `{max_attempts, backoff}` (§5.10) |
| `check` | `{contract, eva_url}` (§5.8) |
| `timeout` | Duration bound for the step; the node spec supplies a default |
| `run_as`, `privacy`, `budget` | Narrow the flow value for this step only (§7.5–7.7) |
| `task` | `true`\|`false`: whether this step gets an accountable task under `tasks: per-step` |
| `runner` | Reserved: runner class (phase 1: `local` only) |

### 5.2 Step shapes

Exactly one of:

```yaml
- id: build                      # core kind (agent, exec, human), as recipe 0.1
  kind: exec
  exec: { argv: [make, build] }

- id: deps                       # node
  uses: gh/dependency-list@1
  with: { repos: "{{ vars.repos }}" }

- id: brief                      # sub-flow
  uses: meeting-brief@0.1.0
  with: { event: "{{ trigger.event }}" }

- id: sec                        # include (static expansion, recipe 0.1 §3.3)
  include: security-scan@1.2.0
  with: { target: "{{ vars.pr }}" }
```

- `kind:` and `uses:` are mutually exclusive. With neither, the step is
  `agent`.
- `uses:` containing `/` before the `@` is a **node** (`ns/name@N`, `N` an
  integer major). Otherwise it is a **sub-flow** (`name@semver`, exact
  version required).
- `with:` is valid on `uses` and `include` steps only. Every key MUST be
  an input of the node, or a var of the sub-flow or included flow. For
  nodes, `with` values are typed per the node's `inputs` (node.md §3).
- `include` and `uses: <flow>` differ: `include` splices the child's
  steps into this document at plan time (prefixed ids, `with` as var
  binding, plan-level, no new run record); `uses: <flow>` runs the child
  as one step of this run (§5.9).

### 5.3 Core kinds

`agent`, `exec` and `human` are exactly recipe 0.1 §3.2: same keys, same
results. In the engine:

- `exec` runs `exec.argv` through `aps run <run_as> --`, result
  `{exit_code, stdout, stderr, duration_ms, truncated}`;
- `agent` launches the named agent through aps under `run_as`, result the
  `outputs` object of its results file;
- `human` parks the run until a person decides (approve/reject), as in
  recipe 0.1 §12.7. It is a whole-step decision. Item-level decisions are
  the `flo/approval@1` node.

Core kinds have no node spec; their side effect is unknown, so they are
treated as `write-shared` for admission, and an `exec` step MUST NOT be
the only guard of irreversible work (§8, rule V20).

### 5.4 Readiness and order

A step is **ready** when every `depends_on` entry is terminal and every
step named by a `results.<id>` reference in its `with`, `for_each`,
`when`, `outputs` or `check` is terminal. That second part makes data
references implicit edges; `depends_on` is for ordering with no data
(a script that hands over files, a side effect that must come first).
Terminal means `succeeded`, `skipped` or `failed`-after-retries. A step
whose dependency **failed** is `skipped` with reason `upstream_failed`; the
run is `failed` when any step failed. Dependents of a **skipped** step
still run (recipe 0.1 behavior) unless they read its result (§5.7).

Ready steps run concurrently, bounded by the engine's worker limit. There
is no implicit sequential order: list order is only a tiebreak. Every
`depends_on` entry and every referenced result MUST name a step of the same
list, and the combined graph MUST be acyclic.

### 5.5 Step output

A step's **output** is the value `results.<id>` reads:

| Step | Output |
|---|---|
| `exec` | the exec result object |
| `agent` | the agent's `outputs` object |
| `human` | `{decision: approved\|rejected, by, at, note}` |
| node | the node's `outputs` value, typed per its spec |
| sub-flow | the sub-flow's `returns` (§5.9) |
| `for_each` | the list of per-element outputs (§5.6) |
| skipped, cancelled | absent |

Outputs are JSON values, stored in the run tree. An output over the
engine's size limit is stored by reference and still readable through
`results`.

### 5.6 `for_each`

```yaml
- id: releases
  uses: gh/releases-since@1
  for_each: "{{ results.deps.packages }}"
  concurrency: 4
  on_item_error: skip
  with: { package: "{{ item.name }}" }
```

- `for_each` evaluates to a list (a map is an error). The step runs once
  per element. `item` and `index` are bound in `with`, `when` (per
  element: false skips that element), `retry`, `timeout`, `check` and
  `outputs`.
- `results.<id>` is the list of per-element outputs **in input order**.
  Skipped elements are left out of the list and recorded in the run log.
- When each per-element output is itself a list or holds a list, flows
  flatten explicitly: `results.releases.map(r, r.releases).flatten()`. The
  engine never merges implicitly.
- `concurrency:` (default 4) bounds in-flight elements. An empty list
  succeeds with `[]`.
- `on_item_error`: `fail` (default; one failing element fails the step
  after the rest finish or are cancelled) or `skip` (record and continue).
- Elements share the step's `budget` and `retry`. Each element is its own
  child record in the run tree; `check` runs per element.
- A built-in node that iterates over a list it receives (`flo/dedupe@1`,
  `flo/filter@1`) binds `item` per element when it evaluates its own
  expressions (node.md §6.4). `for_each` on the step and iteration inside
  a node are different things: use the node's own list input when the
  node accepts a list.
- `for_each` is run level (§9).

### 5.7 Absent results

`results.<id>` is absent when step `<id>` was skipped or cancelled. A step
whose `with`, `for_each`, `when` or `outputs` reads an absent result is
**skipped** with reason `upstream_skipped`, not failed. Guard with
`has(results.<id>)` to read it conditionally:

```yaml
when: "{{ has(results.review) && size(results.review.approved) > 0 }}"
```

With a `has()` guard the expression is evaluated, and absence is
data. Reading a field of a present result that does not exist is an
error.

### 5.8 `check`

`check: { contract: <name>, eva_url: <url> }` (both templatable; the URL
may come from `config.eva.url`). After a step produces its output the
engine sends `{"contract": <name>, "body": <output>}` to the eva gateway
(`POST /v1/contract/invoke`). 200 passes. 422 is a failure
(`check_failed`, summary `check <contract>: <error>`). Any other non-200
status or an unreachable gateway is a transient failure and retries per
the step's `retry`.

A check runs before the step's `outputs.state` is staged and before any
dependent starts. For `for_each` steps it runs once per element. A flow
with a `check` and no reachable gateway fails `flo flow test` (testing is
outside this spec; see the run and test documents).

### 5.9 Sub-flows and `returns`

A sub-flow step runs another flow version as one step:

- the interface is its `vars` (inputs) and its `returns` (output);
- the child is **version-pinned** (`name@semver`); `name` alone is a
  validation error;
- loops are refused (a flow reachable from itself through `uses:`), and
  depth is capped at 8;
- it runs inside the parent run's tree, **within the parent's grant**
  (`run_as` may only narrow; capabilities are intersected), and its
  approvals and budget **roll up** to the parent;
- the child's `on:` is ignored; its `trigger` namespace is the parent's.

`returns` maps names to expressions evaluated after the last step:

```yaml
returns:
  text: "{{ results.brief.text }}"
```

Without `returns` the child's output is a map of all its step outputs by
step id. `for_each` is allowed on a sub-flow step.

### 5.10 Retry and timeout

`retry.max_attempts` includes the first attempt (default 1);
`retry.backoff` is the sleep before the next. The node spec supplies
defaults (node.md §11); the step overrides them. A node that is not
idempotent and declares no idempotency key has `max_attempts` clamped to
1 and a validation warning if the flow asks for more (§8, V22).
`timeout` bounds one attempt. `exec.timeout` and `human.timeout` keep
their recipe meaning.

### 5.11 Include and repeat

As recipe 0.1 §3.3–§3.4 and §6, unchanged: prefixing, leaves, inherit
edges, cycle detection, depth 8, `{{run.iteration}}`, chained iterations,
`until` as the negation `when` on later iterations. A `repeat` block's
nested steps may be any step shape; `for_each` and `uses` steps inside a
block are allowed. A repeat block has no `for_each` of its own; use a
sub-flow with `for_each` for data-driven repetition.

---

## 6. Triggers (`on:`)

`on` is a list; any entry may start a run. Each entry is a mapping with
one key naming the trigger kind.

| Kind | Config | `trigger` payload | Phase |
|---|---|---|---|
| `manual` | `{}` | `vars` (from `--var`) | 1 |
| `schedule` | `rrule` (RFC 5545 string, required), `tz` (IANA, default UTC), `jitter` (duration, optional) | none | 1 |
| `calendar` | `source` (a trigger node, e.g. `calendar/events@1`), `offset` (signed duration from the event start, default `0`), `when` (condition over `trigger.event`), `horizon` (default `7d`), config of the source | `event` | 1 |
| `event` | `source` (a trigger node), `when` (condition over `trigger.event`), source config | `event` | 1 via plugin, generic in 3 |
| `webhook`, `file` | reserved | | 2 |

- A trigger kind that is not `manual` or `schedule` names a **trigger
  node** (node.md §7, `role: trigger`), which declares its payload schema.
  `trigger.event` is validated against it.
- `schedule`: the next occurrence is computed from the RRULE and `tz`
  (`hop.top/vstar/rrule`); recurrence is made by enqueueing the next
  occurrence after each fire. A fire missed while the engine was down is
  run once on start if still within the schedule's period, else dropped.
- `calendar`: the engine keeps one pending fire per upcoming event within
  `horizon`, keyed by (event UID, recurrence id, offset). A moved or
  cancelled event cancels and re-enqueues its fire. A fire time already in
  the past fires immediately. `when` is evaluated when the fire is
  planned, not when it fires.
- A trigger has no side effects and no grant requirement of its own; the
  trigger node's capabilities (a calendar read) are checked when the flow
  is activated and when the source syncs, against the flow's `run_as`.
- `on:` is ignored when a plan-level tool materializes a flow by hand,
  with a warning.

### 6.1 Run key

Each fire derives an **idempotency key**; a second fire with the same key
returns the existing run.

| Trigger | Key |
|---|---|
| `schedule` | (flow, the RRULE occurrence instant) |
| `calendar` | (flow, event UID, recurrence id, offset, event start) |
| `event` | (flow, source event id) |
| `manual` | `--idempotency-key` if given, else none (every create is a new run) |

The flow version is not part of the key: a fire belongs to the version
active when it is enqueued.

---

## 7. Run semantics

### 7.1 Run lifecycle

`queued → running → (awaiting_approval ↔ running) → succeeded | failed |
cancelled`. A run **awaiting approval** (or a `human` step) is parked: no
worker holds it and it survives a restart. A decision resumes it (node.md
§10). `budget_exceeded` is a `failed` run with that error code.

Step statuses: `pending`, `running`, `waiting`, `succeeded`, `failed`,
`skipped`, `cancelled`. Each step record keeps inputs, output, cost,
attempts, receipts and the node spec version used.

### 7.2 Version at run time

A run uses the flow version that was **active** when its trigger was
enqueued, and keeps it for its whole life, including across a restart or a
park. Activating a new version never changes a parked run.

### 7.3 Concurrency

```yaml
concurrency:
  group: "{{ trigger.event.uid }}"     # default: the flow name
  max: 1                               # default
  on_overlap: skip                     # skip | queue | cancel_previous
```

At most `max` runs of a group are active at once. A parked run is active.
`on_overlap` decides what a new fire does when the group is full. Default:
`skip` for runs started by `schedule`, `queue` otherwise. A skipped fire
is recorded, not run.

### 7.4 State

```yaml
state:
  seen:   { default: [] }
  cursor: { default: null }
```

- State is a JSON key-value map **per flow and per tenant**, read as
  `state.<key>`. Every key MUST be declared with a `default`; reading an
  undeclared key is a validation error.
- `state` in a run is a **snapshot** taken at run start. A run never sees
  its own staged writes.
- A step writes state through `outputs.state`, a map from key to a
  placeholder evaluated after the step (and its check) succeeds, with
  `result` bound to the step's output:

  ```yaml
  outputs:
    state: { seen: "{{ result.seen }}" }
  ```

- Writes are **staged** and committed atomically when the **run succeeds**.
  A failed or cancelled run leaves state untouched, so a re-run sees the
  same state. Two steps writing the same key is a validation error (V17).
- With `max: 1` concurrency (the default) there is one writer per flow.
  When `max > 1`, commits are last-writer-wins per key; flows that need
  more use an explicit lock group.
- A state value is limited to 1 MiB; exceeding it fails the run
  (`state_too_large`). Collections that grow without bound are the flow
  author's job to bound (see `flo/dedupe@1`'s `max`).
- Plan-level tools refuse a flow that reads `state` (`engine_feature`).

### 7.5 `run_as`

`run_as` names an aps profile. The caller who creates, edits or activates
the flow MUST be granted that profile; the profile MUST be granted every
capability the flow's nodes declare (node.md §12). A step-level `run_as`
may only name a profile whose grants are a subset of the flow's for the
capabilities that step needs. Command nodes run through
`aps run <profile> --`; HTTP nodes and built-ins use the profile's
credentials and grants.

### 7.6 `privacy`

An egress **ceiling** for the flow (or one step):

- `local-only`: no node whose `privacy` is `cloud` may run, and AI nodes
  must name a local model (`ollama://<model>`, a local `scheme://host:port/<model>`; node.md §9).
- `cloud-allowed`: nodes with `privacy: cloud` may run.

Nodes declare their own level (node.md §9); validation compares. The step
value may only narrow (`cloud-allowed` → `local-only`). The engine does
not track data lineage in 0.1: a flow that is `cloud-allowed` may send the
output of a `local` node to a cloud node. That is a property of the flow,
visible in the document and in review. (Open question 2.)

### 7.7 `budget`

```yaml
budget: { usd: 0.50, tokens: 200000, duration: 10m, calls: 500 }
```

All keys optional. `usd` and `tokens` are summed from the `cost` of model
nodes' step records; `calls` counts node and command invocations;
`duration` is wall-clock from run start (parked time excluded).
Before a step starts, the engine compares the node's cost class and
estimate (node.md §13) with the remaining budget; when a step finishes it
adds the actual cost. Exceeding any limit stops the run with
`budget_exceeded`; steps in flight are allowed to finish. A step `budget`
caps that step (including its elements) and counts toward the flow's. A
sub-flow's budget counts toward its parent's.

### 7.8 `tasks`

Tasks are optional accountable copies in the task tool.

- `none`: the engine creates no task for the run. (An approval step with
  `surface: tlc` still creates its own human task, because that task is
  the approval surface.)
- `per-run` (default): one task per run, created lazily the first time the
  run needs a person (an approval, a `human` step) or fails, titled
  `<flow> <trigger instant>`, due at the trigger instant. A run that never
  needs a person creates none. Approval tasks hang under it.
- `per-step`: a task for each `human` step, each `flo/approval@1` step and
  each step with `task: true`, using the step's `title`, `description`,
  `assignee`, `due`.

A step's result is copied onto its task only if the step has a task.
Approving the accountable task approves every item still undecided;
items already decided keep their decision. Rejecting rejects every
undecided item.

### 7.9 Retries and idempotency

Retries follow `retry`. A step re-executed after an unknown outcome (a
timeout, a crash) is safe only when the node is idempotent or supplies an
idempotency key (node.md §11); otherwise the step is marked `uncertain`,
the run is parked as awaiting a decision, and nothing is repeated
automatically.

### 7.10 `retention`

```yaml
retention: { recordings: 30d }
```

How long the data a flow's runs leave behind is kept, per flow. One key in
0.1:

- `recordings`: how long a recording of one of the flow's runs
  (`flo run create --record`) is kept, counted from when it was made. A
  duration, at least `1d` and at most `3650d`; `d` and `w` units are
  accepted. Absent: 90 days, the retention of outputs.

The engine reads the value from the flow's active version (else the
newest) each time it prunes, so a change applies to recordings already
made. A recording of a flow that no longer exists keeps the default.
Copies of a recording made into a bundle (`flo flow test --from-run`) are
the bundle's files and are never pruned.

### 7.11 Drafts (the chat builder)

A **draft** is a proposed full next version of a flow: not a flow version,
and not stored where versions are. A model makes it from a person's
instruction and the flow's **base**, the version `flo flow show` would show
(the active one, else the newest). Rules:

- **Full document.** The model returns the whole next document, not a
  patch. It keeps `flow:` and changes `version:`; a proposal that does
  either wrong is refused like any other invalid one.
- **Validation loop.** The proposal goes through the validation of §8. Its
  errors go back to the model, up to three times (so at most four model
  calls); then the build fails with `invalid_flow` carrying the last
  errors and nothing is kept.
- **The model call is a node call.** It goes through the same client as
  `flo/ai.generate@1`: the model allow and deny lists apply, keys come from
  the secret store, and the flow's `privacy` ceiling holds, because the
  document is sent to the model: a `local-only` flow is only ever sent to a
  local model. A refused call sends nothing. The default model is local.
- **Dry run.** A valid proposal is replayed offline, as `flo run replay`
  does, on the flow's last recording (§7.1), and the report lists where the
  outcome differs from the recorded run: a step on one side only, another
  status, another output, and the calls that have no recording. No
  recording is not an error; the report says so.
- **Apply gate.** Applying replays again, and refuses with
  `draft_dry_run_failed` (exit 4, the draft kept) when the replay shows a
  step that failed (in the replay, whatever the recorded run did), a step
  with another status than in the recorded run, the run ending with another
  status, or a call with no recording; the error lists them. `--force`
  applies anyway and the report lists what it went over. A step that only
  has another output, a step that appears or does not run, and a replay
  that was not made (no recording, or one that cannot be replayed) are
  reported, never blocking: with nothing to compare, the apply is allowed
  and the report carries the reason.
- **Apply and `draft_stale`.** Applying stores the draft's document as the
  next version (a draft version, exactly as `flo flow apply` stores it:
  validated again, grants checked, tested and activated as any version) only
  if the base has not moved: neither the base version nor the newest stored
  version is another one than at the time of the draft. Otherwise the apply
  fails with `draft_stale` (exit 4) and the draft is kept to be read. An
  applied draft is spent.
- **`--write`.** Applying with `--write` also rewrites the `flow.yaml` of
  the bundle the flow was applied from with the draft's document. The
  draft records the file's hash when it is made; if the file on disk is
  another one by then (changed or gone), the apply fails with
  `draft_stale` before anything is stored. The write is atomic (a
  temporary file beside it, then a rename) and keeps the file's mode.
  Without `--write` no file is touched. A flow applied from a single file
  has no bundle to write to (`usage`).

- **A new flow (`--new <name>`).** A draft of a flow that does not exist
  yet. The name must be free (`duplicate` otherwise, and `draft_stale` when
  it is taken between the draft and the apply) and match the flow-name
  grammar; the model must set `flow:` to it and give a `version`. There is
  no base, no recording and so no dry run: the report says so, and the
  apply gate has nothing to block on. There is no flow to read a privacy
  ceiling from, so the model call is `local-only` (a local model only)
  unless the person gives `--privacy cloud-allowed`; the ceiling is stored
  on the draft, and the document's own `privacy` is validated as for any
  flow. Applying stores the first version. With `--write` a new bundle
  `<dir>/<name>/flow.yaml` is written (`--dir`, default `flows`, the
  directory flows are kept in); an existing `<dir>/<name>` is refused
  (`duplicate`) and nothing is stored.
Drafts are run data of the tenant: `<tenant dir>/drafts/<id>.json`, readable
by the owner only.

---

## 8. Validation rules

`flo flow validate` and every save and activation run these. Errors carry
a stable `code`; `V` numbers are for this document. All are errors unless
marked *warning*.

Header and keys:

- **V01** `flow` and `version` present and well-formed; **V02** `steps`
  non-empty; **V03** unknown keys are errors except `x-*`; **V04** reserved
  keys of §1.3 (`reserved_key`).

Vars and state:

- **V05** recipe 0.1 var rules (§3 here); **V06** every `state.<key>`
  reference names a declared key; **V07** every state key has a `default`.

Steps (recursively into blocks):

- **V08** `id` grammar, unique per list; **V09** exactly one of core kind,
  `uses`, `include`, repeat block; **V10** `kind` and `uses` not both;
  **V11** `with` only on `uses`/`include`, keys within the child's inputs
  or vars, required inputs present; **V12** `uses` resolves (node found, or
  sub-flow pinned and found); sub-flow loops and depth refused.
- **V13** `depends_on` targets exist in the same list; the combined graph
  (depends_on plus `results.` edges) is acyclic; a `results.<id>` target is
  an upstream step.
- **V14** every template reference resolves in its namespace (§4.2);
  `item`/`index` only under `for_each` or inside a built-in's own
  expressions; `result` only in `outputs` and `check`; `trigger.event` only
  when some trigger declares an `event` payload.
- **V15** expressions parse and type-check against known types where the
  node spec gives them (`with` values against `inputs`, `for_each` is a
  list); functions exist (`unknown_function`). Checking is partial: only
  top-level names have types (`index` is an int, declared var types, the
  namespaces are maps); fields read through `results`, `state`, `trigger`
  and the other namespaces are dynamic, so a wrong field type is found at
  evaluation. Errors carry the step, the field and the line:column in it.
- **V16** durations parse; `retry`, `budget`, `retention` well-formed
  (`retention.recordings` between `1d` and `3650d`).
- **V17** at most one step writes a given state key; `outputs` only has
  `state`.

Safety and access (these need the node specs and the author's grants):

- **V18** the flow's `run_as` is granted to its author, and every
  capability the nodes declare is granted to that profile
  (`grant_denied`).
- **V19** privacy: no `cloud` node under `local-only` (flow or step); an
  AI node's model scheme matches (`privacy_violation`).
- **V20** every step with side effect `irreversible` has an `approval`
  guard: either an ancestor `flo/approval@1` step (in the combined graph),
  or `trust: <provider>` set on the step and the node declares a trust
  class (`unguarded_irreversible`). A step of a core kind (`exec`, `agent`)
  is never accepted as guarded by trust.

  Pattern: items that trust decides automatically still go through
  `flo/approval@1`. The approval node decides those items itself (its
  `auto` output) and parks the run only for the rest, so a flow passes
  both sets (`items: pending + auto`) and the guard of the irreversible
  step always finds a decided approval. Skip the approval only when there
  is nothing to decide, and gate the irreversible step on something having
  been approved or automatic. A skipped approval does not count as a guard:
  that alternative was rejected because it weakens the guard (it would hold
  for a run that decided nothing). See examples.md §3.
- **V21** the budget is at least the sum of the minimum estimates of
  nodes that must run (*warning*).
- **V22** `retry.max_attempts > 1` on a non-idempotent node without an
  idempotency key (*warning*; the engine clamps).
- **V23** a `check` names a contract (templatable) and exactly one step
  (*warning* when eva is not configured at validation time).
- **V24** a sub-flow's capabilities and privacy are within the parent's.
- **V25** `run_as` and `privacy` on a step only narrow the flow's value.

Triggers:

- **V26** `schedule.rrule` parses; `tz` is an IANA name; **V27** trigger
  nodes exist and `role: trigger`; `when` references only `trigger`;
  **V28** a flow with `calendar` or `event` triggers and a `manual`
  trigger declares the same `with` shape for `trigger.event` consumers, or
  guards with `has(trigger.event)`.

A flow is **refused at activation**, not only warned, on any error
above. Activation additionally requires a passing test of that exact
version (outside this spec).

---

## 9. Conformance levels

| Level | Covers | Implemented by |
|---|---|---|
| **Plan** | Parse, validate (V01–V17), expand `include`/`repeat`, render, due arithmetic, selection, materialize into tasks, run the core kinds (`agent`, `exec`, `human`) | the flo spec module, used by flo and by the task tool |
| **Run** | `on:`, `uses:` steps that lower to `engine`, `for_each`, `state`, sub-flows, `check` orchestration beyond core, placement, admission, budgets, approvals, the run tree | flo only |

How a plan-level tool treats a flow:

| Construct | Plan level |
|---|---|
| `kind:` steps, `include`, `repeat`, `depends_on`, `when` on `results` of core steps, `due`, `vars` | runs |
| `uses:` node that lowers to `exec` | expands into a plain `exec` step (node.md §14) |
| `uses:` node that lowers to `engine`, or has no spec | refused: `engine_node`, exit 2, suggestion `flo run create --flow <name>` |
| `uses:` sub-flow | refused: `engine_node` (a sub-flow is an engine feature in 0.1) |
| `for_each`, references to `state`, `trigger` | refused: `engine_feature` |
| `on`, `run_as`, `privacy`, `budget`, `concurrency`, `tasks`, `retention`, `returns` | ignored with a warning |
| `check` | runs as the recipe gate did |

---

## 10. Reserved: workflows with subjects (after phase 1)

These keys are rejected in phase 1 but the grammar above is closed under
them. They are listed so phase-1 decisions do not paint them out.

- `subject: { kind, id }` names what a run is about. A run is keyed by
  (flow, subject), so there is one long-lived run per client or deal;
  `subject` extends the idempotency key.
- `stages:` replaces `steps:` with a list of named sub-flows, each with
  `until: { event: …}` (a stage ends when the event arrives) and its own
  `steps`. The run parks in the `waiting` status between stages; the
  parked-run mechanics of §7.1 apply unchanged. Under an external
  workflow engine the stage state is read from it.
- `event` triggers carry stage events; `trigger.event` is the event.
- `flo/context@1` (a node whose output kind is `context`) and
  `flo/propose@1` (a node that returns proposals instead of executing)
  are ordinary nodes; node.md §6 reserves their names and shapes.
- Privacy defaults: a subject-bearing flow defaults to `local-only`.

Nothing in §1–§9 depends on a step list being flat: `stages` are named
step lists with the same rules per list.

---

## 11. Open questions

1. **Spec marker.** Whether a document carries `spec: flo/0.1` (like a
   `$schema`), or the version is implied by the tool. Chosen for 0.1: no
   marker; revisit when 0.2 exists.
2. **Data lineage under `cloud-allowed`.** The engine does not stop local
   data reaching a cloud node when the flow allows cloud. A `data:
   public|private` class on outputs would let it. Chosen: ceiling only.
3. **Parked runs and short schedules.** A 15-minute schedule with a parked
   approval means the next fire is skipped (§7.3) and triage stalls until a
   decision. An approval mode that detaches (run completes, decisions
   trigger a continuation) is likely needed; not specified in 0.1.
4. **Extension namespace.** `x-*` here versus the task tool's own
   extension namespace for round-trip fields; align when the spec module is
   extracted.
5. **Legacy loose equality.** §4.4 keeps recipe 0.1's `%v` equality for
   expressions that match its grammar. Remove once no recipe depends on it.
6. **Block-level `for_each`.** Only step-level and sub-flow `for_each` in
   0.1.
7. **Cancellation of a parked run's pending approval items.** Chosen:
   cancelling a run rejects its undecided items with reason `run_cancelled`;
   such rejections are not reported to a trust provider (node.md §10.3).
8. **State size and shape.** 1 MiB per key, JSON only; a set type with a
   bound may be preferable to lists.
9. **CEL list extension in the engine.** `flatten` depends on the engine's
   CEL environment enabling the lists extension.
10. **Plan-level parse of lowered node output.** A node that lowers to
    `exec` and declares parsed outputs (`format: json`): the plan-level
    tool gets the raw exec result; whether the spec module also parses
    stdout into `results.<id>` there is undecided.
