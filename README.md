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
- Engine settings: `FLO_ENGINE_POLLERS`, `FLO_ENGINE_INTERVAL`,
  `FLO_ENGINE_CLAIM_TTL` (how long a killed engine's runs wait before another
  engine takes them), `FLO_ENGINE_SHUTDOWN_GRACE`, `FLO_APPROVAL_POLL`,
  `FLO_TLC_DIR`, `FLO_TLC_PROFILE`, `FLO_EVA_URL`. Secrets of HTTP nodes come
  from `FLO_SECRET_<TENANT>_<PROFILE>_<NAME>`.
- Classifier: `ai.classifier` (env `FLO_AI_CLASSIFIER`) picks the backend of
  `flo/ai.classify@1`: `rules` (deterministic, no model; security by advisory,
  breaking by major bump, else routine) or `judge` (kit go/ai/judge; refused
  until it exists). Unset configures none and a step using the node fails
  with `engine_feature`: no backend stands in for another.
- Generator: `ai.generator` (env `FLO_AI_GENERATOR`) picks the backend of
  `flo/ai.generate@1`: `template` (renders the prompt's template over the
  context: no model, no cost) or `llm` (refused until a model client lands).
  Unset configures none and a step using the node fails with
  `engine_feature`. A prompt `name@N` is `prompts/name@N.yaml` in the flow
  bundle, else in an installed pack: YAML with `system:` and `template:` (Go
  text/template; funcs `join`, `trunc`, `default`; a missing key is an
  error). The ceiling of the step (`privacy`) refuses a cloud model with
  `privacy_violation`.
- `packs/github` holds the GitHub nodes of `flows/dependency-watch.yaml`. Its
  wrappers (`wrappers/gh-deps`, `gh-advisories`, `gh-releases`, `gh-lib.sh`) need
  `gh` and `jq` and must be on the `PATH` of the profile the flow runs as.
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
  needs its token as the secret `ctxt_token` of the flow's profile. The
  `heo` pack runs `wrappers/heo-conversations` (needs `heo`, its daemon and
  `jq`; stand-in for a participant filter heo lacks); `tlc/task.list@1` runs
  `tlc task list --all-projects`. Their capabilities are `knowledge.read`,
  `mail.read` and `tasks.read`.
- `flo status` reports the engine: running, stopped, or stale after a crash.
- Tests: `go test ./tests/e2e` builds the binary and runs it against fake
  `aps`, `gh`, `heo`, `tlc` and `inbox` executables on `PATH`; nothing reaches a
  real tool.
