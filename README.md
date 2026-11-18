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
flo flow apply -f flows/dependency-watch       # new version only when content changed
flo flow set dependency-watch --state active --yes
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
- `flo status` reports the engine: running, stopped, or stale after a crash.
- Tests: `go test ./tests/e2e` builds the binary and runs it against fake
  `aps`, `gh`, `tlc` and `inbox` executables on `PATH`; nothing reaches a
  real tool.
