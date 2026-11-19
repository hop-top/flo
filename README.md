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
- Identity: `FLO_PROFILE`, else `owner_profile` of the config, else `$USER`;
  tenant `FLO_TENANT`, else `tenant`, else `default`. The owner profile
  (`owner_profile`, else `$USER`) holds the wildcard grant; the engine's
  profile (`FLO_ENGINE_PROFILE`, default `flo`) holds what installed packs
  declare. Packs are read when a process starts.
- Config: one loader for every command and `flo engine serve`, kit's `-c`
  convention (as tlc and aps). Precedence, highest first: `-c key=value`,
  `FLO_*` environment, config files, default. Files merge in order
  `/etc/flo/config.yaml`, `$XDG_CONFIG_HOME/flo/config.yaml` (kit's xdg
  default when unset: `~/.config/flo` on Linux, `~/Library/Application
  Support/flo` on macOS), then each `-c <path>` (later over earlier; a missing `-c` path is an error, a missing
  default file is skipped). `FLO_CONFIG` is `-c <path>` in the environment. A
  key's variable is `FLO_` + the key upper-cased, dots as underscores
  (`packs.ctxt.url` is `FLO_PACKS_CTXT_URL`); the one exception is
  `engine.approval_poll`, which is `FLO_APPROVAL_POLL`. `flo status` shows
  the merged values (`effective-config`) and the files read (`config`).
  Secrets are never read from a config file or this loader: they stay in the
  secret store (`FLO_SECRET_<TENANT>_<PROFILE>_<NAME>`).

  ```yaml
  # $XDG_CONFIG_HOME/flo/config.yaml
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
  ai: { classifier: rules, generator: template, judge: { engine: auto } }  # FLO_AI_*
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
  `strategy`, with a `notes` entry in the step output. When none is usable the error lists each model and why.
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
  host or params is refused at load.

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
  on a host whose ollama has the model.
- `packs/inbox` holds the nodes of `flows/inbox-triage/` over the `inbox`
  command, and the `inbox` trust provider. The nodes run `inbox` and the
  wrappers in `wrappers/` (`inbox-plan`, `inbox-trust`, `inbox-approve`; they
  need `jq`) as the flow's profile, so both go on that profile's `PATH`,
  with the script's credentials, `INBOX_HOME` and `INBOX_RUNS`. The trust
  provider runs on the engine's own `PATH` and environment, which must name
  the same directories. `inbox/lanes@1` acts on mail outside the plan and is
  not used by the flow. Re-record the flow's `record/` with
  `FLO_E2E_RECORD=1 go test ./tests/e2e -run TestRecordInboxTriage` after a
  change to the flow, its tests or its nodes.
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
