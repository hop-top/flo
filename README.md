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
flo flow test dependency-watch --record --yes  # reads run once for real, cassettes in record/
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
  `plans.json`). Pass: run succeeded, outputs match node specs, `check:`s pass,
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
- `packs/github` holds the GitHub nodes of `flows/dependency-watch.yaml`. Its
  wrappers (`wrappers/gh-deps`, `gh-advisories`, `gh-releases`, `gh-lib.sh`) need
  `gh` and `jq` and must be on the `PATH` of the profile the flow runs as.
- `flo status` reports the engine: running, stopped, or stale after a crash.
- Tests: `go test ./tests/e2e` builds the binary and runs it against fake
  `aps`, `gh`, `tlc` and `inbox` executables on `PATH`; nothing reaches a
  real tool.
