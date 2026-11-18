# Worked examples

Three flows that share nothing but the engine, written against
[flow.md](flow.md) and [node.md](node.md): a dependency watcher, a meeting
brief and inbox triage. Each is followed by the node specs it uses. Names,
repositories and profiles are placeholders. The commands wrapped by the
triage nodes (`inbox …`) stand for any script with the same contract.

The GitHub and inbox node specs follow what probing the real calls and the
real script showed (response shapes, rate limits, exit codes, side effects),
including the changes a wrapped script needs. Where a spec asks for more
than a tool does today, the example lists it as a prerequisite.

## 1. dependency-watch

Every morning: list what the repositories depend on (deduplicated across
repositories), check them against advisories, fetch releases since the last
run, drop what was seen, classify, publish findings, and offer to open an
upgrade task for the serious ones.

```yaml
flow: dependency-watch
version: 0.1.0
description: New releases and advisories for what our repos depend on.
on:
  - schedule: { rrule: "FREQ=DAILY;BYHOUR=7", tz: America/Toronto }
  - manual: {}
vars:
  repos:
    default: [example-org/api, example-org/web, example-org/worker]
    type: list
  lookback: { default: 14d, type: duration }
run_as: automation
privacy: cloud-allowed          # only public package data leaves the machine
budget: { usd: 0.50 }
tasks: none                     # the approval below is the only human surface
state:
  seen:     { default: [] }     # package@version of releases already reported
  seen_adv: { default: [] }     # advisory@package@version already reported
  cursor:   { default: null }
steps:
  - id: deps
    uses: gh/dependency-list@1
    with: { repos: "{{ vars.repos }}", mode: sbom }

  - id: advisories
    uses: gh/advisories@1
    with:
      packages: >-
        {{ results.deps.packages.map(p,
           {'ecosystem': p.ecosystem, 'name': p.name, 'version': p.version,
            'used_by': p.used_by}) }}

  - id: newadv
    uses: flo/dedupe@1
    with:
      items: "{{ results.advisories.advisories }}"
      key: "{{ item.ghsa_id + '@' + item.package + '@' + item.version }}"
      seen: "{{ state.seen_adv }}"
      max: 5000
    outputs:
      state: { seen_adv: "{{ result.seen }}" }

  - id: releases
    uses: gh/releases-since@1
    for_each: "{{ results.deps.packages.filter(p, has(p.source_repo)) }}"
    concurrency: 4
    on_item_error: skip
    with:
      source_repo: "{{ item.source_repo }}"
      package: "{{ item.name }}"
      version: "{{ item.version }}"
      tag_prefix: "{{ has(item.tag_prefix) ? item.tag_prefix : '' }}"
      prerelease: match
      used_by: "{{ item.used_by }}"
      since: "{{ state.cursor }}"
      lookback: "{{ vars.lookback }}"

  - id: fresh
    uses: flo/dedupe@1
    with:
      items: "{{ results.releases.map(r, r.releases).flatten() }}"
      key: "{{ item.package + '@' + item.version }}"
      seen: "{{ state.seen }}"
      max: 5000
    outputs:
      state:
        seen: "{{ result.seen }}"
        cursor: "{{ run.started_at }}"

  - id: judge
    uses: flo/ai.classify@1
    with:
      model: anthropic://claude-haiku-5-5
      labels: [security, breaking, notable, noise]
      instructions: "Label each release by what it means for a team that depends on it."
      items: "{{ results.fresh.items }}"
    check: { contract: dep-judge-v1 }       # optional

  - id: findings
    uses: flo/output@1
    when: >-
      {{ size(results.newadv.items) > 0 ||
         results.judge.items.exists(x, x.label != 'noise') }}
    with:
      kind: findings
      items: >-
        {{ results.newadv.items.map(a,
             {'title': a.package + ' ' + a.version + ' ' + a.ghsa_id,
              'label': 'security', 'detail': a.summary, 'url': a.url,
              'refs': a.used_by})
           + results.judge.items.filter(x, x.label != 'noise').map(x,
             {'title': x.item.package + ' ' + x.item.version,
              'label': x.label, 'detail': x.why, 'refs': x.item.used_by}) }}

  - id: review
    uses: flo/approval@1
    when: >-
      {{ results.newadv.items.exists(a, a.severity in ['high', 'critical']) ||
         results.judge.items.exists(x, x.label == 'security') }}
    with:
      surface: tlc
      items: >-
        {{ results.newadv.items.filter(a, a.severity in ['high', 'critical']).map(a,
             {'id': a.ghsa_id + '@' + a.package,
              'action': 'open an upgrade task for ' + a.package,
              'why': a.summary, 'class': 'task.create'})
           + results.judge.items.filter(x, x.label == 'security').map(x,
             {'id': x.item.package + '@' + x.item.version,
              'action': 'open an upgrade task for ' + x.item.package,
              'why': x.why, 'class': 'task.create'}) }}

  - id: upgrade
    uses: tlc/task.create@1
    for_each: "{{ results.review.approved }}"
    with:
      title: "{{ item.action }}"
      description: "{{ item.why }}"
      tags: [dependency]
```

What this exercises:

- `for_each` over the packages that have a known source repository, with
  `flatten()` over the per-element results (flow.md §5.6);
  `on_item_error: skip` so one unreachable package does not lose the day.
- Dependencies are **deduplicated across repositories before** any
  per-package call (the node does it and reports `used_by`), so advisories
  and releases are each asked once per package, not once per repository.
- Advisories are asked for **the installed version** of every package, in
  batches, independent of the releases step; the two run concurrently.
- State is read as a snapshot and written through `outputs.state`, one
  writer per key, committed only if the run succeeds (flow.md §7.4). A
  failed run re-sees the same candidates. Releases and advisories are
  keyed `package@version`-style so a re-reported version is dropped.
- First run: `cursor` is `null`, so `gh/releases-since@1` uses `lookback`.
- `upgrade` reads `results.review.approved`. On days with nothing serious
  `review` is skipped, `results.review` is absent and `upgrade` is skipped
  as `upstream_skipped` (flow.md §5.7).
- `tlc/task.create@1` is `write-shared`, so it needs no approval by class;
  the approval above is a product choice. No step is `irreversible`, so V20
  has nothing to guard.
- `privacy: cloud-allowed` is the honest label: package names and release
  notes go to a hosted API and a hosted model. Nothing private is in them.
- The hosted API is rate limited per resource. The dependency-graph export
  has its own small hourly bucket, declared on the node (`rate_limit`,
  node.md §1.1) so the engine can delay or refuse before the provider
  does.

### Node specs

The three GitHub nodes wrap small commands around `gh api` (`gh-deps`,
`gh-advisories`, `gh-releases`). The wrappers, not the engine, own the
provider details. Obligations the wrappers meet (checked in the node's
tests, not by the engine):

- `gh api` exits non-zero on `304` and on every `4xx`. The wrapper turns
  those into the node's documented exit codes (below) and a JSON result;
  the engine never inspects `gh` exit codes.
- Output is written to a file in the step directory (`outputs.from: file:`),
  not stdout. A secret-redacting runner can corrupt a large JSON document
  on stdout, and file output avoids it.
- Versions are normalized (`v` prefix, `= ` prefix and purl-encoded ranges
  removed) and ecosystems are mapped to the advisory vocabulary
  (`golang`→`go`, `pypi`→`pip`, `cargo`→`rust`, `githubactions`→`actions`;
  `npm` unchanged).

```yaml
node: gh/dependency-list@1
kind: command
lowers_to: engine
description: >-
  Packages the given repositories depend on, deduplicated across repositories,
  with who uses them.
argv:
  - gh-deps
  - --mode
  - "{{ in.mode }}"
  - --out
  - "{{ run.step_dir }}/out.json"
  - --
  - "{{ in.repos }}"
inputs:
  repos:
    type: array
    items: { type: string }
    required: true
    description: Source repositories, not mirrors.
  mode:
    type: string
    enum: [sbom, graphql]
    default: sbom
    description: >-
      sbom is one call per repo and cheap, with no direct/transitive flag and no
      source repo. graphql adds relationship and source_repo at more calls per repo.
  include_actions: { type: boolean, default: false }
exit_codes: { ok: [0], retry: [75] }    # 75: provider rate limit, retry later
outputs:
  from: file:out.json
  format: json
  schema:
    type: object
    required: [packages, skipped]
    properties:
      packages:
        type: array
        items:
          type: object
          required: [name, ecosystem, version, used_by]
          properties:
            name: { type: string }
            ecosystem: { type: string, enum: [go, npm, pip, rust, actions, maven, nuget, composer, rubygems, pub, swift, erlang, other] }
            version: { type: string }               # normalized, no "v"
            source_repo: { type: string }           # owner/repo, when known
            tag_prefix: { type: string }            # monorepo module prefix, when known
            relationship: { type: string, enum: [direct, transitive] }   # graphql mode only
            used_by: { type: array, items: { type: string } }
      skipped:
        type: array
        description: Repos that could not be listed, with why (graph disabled, not found).
        items:
          type: object
          properties: { repo: { type: string }, reason: { type: string } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
rate_limit: { bucket: vcs-dependency-export, max: 100, per: 1h }   # applies to mode: sbom
timeout: 120s
retry: { max_attempts: 2, backoff: 30s }
cost: { class: low }
```

A repository whose export returns `404` is ambiguous (missing, or the graph
is disabled); the wrapper disambiguates with one more call and reports it in
`skipped` instead of failing the step.

```yaml
node: gh/advisories@1
kind: command
lowers_to: engine
description: >-
  Published security advisories affecting the given package versions, in
  batches. Withdrawn advisories are dropped.
argv: [gh-advisories, --out, "{{ run.step_dir }}/out.json", --batch, "{{ in.batch_size }}", --]
stdin: { json: "{{ in.packages }}" }
inputs:
  packages:
    type: array
    required: true
    items:
      type: object
      required: [ecosystem, name, version]
      properties:
        ecosystem: { type: string }
        name: { type: string }
        version: { type: string }
        used_by: { type: array, items: { type: string } }
  batch_size: { type: integer, default: 50, minimum: 1, maximum: 100 }
exit_codes: { ok: [0], retry: [75] }
outputs:
  from: file:out.json
  format: json
  schema:
    type: object
    required: [advisories]
    properties:
      advisories:
        type: array
        items:
          type: object
          required: [ghsa_id, package, ecosystem, version, severity]
          properties:
            ghsa_id: { type: string }
            cve_id: { type: [string, 'null'] }
            package: { type: string }
            ecosystem: { type: string }
            version: { type: string }                 # the affected installed version
            severity: { type: string, enum: [low, medium, high, critical] }   # lowercase
            summary: { type: string }
            vulnerable_range: { type: string }
            first_patched: { type: [string, 'null'] }  # plain string
            url: { type: string }
            used_by: { type: array, items: { type: string } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
timeout: 60s
retry: { max_attempts: 3, backoff: 5s }
```

The wrapper groups packages by ecosystem and asks one advisories call per
batch with the provider's `affects=pkg@version,pkg@version,…` filter,
following its cursor pagination. It maps severities to one lowercase scale
and patched versions to a plain string, since the provider's own endpoints
disagree on both.

```yaml
node: gh/releases-since@1
kind: command
lowers_to: engine               # reshapes its output with `map`
description: >-
  Releases (or tags, when there are none) of one package's source repository
  newer than a point in time. Never uses the "latest release" endpoint.
argv:
  - gh-releases
  - --out
  - "{{ run.step_dir }}/out.json"
  - --repo
  - "{{ in.source_repo }}"
  - --package
  - "{{ in.package }}"
  - --version
  - "{{ in.version }}"
  - --tag-prefix
  - "{{ in.tag_prefix }}"
  - --prerelease
  - "{{ in.prerelease }}"
  - --since
  - "{{ in.since }}"
  - --lookback
  - "{{ in.lookback }}"
inputs:
  source_repo: { type: string, required: true }
  package:     { type: string, required: true }
  version:     { type: string, required: true, description: The installed version. }
  tag_prefix:  { type: string, default: "", description: "Monorepo module prefix such as `sub/`; only tags with it count." }
  prerelease:
    type: string
    enum: [exclude, include, match]
    default: match
    description: >-
      match reports prereleases only when the installed version is itself
      one; exclude never; include always. Drafts are always skipped.
  used_by:  { type: array, items: { type: string }, default: [] }
  since:    { type: [string, 'null'], format: date-time, default: null }
  lookback: { type: string, format: duration, default: 14d }
exit_codes: { ok: [0], retry: [75] }
outputs:
  from: file:out.json
  format: json
  map: >-
    {{ {'source': out.source,
        'releases': out.releases.map(r,
          {'package': in.package, 'version': r.version, 'tag': r.tag,
           'notes': r.notes, 'url': r.url, 'published_at': r.published_at,
           'prerelease': r.prerelease, 'used_by': in.used_by})} }}
  schema:
    type: object
    required: [source, releases]
    properties:
      source: { type: string, enum: [releases, tags, none] }
      releases:
        type: array
        items:
          type: object
          required: [package, version]
          properties:
            package: { type: string }
            version: { type: string }                    # tag with prefix and "v" removed
            tag: { type: string }
            notes: { type: string }                       # empty for tag-only repos
            url: { type: string }
            published_at: { type: [string, 'null'] }
            prerelease: { type: boolean }
            used_by: { type: array, items: { type: string } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
timeout: 30s
retry: { max_attempts: 3, backoff: 2s }
```

The wrapper walks releases newest-created first and stops at the `since`
point or the installed version; it does not sort by name. It matches the tag
prefix before comparing versions. With no releases at all it falls back to
tags with commit dates (`source: tags`, no notes). A conditional request
that returns "not modified" is a successful empty result, not an error.

```yaml
node: tlc/task.create@1
kind: command
lowers_to: exec
description: Create a task.
argv: [tlc, task, create, "{{ in.title }}", --description, "{{ in.description }}", --format, json]
inputs:
  title:       { type: string, required: true }
  description: { type: string, default: "" }
  tags:        { type: array, items: { type: string }, default: [] }
outputs:
  from: stdout
  format: json
  schema: { type: object, required: [id], properties: { id: { type: string } } }
side_effect: write-shared
privacy: local
capabilities: [tasks.write]
idempotent: false
idempotency_key: "{{ in.title }}"
timeout: 15s
```

`tags` is accepted by the node but this command spec does not pass it;
a real spec repeats `--tag` per entry (a list-valued whole argument
expands into several arguments, node.md §4.1).

## 2. meeting-brief

Thirty minutes before each meeting with more than one attendee: gather
what is known about the people, the open mail threads and the related
tasks, and write a one-page brief.

```yaml
flow: meeting-brief
version: 0.1.0
description: A brief 30 minutes before each meeting.
on:
  - calendar:
      source: calendar/events@1
      offset: -30m
      when: "{{ size(trigger.event.attendees) > 1 }}"
run_as: automation
privacy: local-only             # attendees, threads and tasks stay on this machine
budget: { usd: 0.10 }
tasks: none
steps:
  - id: people
    uses: ctxt/find@1
    with:
      query: "{{ trigger.event.attendees.map(a, a.email) }}"
      limit: 20

  - id: threads
    uses: heo/conversation.list@1
    with:
      participants: "{{ trigger.event.attendees.map(a, a.email) }}"
      since: 30d

  - id: open
    uses: tlc/task.list@1
    with:
      search: "{{ trigger.event.title }}"
      status: [todo, in_progress, blocked]

  - id: brief
    uses: flo/ai.generate@1
    with:
      model: ollama://gemma4:e4b
      prompt: meeting-brief@1
      context:
        event: "{{ trigger.event }}"
        people: "{{ results.people }}"
        threads: "{{ results.threads }}"
        tasks: "{{ results.open }}"

  - id: out
    uses: flo/output@1
    with:
      kind: brief
      title: "{{ trigger.event.title }}"
      body: "{{ results.brief.text }}"
```

What this exercises:

- A calendar trigger with an offset and a `when` over the event payload;
  the engine keeps one pending fire per upcoming event and re-plans it
  when the event moves (flow.md §6).
- Three independent read steps with no `depends_on`: they run
  concurrently. `brief` starts when all three are terminal, because it
  reads their results.
- `privacy: local-only` is satisfied because every node here is `local`
  and the model scheme is `ollama://` (flow.md §7.6, V19). A hosted model
  would need `cloud-allowed` and would send the same data off the
  machine.
- Whole-placeholder fields keep their types: `query` is a list,
  `context` is a map.

The prompt `prompts/meeting-brief@1` lives in the flow bundle's
`prompts/` directory.

### Node specs

```yaml
node: calendar/events@1
kind: plugin
role: trigger
description: Upcoming calendar events with start times.
config:
  url: { type: string, required: true }
inputs:
  calendar: { type: string, default: primary }
outputs:
  schema:
    type: object
    required: [uid, start, title, attendees]
    properties:
      uid: { type: string }
      recurrence_id: { type: [string, 'null'] }
      start: { type: string, format: date-time }
      end: { type: string, format: date-time }
      title: { type: string }
      attendees:
        type: array
        items:
          type: object
          required: [email]
          properties:
            email: { type: string }
            name: { type: string }
            status: { type: string }
side_effect: read
privacy: local
capabilities: [calendar.read]
```

```yaml
node: ctxt/find@1
kind: http
lowers_to: engine
description: Look up people, decisions and notes in the knowledge store.
request:
  method: POST
  url: "{{ config.ctxt.url }}/api/v1/find"
  json: { query: "{{ in.query }}", limit: "{{ in.limit }}" }
inputs:
  query: { type: [string, array], required: true }
  limit: { type: integer, default: 10 }
outputs:
  from: body.results
  schema: { type: array, items: { type: object } }
side_effect: read
privacy: local
capabilities: [knowledge.read]
timeout: 10s
retry: { max_attempts: 3, backoff: 2s }
record: true
```

```yaml
node: heo/conversation.list@1
kind: command
lowers_to: exec
description: Conversations involving the given participants.
argv: [heo, conversation, list, --participants, "{{ in.participants }}", --since, "{{ in.since }}", --format, json]
inputs:
  participants: { type: array, items: { type: string }, required: true }
  since: { type: string, format: duration, default: 30d }
outputs:
  from: stdout
  format: json
  schema:
    type: array
    items:
      type: object
      properties:
        subject: { type: string }
        last_message: { type: string }
        state: { type: string }
side_effect: read
privacy: local
capabilities: [mail.read]
timeout: 20s
```

```yaml
node: tlc/task.list@1
kind: command
lowers_to: exec
description: Tasks matching a search and status.
argv: [tlc, task, list, --search, "{{ in.search }}", --status, "{{ in.status }}", --format, json]
inputs:
  search: { type: string, required: true }
  status: { type: array, items: { type: string }, default: [] }
outputs:
  from: stdout
  format: json
  schema: { type: array, items: { type: object } }
side_effect: read
privacy: local
capabilities: [tasks.read]
timeout: 15s
```

## 3. inbox-triage

Every 15 minutes: sync headers, make typed decisions, build a plan of
what is automatic and what needs a person, ask the person, record the
decisions with the system that owns trust, and apply.

```yaml
flow: inbox-triage
version: 0.1.0
description: Classify new mail, ask about what needs a person, apply the rest.
on:
  - schedule: { rrule: "FREQ=MINUTELY;INTERVAL=15" }
run_as: automation
privacy: cloud-allowed          # `decide` sends headers only
tasks: per-run
concurrency: { on_overlap: skip }
steps:
  - id: fetch
    uses: inbox/fetch@1

  - id: decide
    uses: inbox/decide@1
    depends_on: [fetch]
    with: { run: "{{ results.fetch.run }}" }

  - id: plan
    uses: inbox/plan@1
    depends_on: [decide]
    with: { run: "{{ results.fetch.run }}" }

  - id: approve
    uses: flo/approval@1
    when: "{{ size(results.plan.pending) > 0 }}"
    with:
      items: "{{ results.plan.pending }}"
      verdicts: [approve, reject]
      trust: inbox
      surface: tlc
      context: { run: "{{ results.fetch.run }}" }

  - id: apply
    uses: inbox/apply@1
    depends_on: [plan, approve]
    with: { run: "{{ results.fetch.run }}" }
```

What this exercises:

- **Command nodes wrapping a script.** Each `inbox/*` node is a declared
  command run as the `automation` profile. The script keeps its own files
  between subcommands; the flow orders them with `depends_on`, because
  that coupling is not data the engine can see (flow.md §5.4). Data that
  *is* visible (`run`) flows through `with`. Paths come from pack config
  (`config.inbox.*`), so a test run points them at a temporary tree.
- **Honest side-effect classes.** Syncing headers is not free of effects:
  with some script settings the sync step also acts on mail on its own.
  The `fetch` node therefore requires the script's no-lanes mode; the
  mailbox actions that mode leaves out are a separate, irreversible node
  with its own approval (`inbox/lanes@1`, not used in this flow).
- **An irreversible, non-idempotent step behind an approval.**
  `inbox/apply@1` is `irreversible` and not idempotent: until the script
  records each action as it is done, a crash between an external send and
  its record can repeat the send on a re-run. So the engine never retries
  it automatically, and an unknown outcome parks the run for a person
  (node.md §11, flow.md §7.9). V20 accepts the step because `approve` is
  an ancestor. `approve` may be skipped when nothing is pending; dependents
  of a skipped step still run (flow.md §5.4), and what the script applies
  is exactly what the plan marked automatic plus what was approved. At
  dispatch the engine still checks that a decision or an `auto` record
  exists for the run's items.
- **Failures in the output, not the exit code.** The script exits 0 when
  accounts fail or actions fail. The nodes read a JSON summary and fail the
  step with `fail_when` (node.md §3.2).
- **A trust provider.** `trust: inbox` names a provider declared below:
  `read` marks pending items whose actions are already trusted as `auto`;
  `decide` records the person's decisions with the script, which owns the
  counters; `set` backs `flo trust set`.
- **Parked runs.** While a run waits for an approval, the next 15-minute
  fire is skipped (`on_overlap: skip`; flow.md §7.3). The script also
  carries pending items into later runs, so a better shape is possible; it
  is an open question in flow.md (§11.3).
- **tasks: per-run.** The accountable task is created only when the run
  needs a person; most runs create none. The script's own task creation is
  turned off (`--no-task`), so the flow's approval is the only task.

### Script-side prerequisites

The node specs below describe the contract flo needs. A script that does
not meet it yet needs these changes before the flow can be activated; they
are properties of the wrapped script, not of the engine.

1. **A fetch mode without lanes.** `fetch --no-lanes` (or equivalent
   configuration honored by the sync step): no automated trash,
   unsubscribe, label or archive outside the plan and approval path. Those
   actions become their own declared node.
2. **A JSON summary on stdout** for `fetch`, `decide`, `plan` and
   `apply` (`--format json`), with the fields in the node outputs
   below. The run id is learned from this, not by parsing text.
3. **Failure signals.** Non-zero exit when every account failed. Failed
   accounts and failed actions appear in the JSON (`accounts`, `failed`);
   the engine fails the step from them.
4. **`--no-task` on `plan` and `apply`**, so only the flow's approval task
   exists.
5. **A decisions interface for `approve`.** `approve RUN --decisions -`
   reads a JSON list `[{n, verdict: approve|reject|correct, reason?,
   actions?}]` and prints `{applied: [n…], skipped: [n…]}`. Today
   approvals are command-line flags by item number, silently ignore
   unknown or already-decided items, and a correction needs a prior edit of
   the decisions file. Until then the flow uses `verdicts: [approve,
   reject]`.
6. **Per-action durability in `apply`.** Record each action as done before
   making the next external call, and consult that record on re-run.
   Until then `inbox/apply@1` stays non-idempotent.
7. **Trust surface.** `trust list --format json` (key, level, streak,
   threshold, resets, last reset), `trust promote KEY` (no command exists;
   only an interface of another component promotes today), and a single
   writer or a lock around the trust file, which other entry points
   (lane approval, a review server) also write.
8. **Optional but useful.** `decide --force` for replays; `apply
   --retry-failed`; stop copying superseded items into `plan.json` (the
   node filters them out meanwhile); a `version` field on the run files.
9. **Environment.** Credentials for the run's profile, and
   `INBOX_HOME`/`INBOX_RUNS` (set by the nodes from config) so tests run
   on a temporary tree. The script has no dry-run for `apply`; flo's own
   dry run never executes an irreversible node, so it needs none.

### Node specs

All `inbox/*` nodes share a pack config:

```yaml
pack: inbox
version: 0.1.0
config:
  inbox:
    root: { type: string, required: true }     # working directory of the script
    home: { type: string, required: true }     # rules and state
    runs: { type: string, required: true }     # run folders
```

```yaml
node: inbox/fetch@1
kind: command
lowers_to: engine
description: Sync message headers into a new run, without acting on mail.
argv: [inbox, fetch, --no-lanes, --format, json]
env: { INBOX_HOME: "{{ config.inbox.home }}", INBOX_RUNS: "{{ config.inbox.runs }}" }
cwd: "{{ config.inbox.root }}"
inputs:
  accounts: { type: array, items: { type: string }, default: [] }
  limit:    { type: integer, default: 200 }
exit_codes: { ok: [0] }
outputs:
  from: stdout
  format: json
  fail_when: "{{ size(out.accounts) > 0 && out.accounts.all(a, a.status == 'error') }}"
  schema:
    type: object
    required: [run, rows, new, carried, accounts]
    properties:
      run: { type: string }
      rows: { type: integer }
      new: { type: integer }
      carried: { type: integer }
      accounts:
        type: array
        items:
          type: object
          required: [account, status]
          properties:
            account: { type: string }
            status: { type: string, enum: [ok, error] }
            count: { type: integer }
            error: { type: string }
side_effect: write-local        # a new run folder and the local mail store; reads mail servers
idempotent: false               # every call creates a new run
privacy: local
capabilities: [mail.read]
timeout: 120s
```

`fetch` is not idempotent in effect (each call makes a new run), but it is
safe to repeat: nothing is marked seen until `plan`. The engine treats it
as `write-local`, may re-run it after a crash, and the step record names the
run it created.

```yaml
node: inbox/decide@1
kind: command
lowers_to: engine
description: Typed decisions for the run's messages, from headers only.
argv: [inbox, decide, "{{ in.run }}", --format, json]
env: { INBOX_HOME: "{{ config.inbox.home }}", INBOX_RUNS: "{{ config.inbox.runs }}" }
cwd: "{{ config.inbox.root }}"
inputs: { run: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  schema:
    type: object
    required: [run, decided]
    properties:
      run: { type: string }
      decided: { type: integer }
      cost_usd: { type: number }
side_effect: write-local        # writes the run's decisions; the model call spends money
idempotent: true                # an existing decisions file is kept, not asked again
privacy: cloud                  # headers go to a hosted classifier
capabilities: [mail.read]
timeout: 300s
cost: { class: metered, estimate: { usd: 0.05 } }
```

```yaml
node: inbox/plan@1
kind: command
lowers_to: engine               # reshapes its output with `map`
description: >-
  Split decisions into automatic, pending and invalid. Marks the run's
  messages as seen and locks pending ones. Creates no task.
argv: [inbox, plan, "{{ in.run }}", --unattended, --no-task, --format, json]
env: { INBOX_HOME: "{{ config.inbox.home }}", INBOX_RUNS: "{{ config.inbox.runs }}" }
cwd: "{{ config.inbox.root }}"
inputs: { run: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  map: >-
    {{ {'run': out.run,
        'auto': out.items.filter(i, i.status == 'auto'),
        'invalid': out.items.filter(i, i.status == 'invalid'),
        'pending': out.items.filter(i, i.status == 'pending' && i.verdict == null).map(i,
          {'id': i.message_id, 'n': i.n, 'action': i.keys.join(', '),
           'why': i.why, 'class': i.keys, 'from': i.from, 'subject': i.subject,
           'actions': i.actions})} }}
  schema:
    type: object
    required: [run, auto, invalid, pending]
    properties:
      run: { type: string }
      auto:    { type: array, items: { type: object } }
      invalid: { type: array, items: { type: object } }
      pending:
        type: array
        items:
          type: object
          required: [id, n, action, why, class]
          properties:
            id: { type: string }            # the message id: stable across runs
            n: { type: integer }            # item number within this run
            action: { type: string }
            why: { type: string }
            class: { type: array, items: { type: string } }   # trust keys of its actions
side_effect: write-local        # run files, seen set, locks; reads mail servers
idempotent: true                # re-planning keeps decided items and rebuilds locks
privacy: local
capabilities: [mail.read]
timeout: 60s
```

The JSON summary from the script is the plan itself (`items` with `status`,
`verdict`, `keys`, `actions`, `message_id`, `n`); only items still
undecided are offered for approval, and superseded copies of earlier items
are ignored. The approval item's `id` is the message id, because the item
number is only unique within one run.

```yaml
node: inbox/apply@1
kind: command
lowers_to: engine               # irreversible nodes never lower to exec
description: Apply the plan's automatic and approved actions to the mailbox.
argv: [inbox, apply, "{{ in.run }}", --no-task, --format, json]
env: { INBOX_HOME: "{{ config.inbox.home }}", INBOX_RUNS: "{{ config.inbox.runs }}" }
cwd: "{{ config.inbox.root }}"
inputs: { run: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  fail_when: "{{ size(out.failed) > 0 }}"
  schema:
    type: object
    required: [run, applied, failed, pending_left]
    properties:
      run: { type: string }
      applied: { type: integer }
      failed:
        type: array
        items: { type: object, properties: { n: { type: integer }, error: { type: string } } }
      pending_left: { type: integer }
side_effect: irreversible       # label, archive, reply, send, unsubscribe, move
approval: required
idempotent: false               # until the script records each action as it is done
privacy: local
capabilities: [mail.write, mail.send]
timeout: 300s
```

With no idempotency key the engine clamps retries to one (node.md §11). A
step that fails because some actions failed (`failed` not empty) is a
normal failed step; repeating them is the person's decision (a retry
needs the script's `--retry-failed`, prerequisite 8).

```yaml
trust: inbox
kind: command
description: Trust state owned by the inbox script.
read:                           # [{class, level: ask|auto, streak, threshold, resets, last_reset}]
  argv: [inbox, trust, list, --format, json]
decide:                         # once per approval, after every item has a decision
  argv: [inbox, approve, "{{ in.context.run }}", --decisions, "-", --format, json]
  stdin: { json: "{{ in.decisions }}" }
set:                            # backs `flo trust set`
  argv: [inbox, trust, "{{ in.level == 'auto' ? 'promote' : 'reset' }}", "{{ in.class }}", --reason, "{{ in.reason }}"]
env: { INBOX_HOME: "{{ config.inbox.home }}", INBOX_RUNS: "{{ config.inbox.runs }}" }
cwd: "{{ config.inbox.root }}"
```

Mapping to the script: its modes `training` and `auto` are `ask` and
`auto`; its streak and threshold per action key are reported as they are.
Destructive classes (trash, spam, unsubscribe) cap promotion below `auto`,
and `set` fails with `trust_capped` for them. `decide` feeds the script's
own counters (a clean approval advances a streak, a rejection resets it);
flo never writes those files itself.

## 4. What writing these changed in the spec

Each of these was a point where a first draft of a flow did not work, and
the spec was fixed instead.

- **Per-element results are lists of lists.** A `for_each` over packages
  yields one result per package, each holding its own list. Reading them
  as one flat list needs `flatten()`, so the rule is explicit (flow.md
  §5.6) and the engine does not merge implicitly.
- **Steps have no implicit order.** The triage flow chained script
  subcommands that share files, not data. `depends_on` is needed and data
  references add edges on their own (flow.md §5.4).
- **Optional branches.** `review` is skipped on days without a security
  finding, and `upgrade` reads its result. A step that reads an absent
  result is skipped, not failed; `has()` opts into reading absence
  (flow.md §5.7).
- **State must have defaults.** The first run has no `cursor` and no
  `seen`; keys are declared with defaults, read from a snapshot, and
  written by `outputs.state` (flow.md §7.4). The dedupe node is pure and
  returns the new `seen`; the flow commits it.
- **Triggers have their own payload type.** The calendar filter reads
  `trigger.event`, validated against the trigger node's output schema
  (node.md §7).
- **Overlap.** A 15-minute schedule meets a run parked for hours.
  `concurrency` exists, and `skip` is the default for scheduled runs
  (flow.md §7.3).
- **Privacy is a ceiling, not a claim about data.** The watcher can be
  `cloud-allowed` because its data is public; the brief handles private
  data and is `local-only` with a local model. The same label cannot
  carry both meanings, so the spec says what it enforces and what it
  does not (flow.md §7.6).
- **Trust has a home.** The triage flow needs counters it does not own;
  a trust provider names the commands that read, record and set them
  (node.md §10.3). The provider is the single writer of that state only if
  the wrapped tool's other entry points also go through its lock.
- **Declared effects must be true.** A sync step that can also act on mail
  is not `read`. The spec requires a mode without those actions, and the
  actions get their own irreversible node.
- **Command wrappers own provider quirks.** Rate-limit buckets, error
  statuses that make the CLI exit non-zero, differing version and severity
  formats and ecosystem names are handled in the wrapper; the node spec
  declares the clean shape, a `rate_limit`, `exit_codes` and file output
  (node.md §1.1, §4).
- **Exit codes are not failure signals.** A script may exit 0 with failed
  items. `outputs.fail_when` turns a field of the parsed output into a step
  failure (node.md §3.2).
- **A command node can be irreversible.** `inbox/apply@1` wraps a script
  with real effects; the approval requirement lives on the node spec, so a
  flow cannot forget it (node.md §10.1).
