# Node Specification (FLO-NODE) v0.1

- Version: 0.1
- Status: draft
- Companion documents: [flow.md](flow.md), [examples.md](examples.md)

## Summary

A **node** is a typed, versioned unit of work that a flow step can use
(`uses: ns/name@N`). A **node spec** is the document that describes it:
inputs, outputs, configuration, what it may touch, what it costs, whether it
can be repeated, and how it runs. The engine reads node specs to validate
flows before they run, to decide what needs approval, and to decide where
and as whom a step executes.

There are three kinds of node:

| Kind | Written as | Runs as | Lowers to |
|---|---|---|---|
| **built-in** | engine code, spec shipped with the engine (`flo/…`) | engine, in process | `engine` |
| **declared** | data only: a YAML file wrapping a command or an HTTP call | `aps run <profile> -- argv`, or an HTTP request | `exec` (command), `engine` (HTTP) |
| **plugin** | code behind kit's extension interface, in process in phase 1 | engine, in process (subprocess plugins later) | `engine` |

All three present the same face to a flow: the fields below. A flow
author does not need to know which kind a node is, except for what it
lowers to (a plan-level tool can run only `exec`-lowering nodes; flow.md
§9).

This spec OWNS: the node spec grammar; the inputs, outputs, side-effect,
privacy, approval, idempotency, cost and placement vocabularies; the
declared command and HTTP shapes; pack layout; the built-in catalog for
phase 1.

This spec DOES NOT own: the plugin call protocol (in-process in phase 1;
subprocess protocol is a later addition to the extension interface), the
grant and admission backends, prompt text of AI nodes.

---

## 1. Document

A node spec is one YAML (or JSON) document, root a mapping, unknown keys
are errors (except `x-*`). Built-in and plugin specs are YAML files
shipped with the code that implements them; declared specs *are* the node.

### 1.1 Keys

| Key | Type | Required | Meaning |
|---|---|---|---|
| `node` | string | yes | Type: `ns/name@N` (§2) |
| `kind` | `command` \| `http` \| `builtin` \| `plugin` | yes | §2 |
| `role` | `step` \| `trigger` | no | Default `step`. A `trigger` node feeds `on:` (§7) |
| `description` | string | no | One sentence shown by `flo node show` |
| `lowers_to` | `exec` \| `engine` | no | Default `exec` for `command`, `engine` otherwise (§14) |
| `config` | schema | no | Static settings an operator sets for a pack (not per-step); exposed as `config.<ns>.*` |
| `inputs` | map name → InputDef | yes (may be `{}`) | §3 |
| `outputs` | OutputDef | yes | §3 |
| `capabilities` | list of string | yes (may be `[]`) | §12 |
| `side_effect` | enum | yes | §8 |
| `privacy` | `local` \| `cloud` | yes | §9 |
| `approval` | `none` \| `required` \| `trust` | no | §10. Default derived from `side_effect` |
| `trust` | `{class, provider?}` | no | The trust class of an action (§10.3) |
| `idempotent` | bool | no | §11. Default `false` for writes, `true` otherwise |
| `idempotency_key` | placeholder | no | §11 |
| `cost` | `{class, estimate}` | no | §13. Default class `free` |
| `timeout` | duration | no | Default per attempt, default `30s` |
| `rate_limit` | `{bucket, max, per}` | no | A provider quota the node draws on (§13.1) |
| `exit_codes` | `{ok, retry}` | no | Command nodes, §4.1 |
| `retry` | `{max_attempts, backoff}` | no | Default `{1, 0}` |
| `placement` | `{runner, isolation}` | no | §15. Default `{local, none}` |
| `record` | bool | no | §16. Default `true` for declared and plugin, per-node for built-in |
| `request` / `argv` … | | per kind | §4, §5 |
| `examples` | list | no | `{name, in, out?}`; used by `flo node show`, by the chat builder, and as fixtures |

### 1.2 Where specs live

Resolved in order: the flow bundle's `nodes/`, installed packs (§17), the
engine's built-ins. First match wins at the highest `N` unless the flow
pins another.

---

## 2. Type and version

`ns/name@N`:

- `ns`: `^[a-z][a-z0-9-]*$`. `flo` is reserved for built-ins. A pack owns
  the namespaces it declares.
- `name`: `^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$`. A dot groups verbs
  (`conversation.list`, `ai.classify`, `task.create`).
- `N`: a positive integer, the **major version** of the node's contract.
  Within a major, a change may add optional inputs and add outputs. It
  MUST NOT remove or retype any input or output, tighten a schema, or
  widen `side_effect`, `privacy` or `capabilities`. Any of those is a new
  `N`.

A flow pins the major in `uses`. The record of each step names the exact
pack version that served it.

A node type is told from a sub-flow reference by the `/` before the `@`
(flow.md §5.2).

---

## 3. Inputs and outputs

### 3.1 InputDef

```yaml
inputs:
  query:  { type: [string, array], required: true, description: "…" }
  limit:  { type: integer, default: 10, minimum: 1, maximum: 100 }
  since:  { type: string, format: duration }
```

A JSON-Schema subset: `type` (one or a list of `string`, `integer`,
`number`, `boolean`, `array`, `object`, `null`), `required`, `default`,
`enum`, `minimum`, `maximum`, `items`, `properties`, `format` (`duration`,
`date-time`, `uri`), `description`. Input names: `^[a-z][a-z0-9_]*$`.
`required: true` with a `default` is an error.

Inputs reach the node as `in` (a map). Inside a node spec, templates see
`in`, `config`, `run` (read only; includes `run.step_dir`, §4.3) and, in a command or HTTP node, nothing
else: a node cannot read other steps' results or flow state. A node gets
its data only through `with`.

A flow's `with` values are validated against `inputs` at save time where
their type is known from the expression, and always at dispatch.

### 3.2 OutputDef

```yaml
outputs:
  from: body.results          # where the value comes from (§4.3, §5.3)
  format: json                # json | lines | text (command nodes)
  map: "{{ ... }}"            # optional reshaping, CEL over `out` (and `in`)
  fail_when: "{{ ... }}"      # optional: true fails the step, CEL over `out` (after `map`)
  schema: schemas/find.json   # JSON Schema file or inline map
```

`schema` is mandatory for declared nodes and describes the value a flow
reads as `results.<step>`. Outputs are validated against it at runtime; a
mismatch is `output_invalid`. A node MAY return extra fields; flows
SHOULD NOT depend on undeclared fields.

Plugin and built-in nodes declare `outputs.schema` the same way and,
because they are code, `outputs.from` and `map` do not apply.

### 3.3 Cost report

Any node that spends money or tokens reports it to the engine as part of
its result (`cost: {usd, tokens}`); declared nodes report a fixed
estimate (§13). It is part of the step record, not of `results`.

---

## 4. Declared command node

```yaml
node: gh/dependency-list@1
kind: command
lowers_to: exec
description: Packages the given repositories depend on.
argv: [gh, api, "repos/{{ in.repo }}/dependency-graph/sbom"]
env: {}
cwd: ""
stdin: { json: "{{ in.payload }}" }      # or { text: "…" }; optional
exit_codes: { ok: [0] }                   # default; others fail
inputs:  { repo: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  schema: { type: object, properties: { … } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
timeout: 30s
```

### 4.1 Execution

- The engine renders `argv` (each element a string; a placeholder that is
  one whole element is rendered to a string, a list-valued whole-element
  placeholder expands into multiple arguments), `env` values and `cwd`.
  Nothing is passed through a shell. Shell features are the author's
  problem: `argv: [sh, -c, "…"]` is allowed and is opaque to the engine.
- It runs as `aps run <run_as> -- <argv>`. The flow's `run_as` (or the
  step's) is the profile. The aps profile, not the node, supplies
  credentials and environment.
- `timeout` bounds the process; on expiry the process group is killed and
  the step fails with `timeout`, keeping partial output.
- `stdout` and `stderr` are each capped (default 1 MiB, `stdout_max`);
  excess is dropped and flagged.
- `exit_codes.ok` lists success codes (default `[0]`). A code in
  `exit_codes.retry` fails transiently (retried per `retry`); any other
  fails permanently. Engines read only the exit code and the parsed
  output: a wrapped CLI that exits non-zero for conditions that are not
  failures (a "not modified" answer, a client error that is data) is
  wrapped so it exits as documented here and reports the condition in its
  output.
- A `null` value renders as an empty string in `argv`, `env` and `cwd`.
- **Exit 0 is not success.** A tool may exit 0 and report failures in its
  output; `outputs.fail_when` (§3.2) turns such a report into a step
  failure (`node_reported_failure`, output kept in the record).

### 4.2 stdin

`stdin.json` renders the value as JSON text; `stdin.text` renders a
string. This is how a node passes structured input to a script without
temp files. Command nodes MUST NOT receive secrets through `argv` (they
appear in process listings); use `env` with a secret reference (§5.4).

### 4.3 Outputs from a command

`run.step_dir` is a scratch directory the engine creates per step
attempt and removes after the output is read. `file:<path>` is relative
to it.

`outputs.from`:

- `stdout` (default), `stderr`; or `file:<path>` (read after exit, path
  rendered, relative to `run.step_dir`); or `exit_code`.
- `format: json` parses the text as one JSON document; `lines` yields a
  list of strings (one per non-empty line); `text` the raw string.
- `map` (optional) then reshapes the parsed value. In `map`, `out` is the
  parsed value and the expression's value becomes the output. Example:
  `map: "{{ {'run': out.stamp, 'pending': out.items.filter(i, i.state == 'pending')} }}"`.
- Then `schema` validates.

Prefer `file:` over `stdout` for large or JSON outputs: a runner that
filters secrets from child output can alter a JSON document on stdout, and
runner status lines may share the stream. File output is read byte for
byte.

The raw exec result (`exit_code`, `stdout`, `stderr`, `duration_ms`,
`truncated`) is always kept in the step record; flows read the parsed
output as `results.<step>` and cannot read the raw one unless
`outputs.from: exec` (the whole exec result as the value).

---

## 5. Declared HTTP node

```yaml
node: ctxt/find@1
kind: http
lowers_to: engine
request:
  method: POST
  url: "{{ config.ctxt.url }}/api/v1/find"
  headers: { Accept: application/json }
  json: { query: "{{ in.query }}", limit: "{{ in.limit }}" }
  auth: { secret: ctxt-token, scheme: bearer }
response:
  success: [200]
inputs:  { query: { type: [string, array] }, limit: { type: integer, default: 10 } }
outputs: { from: body.results, schema: { … } }
side_effect: read
privacy: local
capabilities: [knowledge.read]
timeout: 10s
retry: { max_attempts: 3, backoff: 2s }
record: true
```

### 5.1 Request

`method`, `url`, `headers`, `query` (map), and one body key: `json`
(rendered, typed values preserved), `form`, or `text`. All string leaves
are templated against `in` and `config`.

### 5.2 Response

`response.success` lists acceptable status codes (default 200–299). A 429
or 5xx that is not listed fails **transiently** and is retried per `retry`
(honoring `Retry-After`); any other unlisted status fails permanently.
Redirects are followed for GET only.

### 5.3 Outputs from an HTTP call

`outputs.from` is a dotted path into `{status, headers, body}`, default
`body`. `body` is parsed as JSON when the response is JSON, else a string.
`map` and `schema` apply as in §3.2.

### 5.4 Secrets

`auth: { secret: <name>, scheme: bearer | basic | header:<Name> }` names a
secret the engine resolves from its secret store for the run's profile.
The secret is never rendered into the step record, the run log or a
cassette (§16). `url` and `headers` MUST NOT embed secrets.

---

## 6. Built-in nodes

A built-in node runs in the engine and has its spec shipped with it. The
phase-1 catalog is below; each spec follows §1. Only what the three
phase-1 flows need is normative in 0.1; the rest are reserved names.

| Node | Side effect | Privacy | Does |
|---|---|---|---|
| `flo/dedupe@1` | none | local | Remove items already seen, report the keys to remember |
| `flo/filter@1` | none | local | Keep items matching a condition |
| `flo/ai.classify@1` | none | by model | One label per item, with confidence and a reason |
| `flo/ai.generate@1` | none | by model | Text from a versioned prompt and a context |
| `flo/approval@1` | write-shared | local | Park until a person decides each item |
| `flo/output@1` | write-local | local | Publish a result to the brief and the run |
| `flo/context@1` | reserved | | per-subject context for a workflow (after phase 1) |
| `flo/propose@1` | reserved | | emit ready-to-run proposals instead of executing (after phase 1) |
| `flo/ai.extract@1`, `flo/ai.judge@1` | none | by model | typed extraction; typed judgments (choice, score, yes/no/abstain) (phase 1 triage port) |

There is no `log` node: every step emits start, end, inputs, outputs and
cost to the run log automatically. There is no `condition` or `for-each`
node: those are `when` and `for_each` on a step.

### 6.1 `flo/dedupe@1`

| Input | Type | Meaning |
|---|---|---|
| `items` | array, required | candidates |
| `key` | expression string, required | per-item key; `item` and `index` bound |
| `seen` | array, default `[]` | keys already seen |
| `max` | integer, default 10000 | keep at most this many keys, dropping the oldest |

Output: `{items: [the unseen items, input order], keys: [their keys],
seen: [seen ∪ keys, bounded by max]}`. The node is pure. The flow
commits `seen` through `outputs.state` (flow.md §7.4). Duplicate keys
within `items` count once, first wins.

### 6.2 `flo/filter@1`

Inputs `items` (array), `where` (expression string, `item` bound).
Output `{items}`.

### 6.3 `flo/ai.classify@1`

| Input | Type | Meaning |
|---|---|---|
| `items` | array, required | what to classify; each element a map |
| `labels` | array of string, required | the closed label set |
| `model` | string URI, required | `anthropic://…`, `ollama://…`, `openai://host:port/…` (provider chosen by kit's model client) |
| `instructions` | string | extra guidance |
| `pattern`, `strategy` | string | named prompt pattern / multi-call strategy, resolved through kit |
| `schema` | object | extra fields per item the model must return |
| `abstain` | boolean, default false | allow no label |

Output `{items: [{item, label, confidence, why, …schema fields}]}`, one
per input item, in order; `cost` reported. `label` is always in `labels`
(or null when `abstain`); the model call uses structured output and a
validated repair loop. Privacy follows the model URI scheme (§9). Empty
`items` returns `{items: []}` without a model call.

### 6.4 Built-in nodes that evaluate expressions per item

`flo/dedupe@1` and `flo/filter@1` take an expression string (`key`,
`where`) and bind `item` and `index` per element when they evaluate it.
This is the node's own iteration, not a `for_each`; no step records per
element are made.

### 6.5 `flo/ai.generate@1`

| Input | Type | Meaning |
|---|---|---|
| `prompt` | string `name@N`, required | a prompt file in the bundle's or pack's `prompts/` |
| `context` | object | variables for the prompt |
| `model` | string URI, required | as above |
| `schema` | object | optional output schema; validated with repair |

Output `{text, data?, prompt: {name, version, hash}}` (`data` present with
`schema`; `text` is never blank: a blank result fails
`node_reported_failure`). Prompts are files with a version, so a recorded run
names the prompt text it used by hash.

A prompt `name@N` is `prompts/name@N.yaml` in the flow bundle, else in an
installed pack that lists it. The file is YAML: `system:` (optional text) and
`template:` (required Go `text/template`; functions `join`, `trunc`,
`default` only; a missing key is an error). Backend `ai.generator`:
`template` renders the template over `context` (no model, no cost, no
`schema`); `llm` calls the model.

### 6.6 `flo/approval@1`

| Input | Type | Meaning |
|---|---|---|
| `items` | array of ApprovalItem, required | what needs a decision |
| `surface` | `tlc` \| `none` | where the human sees it (default `tlc`); the brief and `flo approval` always list it |
| `trust` | string | a trust provider name (§10.3) |
| `context` | object | passed to the trust provider |
| `verdicts` | array of `approve` \| `reject` \| `correct` | decisions a person may give; default all three. A flow limits it when the guarded tool cannot take a correction |
| `title` | string | task title |
| `timeout` | duration | optional; see below |
| `on_timeout` | `reject` | the only value in 0.1 |

ApprovalItem: `{id: string (stable, unique), action: string, why: string,
class?: string | string[], capability?: string, args?: object, …}`. `id`
should be stable across runs when the underlying thing is (a message id,
not a position). `class` is the trust action type, or a list when one item
carries several actions (`archive`, `label:Finance`); with a `trust`
provider the item is auto-approved when the provider says **every** class
is `auto`. Extra fields travel with the item to the provider and to the
output.
`capability` + `args` (the exact operation) is optional in phase 1 and
required for proposals later (§10.4).

Output:

```json
{ "approved": [item…], "rejected": [item…], "corrected": [item…],
  "auto": [item…], "reason": {"<id>": "…"} }
```

An item is in exactly one list. `corrected` carries the person's
replacement (`item.correction`). `auto` items were approved by trust
without a person. If every item is `auto` the node succeeds immediately
without parking. Otherwise the run parks (flow.md §7.1) until every item
has a decision (or `timeout`: undecided items are rejected).

Items in `approved` and `auto` are what the guarded step may act on.
The node itself writes only the decision record and the task.

### 6.7 `flo/output@1`

| Input | Meaning |
|---|---|
| `kind` | `findings` \| `brief` (phase 1) |
| `title` | string |
| `body` | string (`brief`) |
| `items` | array of Finding (`findings`): `{title, label?, detail?, url?, refs?}` |

Output `{id, kind}`. The value is written to the run record and listed by
`flo brief show` (retained 90 days). `kind` is closed in 0.1; `context`
is reserved.

---

## 7. Plugin nodes and trigger nodes

A plugin node is code implementing kit's extension interface, loaded
in-process in phase 1. It ships a node spec file next to its code; the
spec is the contract, the code is checked against it (inputs validated
before the call, outputs after). A plugin MUST NOT read state, other
steps' results or the run tree: it receives only `in`, `config` and the
run's identity.

A **trigger node** (`role: trigger`) is a plugin that produces events for
`on:`:

```yaml
node: calendar/events@1
kind: plugin
role: trigger
description: Upcoming calendar events, with start times, as VEVENTs.
config:
  url: { type: string, required: true }
  credentials: { type: string }
inputs:
  calendar: { type: string }                # which calendar; trigger options
outputs:
  schema:                                   # the payload of `trigger.event`
    type: object
    properties:
      uid: { type: string }
      recurrence_id: { type: [string, 'null'] }
      start: { type: string, format: date-time }
      end:   { type: string, format: date-time }
      title: { type: string }
      attendees: { type: array, items: { type: object, properties: { email: {type: string}, name: {type: string}, status: {type: string} } } }
side_effect: read
privacy: local
capabilities: [calendar.read]
```

A trigger node's `inputs` are the options in the `on:` entry. Its
outputs are events with a stable `id` (for the run key; flow.md §6.1) and
a payload. The engine owns timing; the node only reports what exists and
what changed (sync tokens), never when to fire.

Phase 1 trigger plugins: calendar (CalDAV, ICS) and the heo event watch.
Webhook and file triggers are phase 2.

---

## 8. Side-effect class

Every node declares one. The ladder (each level includes the ones above
it in risk):

| Class | Meaning | Examples |
|---|---|---|
| `none` | Pure compute. No external read or write. | dedupe, filter, a model call |
| `read` | Reads external state; changes nothing. | ctxt find, `gh api` GET, list conversations |
| `write-local` | Writes only to the engine's own records, or a path the run owns. | `flo/output` |
| `write-shared` | Changes state others can see, recoverably. | create a task, label a message, open an issue |
| `irreversible` | Cannot be undone, or its effects leave our control. | send a message, delete, unsubscribe, publish |

Rules:

- The class is declared by the node author and checked against grants:
  the capabilities (§12) must be consistent with it (a node listing
  `mail.send` and claiming `read` is invalid, `capability_mismatch`).
- A model call is `none` (it is pure from our side; its cost is separate).
- The class feeds dry-run and approval: under test and under `--dry-run`,
  `write-shared` and `irreversible` nodes do not execute; they return the
  plan of what they would do (their rendered request or argv) and an
  output synthesized from `examples` or an `outputs.dry_run` value
  (otherwise `null`).
- The vocabulary maps onto kit's tool side-effect annotation; where
  kit's names differ, kit's are authoritative for the CLI surface and the
  mapping is a table in the engine (open question 1).
- A core-kind step (`exec`, `agent`) has no spec and is treated as
  `write-shared` (flow.md §5.3).

---

## 9. Privacy

`privacy` says where a node's data goes.

| Value | Meaning |
|---|---|
| `local` | Inputs and outputs stay on this machine or its local network. Never placed on a remote runner. May run under a flow of either privacy. |
| `cloud` | Inputs or outputs reach a third-party service (a hosted API, a hosted model). Allowed only under a flow or step with `privacy: cloud-allowed`. |

Rules:

- A flow with `privacy: local-only` is refused if any node is `cloud`
  (flow.md V19).
- For AI nodes the level is derived from the `model` URI: `ollama://<name>`
  (the daemon on this machine; the name has no dot), `ollama://<host>/<model>` and
  `openai://<host>/<model>` with a local host, and an `ollama://` unix
  socket are `local`; hosted providers and any other host are `cloud`. An
  `ollama://<authority>` with no slash after it and a dot in the authority
  (`ollama://llama3.1`) is ambiguous (host or dotted model name) and is
  `cloud`; a local dotted model is written `ollama:///llama3.1` or
  `ollama://localhost/llama3.1`. A host
  is local when it is `localhost`, `*.local`, loopback (127.0.0.0/8, `::1`) or
  private (RFC 1918). The engine applies the same rule at run time: under a
  local-only ceiling a cloud model fails `privacy_violation` before any call. The node spec states the default; the validator resolves the
  actual value from `with.model` when it is a literal.
- A node whose data may not leave the machine (`local`) cannot be placed
  on a remote runner (placement check; §15).
- The declared level is the node author's claim. Engines MUST NOT infer a
  lower level than declared. `record` (§16) redacts auth, and a node MAY
  list `redact:` paths in its inputs and outputs that are dropped from
  cassettes and logs.
- Data lineage is not tracked in 0.1 (flow.md §7.6).

---

## 10. Approval, admission, trust

### 10.1 Approval requirement

`approval` is `none`, `required` or `trust`. If absent:

| Side effect | Default |
|---|---|
| `none`, `read`, `write-local`, `write-shared` | `none` |
| `irreversible` | `required` |

An `irreversible` node MAY NOT declare `approval: none` (invalid). A
node declaring `trust` says: no person is needed if the action's **trust
class** is at level `auto`; it MUST also declare `trust.class`
(a string, or a placeholder over `in`, e.g. `"{{ in.action }}"`).

The engine enforces approval twice:

- **Statically** (flow.md V20): an `irreversible` step needs an ancestor
  `flo/approval@1` step, or `trust: <provider>` on the step with a node
  that supports trust.
- **At dispatch:** the engine refuses to execute an `irreversible` step
  unless the run record holds an approved decision (or `auto` by trust)
  for it. Approval items of the guarding `flo/approval@1` step carry the
  stable `id`s the step operates on; a step acting on an item that was
  not approved gets no input for it (`approved` and `auto` are the lists
  to read).

### 10.2 Admission

Before every step that has a side effect above `read`, and before every
step that names a `run_as` other than the starter's, the engine asks the
**admission port**:

```text
(agent, principal, purpose, operation) → allow | deny | deferred + receipt
```

- `agent`: the `run_as` profile. `principal`: the person the run acts for
  (the flow's owner; phase 1: single tenant).
- `purpose`: the flow name and version and the step id.
- `operation`: the node type, the capabilities, the side-effect class and
  the **request digest** of the rendered inputs.
- `deferred` parks the run until the backend decides; `deny` fails the
  step with `admission_denied`.

The **request digest** is SHA-256 over the canonical JSON of
`{v, operation, scope, actor, acting_for, input}`, the same shape a
business-governance backend uses, so one request is recognized as the
same across tools. Phase 1's local port allows what the actor's grants
cover and records the receipt. (Whether canonicalization matches the other
backend byte for byte is unverified; open question 3.)

### 10.3 Trust

A **trust provider** is a pack-declared object, referenced by name
(`trust: inbox` on `flo/approval@1` or on a step):

```yaml
trust: inbox
kind: command
read:                                   # → [{class, level: ask|auto, streak, threshold, resets, last_reset}]
  argv: [inbox, trust, list, --format, json]
decide:                                 # once per approval, after every item has a decision
  argv: [inbox, approve, "{{ in.context.run }}", --decisions, "-", --format, json]
  stdin: { json: "{{ in.decisions }}" }
set:                                    # backs `flo trust set`
  argv: [inbox, trust, "{{ in.level == 'auto' ? 'promote' : 'reset' }}", "{{ in.class }}"]
```

- `read` supplies levels at approval time; items whose classes are all
  `auto` become `auto` items. A tool with finer levels maps them onto
  `ask` and `auto`.
- `decide` records the person's decisions with the system that owns the
  counters, so graduation and resets stay in one place. `in.decisions` is
  `[{id, verdict, reason?, item}]` plus whatever the item carried. It
  prints `{applied: [...], skipped: [...]}`; any `skipped` entry fails the
  approval step (`decision_not_recorded`) rather than being ignored. A
  rejection caused by cancelling a run is **not** reported to `decide`,
  so it does not reset trust.
- `set` backs `flo trust set` (promote or demote). A provider may refuse a
  level (`trust_capped`), e.g. for destructive classes.
- The wrapped tool owns the counters; the engine never writes them. That
  makes the provider the single writer only while every other entry point
  of the tool uses the same store under a lock. A tool with other writers
  needs that lock before flo relies on its counters (a script-side
  prerequisite, not engine behavior).
- Providers are `kind: command` or `http` with the same execution rules
  as §4–§5, and may use `env`, `cwd` and pack `config`. They are checked at
  install time to be at most `write-shared`.
- Later backends (a business-governance engine's graduated autonomy, an
  admission service) implement the same three operations behind the
  admission port; then the local provider is off.

### 10.4 Proposals

An approval item with `capability` and `args` is the exact operation a
person would approve. The engine's record of such an item is a
**proposal** (command, arguments, reason, approval chain, status),
shaped for the dek contract and keyed by the request digest. Phase 1
approvals work with or without `capability`/`args`.

---

## 11. Idempotency and retry

- `idempotent: true` means executing the node twice with the same
  inputs gives the same effect as once (reads, upserts, labelling).
  Default `true` for `none` and `read`, `false` for the rest.
- `idempotency_key: "{{ … }}"` (over `in`) names the key by which the
  *target system* recognizes a repeat. The engine passes it as the
  environment variable `FLO_IDEMPOTENCY_KEY` (command) or the
  `Idempotency-Key` header (HTTP) and stores it with the step record.
  The default key, when the node declares none but is `idempotent`, is
  the step's identity (run id, step id, element index).
- A non-idempotent node with no key MUST NOT be retried after an
  unknown outcome (timeout, crash, lost connection). The engine clamps
  `max_attempts` to 1 and, on an unknown outcome, marks the step
  `uncertain` and parks the run for a person (flow.md §7.9).
- A failure known not to have executed (spawn failure, connection
  refused, HTTP 429 or 503 declared safe) is retried regardless.

---

## 12. Capabilities

`<domain>.<verb>` strings (`knowledge.read`, `mail.send`, `vcs.read`,
`tasks.write`, `calendar.read`). The domain is a closed vocabulary owned
by the engine plus packs; the verb should name the strongest thing the
node can do (`mail.send`, not `mail.read`, for a node that can send).

- **Declared by the node, granted to the profile.** The profile named by
  `run_as` must hold every capability of every node the flow uses; a
  sub-flow runs within its parent's grants (the intersection).
- A grant check happens when someone edits or activates a flow, names a
  `run_as`, approves, installs a plugin, and at each step that acts for a
  principal.
- Phase 1: the owner's profile holds every capability; the engine's own
  profile holds only what installed packs declare.
- Capabilities map onto an operation namespace of an external governance
  system (`op:<namespace.Operation>`) without renaming.

---

## 13. Cost class

```yaml
cost: { class: metered, estimate: { usd: 0.002, tokens: 4000 } }
```

`class`: `free` (no money, negligible time), `low` (bounded money or
time per call), `metered` (money scales with input size), `expensive`
(an operator should notice). `estimate` is a per-call number the engine
uses before a step runs and the node corrects with the real figure
after. The static check (flow.md V21) and run-time budget check
(flow.md §7.7) use these. Declared nodes use fixed estimates; AI nodes
compute from the model client's price table and the input size.

### 13.1 Rate limits

`rate_limit: {bucket, max, per}` declares a quota the node draws on.
Nodes sharing a `bucket` name share the count. The engine counts calls
(including calls that fail with a client error, since providers may count
them) and, when the bucket is exhausted, delays the step up to its
`timeout` or fails it with `provider_rate_limited` (transient, exit 6).
A provider's own limit response is handled by `retry`.

---

## 14. Lowering

`lowers_to` says what a node becomes when a flow is *planned*:

- `exec`: the node, with its `with` values bound, expands into a plain
  `kind: exec` step with the rendered `argv`, `env`, `cwd`, `timeout`
  and `retry`. A plan-level tool can run it. Only `command` nodes with
  `stdin` omitted, `outputs.from: stdout` and `side_effect` ≤
  `write-shared` may lower to `exec`; everything else is `engine`.
- `engine`: needs the engine. A plan-level tool refuses it
  (`engine_node`; flow.md §9).

Declared HTTP nodes, built-ins and plugins are `engine`. A command node
that needs `stdin`, `map`, a trust provider or a check on its parsed
output is also `engine`.

---

## 15. Placement

```yaml
placement: { runner: local, isolation: none }
```

- `runner`: the runner class (a queue name). Phase 1: `local` only.
- `isolation`: `none` or `process` (phase 1); `container` and `pod`
  reserved.
- A node with `privacy: local` cannot be placed on a remote runner; a
  node's `cost.class` is compared with the budget at save. These are
  checked when a flow is saved, not when it runs.
- External execution (a node hands work to a system that runs it and
  waits) is a node pattern, not a placement.

---

## 16. Record and replay

`record: true` means the node's real interactions are captured as
cassettes during `flo flow test --record` (command: argv, env keys,
stdout, exit; HTTP: request and response; model calls: request and
response) and replayed deterministically in tests and dry runs. A node
that is `write-shared` or `irreversible` never executes during a
recording; its plan is recorded instead. Secrets and `redact:` paths are
removed from cassettes, the trigger payload, the state seed and the plans.
A declared command, http or plugin node with side effect `read` or
`write-local` runs for real in a recording, so `flo flow test --record`
refuses a flow with such a node unless `--allow-live` is given. A node with `record: false` is a validation error
for a flow that must be tested, unless it is pure (`none`).

---

## 17. Packs

A **pack** bundles what one tool or domain needs.

```yaml
pack: ctxt
version: 0.1.0
description: Knowledge reads from ctxt.
requires: { flo: ">=0.1" }
config:
  ctxt: { url: { type: string, required: true } }
nodes:    [nodes/find.yaml]
trusts:   []
prompts:  []
plugins:  []                    # code entries (phase 1: in process)
flows:    [flows/…]             # template flows
```

- `flo pack ensure <name@ver>` installs and is idempotent; `flo pack
  remove` removes. Packs are version-pinned in the project.
- A pack's `config` is set by the operator and read in node specs as
  `config.<pack>.<key>`.
- A pack owns its namespace(s). Two packs may not declare the same
  `ns/name@N`.
- The engine core imports nothing from any pack. A pack depends on its
  tool through a declared command, an HTTP API or a plugin, never the
  other way round.

---

## 18. Validation rules (node specs)

- **N01** `node`, `kind`, `inputs`, `outputs`, `capabilities`,
  `side_effect`, `privacy` present. **N02** type grammar (§2). **N03**
  `kind`-specific keys only (`argv` only for `command`, `request` only for
  `http`). **N04** every `{{ }}` in a declared node references only
  `in`, `config`, `run`. **N05** `outputs.schema` present for declared
  nodes. **N06** `approval` consistent with `side_effect` (§10.1).
  **N07** capabilities consistent with `side_effect` (§8). **N08**
  `trust` declared when `approval: trust`. **N09** `idempotency_key` only
  on nodes that can use one (`write-shared`, `irreversible`). **N10**
  `lowers_to: exec` only where §14 allows. **N11** `record: false` only on
  `none` nodes. **N12** secrets by reference only (§5.4). **N13**
  `examples` validate against `inputs` and `outputs`. **N14** `fail_when`
  and `map` reference only `out`, `in`, `config`, `run`. **N15** `rate_limit`
  `max` positive and `per` a duration.

---

## 19. Open questions

1. **Side-effect vocabulary.** The ladder is defined here; the exact
   names of kit's tool side-effect annotation were not verified. The
   mapping is left to the engine. Earlier drafts used `none` for a read
   call; `read` is introduced to separate pure compute from reads.
2. **`flo/approval@1` for step-level human decisions.** Core `human` and
   the node overlap for whole-step decisions; core `human` is kept for
   plan-level compatibility, the node for item-level decisions.
3. **Request digest canonicalization** against the external governance
   backend: not verified.
4. **Declared node output parsing under `lowers_to: exec`.** §14
   restricts lowering to nodes that need no parsing beyond `stdout`; a
   plan-level tool does not parse.
5. **A `dry_run` output** (`outputs.dry_run`) is introduced for
   side-effect nodes' dry runs; whether kit's side-effect layer already
   carries a plan shape that should replace it.
6. **Trust provider scope.** One provider per class family; per-flow vs
   per-tenant counters when providers are not wrapped scripts.
7. **Subprocess plugin protocol** is a later kit addition; plugin
   in-process contract may need tightening before then.
8. **Node versions beyond a major:** whether minor versions are tracked in
   the spec or only in the pack.
9. **Approval `timeout`:** `reject` is the only safe default; whether
   some flows want `approve` on timeout (as core `human` allows) is left
   out of 0.1 for irreversible work.
