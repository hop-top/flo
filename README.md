# flo

```sh
make build
./bin/flo status
```

## Running flows

State lives under `$XDG_DATA_HOME/flo`: one SQLite file per tenant, a shared
jobs database, one JSON Lines log per run, installed packs in `packs/<name>/`.
The CLI writes the database directly; `flo engine serve` claims the queued
jobs and advances them.

```sh
flo grant set automation vcs.read --yes        # the profile the flow runs as
flo flow validate -f flows/dependency-watch    # a bundle: flow.yaml + nodes/
flo flow apply -f flows/dependency-watch --yes # new version only when content changed
flo flow test dependency-watch --record --yes --allow-live  # reads run once for real, cassettes in record/
flo flow test dependency-watch                 # replay: offline, writes only planned
flo flow set dependency-watch --state active --yes   # needs a passing test of this version
flo flow draft create dependency-watch "also watch cli/cli releases"   # a model proposes the next version
flo flow draft show <draft>                    # proposal, attempts, dry run on the last recording
flo flow draft create --from <draft> "also run at 6"   # refine a draft; --new <name> drafts a new flow
flo flow draft apply <draft> --yes [--write]   # next version, unless the flow moved (draft_stale) or the dry run fails (--force)
flo trigger list                               # next fire of each trigger
flo engine serve &                             # SIGTERM drains running steps
flo run create --flow dependency-watch --wait  # 0 done, 7 awaiting approval
flo run show <run>                             # step records, approvals, outputs
flo run logs <run> --follow                    # CloudEvents, JSON Lines
flo approval set <approval> --item 1=approve --yes
flo run cancel <run> --yes
```

- `run create`: `--var name=value` (typed per the flow), `--var-file`,
  `--idempotency-key` (same key, same run), `--trigger-file` (trigger payload),
  `--version`, `--wait [--timeout]`. `--wait` needs a serving engine.
- `flow test <flow>`: runs a stored version once in a sandbox (nothing reaches
  the real stores). Writes (`write-shared`, `irreversible`) are not executed:
  their plans are recorded; approvals are auto-approved. Replay (default)
  answers processes, HTTP and eva checks from the xrr cassettes in the bundle's
  `record/cassettes/`; a call without one fails the test. `--record` makes the
  reads for real and writes `record/` (`cassettes/`, `trigger.json`,
  `plans.json`, `state.json`), scrubbed as a run recording is (node `redact:`
  paths, secrets, credential headers, env values; bundles may be committed).
  It refuses when a declared command, http or plugin node with `side_effect`
  read or write-local would reach real systems, unless `--allow-live`: point
  the node's profile env at temp dirs first (`--dry-run` lists those nodes). Pass: run succeeded, outputs match node specs, `check:`s pass,
  every `expect:` of `tests/*.yaml` holds. `--trigger-file`, `--var`,
  `--version`, `--timeout`, `--state default|current|<file>` (the flow state
  the run starts from; `record/state.json` is kept by `--record` and reused by
  replay; a `state:` map in a `tests/*.yaml` seeds that case), `--dry-run`
  (plan only). Exit 1 when the test fails. The latest result of a
  version (flow hash + bundle digest) is what activation reads
  (`flow_not_tested`, exit 2). `apply` and `test --record` ask for
  confirmation (`--yes`).
- `flow draft`: the chat builder. `create <flow> <instruction>` (or `-`, the
  instruction on standard input) gives a model the flow's active version (else
  the newest) and the nodes it may use, and gets the full next version back.
  Validation errors go back to the model up to three times, then the command
  fails with `invalid_flow` and the last errors. The call is flo's own model
  path: `ai.models.allow` and `deny` apply, a cloud model's key is read from the
  secret store as the calling profile, and the flow's `privacy` ceiling holds (a `local-only` flow is
  sent to a local model only; nothing is sent when refused). The model is
  `--model`, else `ai.builder.model` (env `FLO_AI_BUILDER_MODEL`), else
  `ollama://qwen3:4b-instruct-2507-q4_K_M`. A valid proposal is replayed offline on the flow's
  last recording and the outcome differences are stored with the draft (none
  when the flow has no recording). A draft is not a version: `show`, `list`,
  `discard` read and delete it, `apply` stores it as the next version (a draft
  version; test and activate it as usual) only if the flow's base and newest
  versions are those the draft was made on, else `draft_stale` (exit 4) and the
  draft is kept. `apply` replays once more and refuses with
  `draft_dry_run_failed` (exit 4) when a step failed or ended with another
  status or a call has no recording, unless `--force`; a different output only
  reports, and no recording means nothing to compare, so it applies.
  `apply --write` also rewrites the bundle's `flow.yaml` (atomically, same
  mode), refused with `draft_stale` when the file changed since the draft.
  `create --new <name> <instruction>` drafts a flow that does not exist yet
  (free name; no recording, so no dry run; the model call is local-only unless
  `--privacy cloud-allowed`; `apply --write` makes `<dir>/<name>/flow.yaml`,
  `--dir`, else the `flows.dir` setting, default `flows`; refused if it exists). `create --from <draft>
  <instruction>` refines a draft: the model gets the original base, the draft
  and the instructions so far; the base stays the original version (so
  `draft_stale` as before), the new draft links to its `parent`, and the
  parent is kept. Drafts live in `<tenant dir>/drafts/<id>.json`. The recorded
  test of a build on a local model (`internal/engine/serve`,
  `TestDraftOnALocalModelTape`) is re-recorded with `FLO_BUILDER_RECORD=1` on a
  host that runs ollama.
- Identity: `FLO_PROFILE`, else `owner_profile` of the config, else `$USER`;
  tenant `FLO_TENANT`, else `tenant`, else `default`. The owner profile
  (`owner_profile`, else `$USER`) holds the wildcard grant; the engine's
  profile (`FLO_ENGINE_PROFILE`, default `flo`) holds what installed packs
  declare. Packs are read when a process starts.
- Config: one loader for every command and `flo engine serve`, kit's `-c`
  convention (as tlc and aps). Precedence, lowest to highest: default,
  `/etc/flo/config.yaml`, `$XDG_CONFIG_HOME/flo/config.yaml` (kit's xdg
  default when unset: `~/.config/flo` on Linux, `~/Library/Application
  Support/flo` on macOS), project config (below), each `-c <path>` (later over
  earlier; a missing `-c` path is an error, a missing default file is
  skipped), `FLO_*` environment, `-c key=value`. `FLO_CONFIG` is `-c <path>` in the environment. A
  key's variable is `FLO_` + the key upper-cased, dots as underscores
  (`packs.ctxt.url` is `FLO_PACKS_CTXT_URL`); the one exception is
  `engine.approval_poll`, which is `FLO_APPROVAL_POLL`. `flo status` shows
  the merged values (`effective-config`) and the files read (`config`).
  Secrets are never read from a config file or this loader: they stay in the
  secret store (`FLO_SECRET_<TENANT>_<PROFILE>_<NAME>`).
- Project config: the nearest `.flo/config.yaml` (else `.flo.yaml`) in the
  working directory or an ancestor. Only the nearest file is read; a farther
  one is ignored. The walk stops at `$HOME`: a config above it is never read,
  and one in `$HOME` is read only when `$HOME` is the working directory. It
  sits under the `-c` files and the `FLO_*` environment, so either beats it.
  It can set any key but these, which fail the load: a key with `secret`,
  `token`, `passw`, `credential`, `api_key`, `private_key` or `access_key` in a
  segment; `owner_profile`, `engine_profile`, `privacy.local_hosts` and `bos.*`
  (user config, environment or `-c` only); a key not in lower case. The model
  policy only narrows what the user and system files set: `ai.models.deny` is
  the union of the lists; `ai.models.allow` keeps the entries both lists allow
  (with no list below, the project's stands). A project allow list that
  leaves no allowed model fails the load. `flo status` shows the file read
  (`project_file`).
- Flows directory: `flows.dir` (`FLO_FLOWS_DIR`), default `flows`: where
  `flow draft apply --write` puts a new flow's bundle (`<dir>/<name>/`), unless
  `--dir`. A relative value is relative to the directory holding the project
  config (`.flo/` or `.flo.yaml`'s parent) when the project config supplies
  it (no `-c` file, `FLO_FLOWS_DIR` or `-c flows.dir=` beats it), else to the
  working directory. `flo status` shows the effective directory
  and its source (`flows_dir`, `flows_dir_from`).

  ```yaml
  # $XDG_CONFIG_HOME/flo/config.yaml (and the project's .flo/config.yaml)
  flows: { dir: flows }                         # FLO_FLOWS_DIR
  tenant: default
  owner_profile: jad            # FLO_OWNER_PROFILE
  engine_profile: flo           # FLO_ENGINE_PROFILE
  tlc: { dir: /path, profile: p }               # FLO_TLC_DIR, FLO_TLC_PROFILE
  engine:                                       # FLO_ENGINE_*, FLO_APPROVAL_POLL
    pollers: 4
    interval: 1s
    claim_ttl: 2m
    shutdown_grace: 30s
    approval_poll: 1m
  ai: { classifier: rules, generator: template, judge: { engine: auto }, builder: { model: "ollama://qwen3:4b-instruct-2507-q4_K_M" } }  # FLO_AI_*
  eva: { url: http://127.0.0.1:9000 }             # FLO_EVA_URL
  packs:                        # config.<pack>.<key> in node specs and triggers
    ctxt: { url: http://127.0.0.1:8080 }          # FLO_PACKS_CTXT_URL
    calendar: { url: /path/work.ics, sync_interval: 10m }
  ```

  Pack keys are flat: `packs.<pack>.<key>`; as a variable, the first `_`
  after `FLO_PACKS_` ends the pack name, so a hyphenated pack name is set in a
  file or with `-c` only.
- Classifier: `ai.classifier` (env `FLO_AI_CLASSIFIER`) picks the backend of
  `flo/ai.classify@1`: `rules` (deterministic, no model; security by advisory,
  breaking by major bump, else routine) or `judge` (kit go/ai/judge over the step's
  model: one choice question per item; confidence and reason from the model;
  tokens and, for a cloud model, dollars count against the run's budget;
  no `schema`). It reads the same model client rules as the generator. Unset configures none and a step using the node fails
  with `engine_feature`: no backend stands in for another.
- Judge engine: `ai.judge.engine` (env `FLO_AI_JUDGE_ENGINE`; `local`, `jev`
  or `auto`, default `auto`) picks what answers the `judge` classifier; a
  classify step's `engine:` overrides it. `local` is the step's non-Jev
  models. `jev` is TypeSafe's Jev through OpenRouter: cloud, key from the
  secret `openrouter_api_key` of the flow's profile (sent to Jev's endpoint
  only), refused under `local-only`. `auto` runs the step's models as listed;
  a single `ollama://` model that is local and listed by the daemon
  (`GET /api/tags`) answers, else `jev` (never under `local-only`:
  `privacy_violation`). An explicit cloud model runs as named, never swapped
  for Jev. The step output's `engine` and `model` say which judged. A flow
  whose recording holds local model calls only pins `engine: local`.
- Model lists: a step's `model:` is a string or an ordered list, for
  `flo/ai.classify@1` and `flo/ai.generate@1`:
  `model: [ollama://qwen3:4b-instruct-2507-q4_K_M, jev]`. The first usable
  model answers: permitted by the allow and deny lists, allowed by the step's privacy ceiling, and
  available (a local ollama model the daemon lists, even a lone one: a missing
  model fails as not detected, `ollama pull <model>`; a cloud model or `jev`
  with its key stored). Only unavailability falls through; a call that fails on the
  chosen model is reported, not retried on the next. `jev` is the default Jev
  model (`jev://typesafe/jev-1.14`) and is classify only; `engine:` filters the
  list. Jev takes a step's `pattern` text first in its instructions and ignores
  `strategy`, with a `notes` entry in the step output (the value is not
  validated there: even a malformed one is only noted). When none is usable the error lists each model and why.
- Model allow and deny lists: `ai.models.allow` and `ai.models.deny` (env
  `FLO_AI_MODELS_ALLOW`, `FLO_AI_MODELS_DENY`, comma-separated) say which
  models flows may run, cloud or local, on every engine. Deny wins. Neither
  set: every model. Only allow: the listed ones. Only deny: all but the
  denied. Both: allowed and not denied. A refused model fails with
  `privacy_violation` before any request or secret read. Entries: `*` (any
  model), `jev`, or a model URI whose model name may be a glob (`*`, `?`,
  `[...]`): `ollama://qwen3*`, `ollama://qwen3:*-instruct-*`,
  `jev://typesafe/*`, `ollama://localhost:11434/*`. Entries are matched as
  parsed URIs: scheme, host and `base_url` must equal the model's, and a glob
  covers the model name only, never a host or a query; a glob in the scheme,
  host or params is refused at load. Deny is looser on purpose: it matches the
  model name at any endpoint and scheme (`qwen3*` or `ollama://qwen3*` also
  denies `ollama://otherhost:11434/qwen3:4b` and `?base_url=…`), so a model
  cannot be moved out of it; entries may be bare names (`qwen3*`). Deny fails
  safe: odd names (`/` segments, `@`, `..`) over-deny rather than slip through.

  ```yaml
  ai:
    classifier: judge
    models:
      allow:
        - ollama://qwen3*
        - ollama://gemma4:e4b
        - jev
      deny:
        - ollama://*-base-*
  ```
- Generator: `ai.generator` (env `FLO_AI_GENERATOR`) picks the backend of
  `flo/ai.generate@1`: `template` (renders the prompt's template over the
  context: no model, no cost) or `llm` (calls the step's model through kit's
  model client: prompt `system` text then the rendered template as messages;
  `schema` gives validated `data`; tokens and, for a cloud model, dollars
  count against the run's budget). The `llm` client reads no environment
  variable and no `llm.yaml`; its requests go through the engine's HTTP seam.
  A cloud model's API key is the secret `<scheme>_api_key` of the flow's
  profile (`anthropic_api_key`); it is sent only to the scheme's default
  endpoint, never to a host or `base_url` the model URI names. This build
  links the `ollama` scheme only. Unset configures none and a step using the
  node fails with `engine_feature`. The `llm` backend runs only models the
  allow and deny lists permit. A prompt `name@N` is `prompts/name@N.yaml` in the flow
  bundle, else in an installed pack: YAML with `system:` and `template:` (Go
  text/template; funcs `join`, `trunc`, `default`; a missing key is an
  error). The ceiling of the step (`privacy`) refuses a cloud model with
  `privacy_violation`; under `local-only` the `llm` client is also resolved
  local-only, so kit enforces the same refusal at the endpoint.
- Local hosts: `privacy.local_hosts` (env `FLO_PRIVACY_LOCAL_HOSTS`,
  comma-separated host names and CIDRs; default none) lists hosts a model URI
  may name under a local-only ceiling beyond localhost and private addresses.
  Tailnet (100.64.0.0/10, `*.ts.net`) and named hosts are cloud unless listed.
  Model URIs: `ollama://<model>`, `scheme://host:port/<model>`, `?base_url=`
  (which wins over the host); `ollama:///x`, `ollama://localhost/x` and unix
  sockets are refused (node.md §9).
- `packs/github` holds the GitHub nodes of `flows/dependency-watch/`. Its
  wrappers (`wrappers/gh-deps`, `gh-advisories`, `gh-releases`, `gh-lib.sh`) need
  `gh` and `jq` and must be on the `PATH` of the profile the flow runs as.
  The flow labels new releases with the `judge` classifier on the local model
  `ollama://qwen3:4b-instruct-2507-q4_K_M` (set `FLO_AI_CLASSIFIER=judge` and
  `FLO_AI_MODELS_ALLOW=ollama://qwen3*`;
  Gemma 4 E4B is the target once ollama serves structured output for it).
  `flows/dependency-watch/record/` holds a recording, model calls included:
  `flo flow test dependency-watch` replays it offline. Re-record with
  `FLO_E2E_RECORD=1 go test ./tests/e2e -run TestDependencyWatchCommittedRecording`
  on a host whose ollama has the model. `tests/watch.yaml` pins the judge's
  engine, model and per-release labels from the tape: a re-recording that
  labels differently fails the replay until the pin is updated with it.
- `packs/inbox` holds the nodes of `flows/inbox-triage/` over the `inbox`
  command, and the `inbox` trust provider. The nodes run `inbox` and the
  wrappers in `wrappers/` (`inbox-plan`, `inbox-trust`, `inbox-approve`,
  `inbox-rows`, `inbox-decisions`; they need `jq`; `inbox/senders@1` runs
  `inbox senders RUN --json`) as the flow's profile, so
  both go on that profile's `PATH`, with the script's credentials,
  `INBOX_HOME` and `INBOX_RUNS`. The trust provider runs on the engine's own
  `PATH` and environment, which must name the same directories.
  `inbox/lanes@1` acts on mail outside the plan and is not used by the flow;
  `inbox/decide@1` is the script's own hosted decider, which the flow no longer
  uses. Re-record the flow's `record/` with
  `FLO_E2E_RECORD=1 go test ./tests/e2e -run TestRecordInboxTriage` on a host
  whose ollama has `qwen3:4b-instruct-2507-q4_K_M`, after a change to the flow,
  its tests or its nodes.
- `flows/inbox-triage` is the mail triage, every 15 minutes: `inbox/fetch@1`,
  then the judgment, then the script's plan, an approval with the script's
  trust, and `inbox/apply@1`. The judgment is two `flo/ai.classify@1` steps
  over the headers of each undecided message (`inbox/rows@1`: sender, subject,
  date, flags, folder, sender history; never a body): `label` files it under
  one of the labels (var `labels`, the script's SOP labels; it may abstain),
  `weigh` says how much it matters (irrelevant to critical).
  `inbox/senders@1` asks the script who each sender is (contact, muted, ruled
  out, person-looking, org, customer vendor and whether its mail authenticates,
  auth-mail class; its local store, no body, no model).
  `inbox/decisions@1` turns both into the script's decisions file by the
  script's own policy: a label under 0.9 confidence is Needs Review, an
  abstention stays in the inbox for a person, a message that matters little is
  also read and archived, mail from a contact or a person-looking sender is
  never filed away unless the script mutes it or it was ruled out; mail from a
  customer vendor (a signup and a receipt for the same address) never gets a
  delete action; a login code, reset or magic link is treated as unrequested
  and labelled `security` and archived (var `security_label`) only for a
  customer vendor whose mail authenticates, else kept in the inbox, flagged;
  mail claiming a customer vendor that fails authentication is flagged too. A
  row without facts falls back to the address. Nothing is ever trashed, replied
  to or unsubscribed. Setup: set
  `FLO_AI_CLASSIFIER=judge` and `FLO_AI_MODELS_ALLOW=ollama://qwen3*`; the
  engine (`ai.judge.engine`, default `auto`) is configuration. **Privacy:** the
  flow and both steps are `local-only`; the model list is
  `[ollama://qwen3:4b-instruct-2507-q4_K_M, jev]`, and under that ceiling the
  engine skips Jev. With no local model detected the step fails
  (`privacy_violation`); mail is never sent to Jev unless the owner opts in:
  set `privacy: cloud-allowed` on the flow and on `label` and `weigh`, put the
  secret `openrouter_api_key` on the profile, and allow `jev` in
  `ai.models.allow`. Each approval item names the operation (`capability`
  `mail.write`, or `mail.send` for a reply, and its `args`), so the engine
  keeps a proposal under a request digest; trust is the script's, read and
  recorded through the `inbox` provider, never written by flo. Left for the
  script: `fetch`, `plan`, `apply` (the executor, with its ledger), the trust
  counters, sender lanes, contacts, mutes and the check that a label exists in
  an account (a missing one is an invalid plan item). Tests replay the
  committed recording offline (synthetic mail, answers of the local model);
  the live scenarios run against a fake model.
- Choosing a judge for mail: `go run ./cmd/judge-eval` compares judges on a
  recorded day over kit's `judge/eval`. `import --step label|weigh --out SET
  RUN_DIR...` turns run folders of the script into a case set whose references
  are the verdicts its hosted decider (Jev) recorded: no model is called, and
  the set must be written outside any git work tree (it is real mail; the tool
  refuses otherwise). `run --cases SET --model ollama://[host:port/]<model>`
  replays it through a local candidate and prints the report and a gate
  (`--min-agreement`, `--min-coverage`, `--at-confidence`, `--min-n`,
  `--max-ece`; exit 3 on FAIL). `--jev --allow-cloud` makes Jev the candidate
  (key in `OPENROUTER_API_KEY`) and sends the set to it. `record` writes a
  candidate's answers as the references, to replay another judge against them.
- `packs/ctxt`, `packs/heo` and `packs/tlc` hold the read nodes of the meeting
  brief. `ctxt/find@1` posts to `{config.ctxt.url}/api/v1/find`: the server
  `ctxt find` uses (`server.url` or the first `server.urls` entry of the ctxt
  config, else `http://127.0.0.1:8080`); a protected or public instance also
  needs its token as the secret `ctxt_token` of the flow's profile (optional:
  unset, no `Authorization` header is sent). The
  `heo` pack runs `wrappers/heo-conversations` (needs `heo`, its daemon and
  `jq`; stand-in for a participant filter heo lacks); `tlc/task.list@1` runs
  `tlc task list --all-projects`. Their capabilities are `knowledge.read`,
  `mail.read` and `tasks.read`.
- `flows/meeting-brief` is the meeting brief: a calendar trigger 30 minutes
  before each event with more than one attendee, a search of ctxt per attendee
  (by name, narrowed to notes naming them, else by address), the open heo
  conversations with them, the open tlc tasks that match the title, and `flo/ai.generate@1`
  over the prompt `meeting-brief@1` into `flo brief show`. It is `local-only`
  with the model `ollama://gemma4:e4b`. A source that is down (ctxt, heo, tlc)
  gives its section the line `source unavailable: <source>` and the brief is
  still produced; the three retry once at most, so a brief waits seconds, not
  minutes. The facts handed to the model are cut to the var `context_chars`
  (default 6000; `--var context_chars=N`). The `template` backend renders the prompt
  (`ai.generator: template` or `FLO_AI_GENERATOR=template`); `llm` runs it (on a model the allow and deny lists permit) on
  the model. To run
  it: set `packs.ctxt.url` and `packs.calendar.url` (config file,
  `FLO_PACKS_*` or `-c`), the secret `ctxt_token` when ctxt needs one, grant
  `knowledge.read`, `mail.read`, `tasks.read` and `calendar.read` to the
  flow's profile, put `heo`, `tlc` and `jq` on that profile's `PATH`, keep
  the heo daemon up, then `flo flow apply`, `flo flow test --record` (with a
  `--trigger-file` of an event), `flo flow set --state active`. Re-record
  `record/` with `FLO_E2E_RECORD=1 go test ./tests/e2e -run
  TestMeetingBriefCommittedRecording` after a change to the flow or its
  nodes.
- `flo status` reports the engine: running, stopped, or stale after a crash.
- Tests: `go test ./tests/e2e` builds the binary and runs it against fake
  `aps`, `gh`, `heo`, `tlc` and `inbox` executables on `PATH`; nothing reaches a
  real tool.
