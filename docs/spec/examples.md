# Worked examples

Three flows that share nothing but the engine, written against
[flow.md](flow.md) and [node.md](node.md): a dependency watcher, a meeting
brief and inbox triage. Each is followed by the node specs it uses. Names,
repositories and profiles are placeholders. The commands wrapped by the
triage nodes (`inbox …`) stand for any script with the same contract.

Node specs for command-based tools are written against the tool's
documented output; their exact response shapes are confirmed by probing
the real tools and may adjust the `outputs.schema` without changing a
flow.

## 1. dependency-watch

Every morning: list what the repositories depend on, fetch releases and
advisories since the last run, drop what was seen, classify, publish
findings, and offer to open an upgrade task for security findings.

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
  seen:   { default: [] }
  cursor: { default: null }
steps:
  - id: deps
    uses: gh/dependency-list@1
    with: { repos: "{{ vars.repos }}" }

  - id: releases
    uses: gh/releases-since@1
    for_each: "{{ results.deps.packages }}"
    concurrency: 4
    on_item_error: skip
    with:
      ecosystem: "{{ item.ecosystem }}"
      package: "{{ item.name }}"
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
    when: "{{ results.judge.items.exists(x, x.label != 'noise') }}"
    with:
      kind: findings
      items: >-
        {{ results.judge.items.filter(x, x.label != 'noise').map(x,
           {'title': x.item.package + ' ' + x.item.version,
            'label': x.label, 'detail': x.why, 'refs': x.item.used_by}) }}

  - id: review
    uses: flo/approval@1
    when: "{{ results.judge.items.exists(x, x.label == 'security') }}"
    with:
      surface: tlc
      items: >-
        {{ results.judge.items.filter(x, x.label == 'security').map(x,
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

- `for_each` over the packages, with `flatten()` over the per-element
  results (flow.md §5.6); `on_item_error: skip` so one unreachable
  package does not lose the day.
- State read as a snapshot (`state.seen`, `state.cursor`) and written
  through `outputs.state`, committed only if the run succeeds (flow.md
  §7.4). A failed run re-sees the same candidates.
- `fresh` reads the first-run cursor as `null`; `gh/releases-since@1`
  then uses `lookback`.
- `upgrade` reads `results.review.approved`. When there is no security
  finding, `review` is skipped, `results.review` is absent, and `upgrade`
  is skipped as `upstream_skipped` (flow.md §5.7) rather than failing.
- `tlc/task.create@1` is `write-shared`, so it needs no approval by class
  and the approval above is a product choice. No step here is
  `irreversible`, so V20 has nothing to guard.
- `privacy: cloud-allowed` is the honest label: package names and release
  notes go to a hosted API and a hosted model. Nothing private is in
  them.

### Node specs

```yaml
node: gh/dependency-list@1
kind: command
lowers_to: exec
description: Packages that the given repositories depend on, with who uses them.
argv: [gh-deps, --repos, "{{ in.repos }}"]
inputs:
  repos: { type: array, items: { type: string }, required: true }
outputs:
  from: stdout
  format: json
  schema:
    type: object
    required: [packages]
    properties:
      packages:
        type: array
        items:
          type: object
          required: [name, ecosystem, version, used_by]
          properties:
            name: { type: string }
            ecosystem: { type: string }
            version: { type: string }
            used_by: { type: array, items: { type: string } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
timeout: 120s
retry: { max_attempts: 2, backoff: 5s }
cost: { class: low }
```

```yaml
node: gh/releases-since@1
kind: command
lowers_to: engine               # reshapes its output with `map`
description: Releases and advisories for one package published after a point in time.
argv: [gh-releases, --ecosystem, "{{ in.ecosystem }}", --package, "{{ in.package }}", --since, "{{ in.since }}", --lookback, "{{ in.lookback }}"]
inputs:
  ecosystem: { type: string, required: true }
  package:   { type: string, required: true }
  used_by:   { type: array, items: { type: string }, default: [] }
  since:     { type: [string, 'null'], format: date-time, default: null }
  lookback:  { type: string, format: duration, default: 14d }
outputs:
  from: stdout
  format: json
  map: "{{ {'releases': out.releases.map(r, {'package': r.package, 'version': r.version, 'notes': r.notes, 'advisories': r.advisories, 'url': r.url, 'used_by': in.used_by})} }}"
  schema:
    type: object
    required: [releases]
    properties:
      releases:
        type: array
        items:
          type: object
          required: [package, version]
          properties:
            package: { type: string }
            version: { type: string }
            notes: { type: string }
            advisories: { type: array, items: { type: string } }
            url: { type: string }
            used_by: { type: array, items: { type: string } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
timeout: 30s
retry: { max_attempts: 3, backoff: 2s }
```

`gh-deps` and `gh-releases` stand for small wrappers around `gh api`; `gh-releases` resolves a package to its
source repository and reads releases and advisories; a node may wrap any
command. In `map`, `in` is readable because `map` is part of the node spec.

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
  *is* visible (`run`) flows through `with`.
- **An irreversible step behind an approval.** `inbox/apply@1` is
  `irreversible`. V20 accepts it because `approve` is an ancestor
  (`depends_on`). `approve` may be skipped when there is nothing pending;
  dependents of a skipped step still run (flow.md §5.4), and what the
  script applies is exactly what the plan marked automatic plus what was
  approved. At dispatch the engine still checks that a decision or an
  `auto` record exists for the run's items.
- **A trust provider.** `trust: inbox` names a provider declared below:
  `read` marks pending items whose class is already trusted as `auto`;
  `decide` records the person's decisions with the script, which stays the
  only writer of the counters while it is wrapped.
- **Parked runs.** While a run waits for an approval, the next 15-minute
  fire is skipped (`on_overlap: skip`; flow.md §7.3). The consequence and
  the way out are an open question in flow.md (§11.3).
- **tasks: per-run.** The accountable task is created only when the run
  needs a person; most runs create none.

### Node specs

```yaml
node: inbox/fetch@1
kind: command
lowers_to: exec
description: Sync message headers and create a run stamp.
argv: [inbox, fetch, --format, json]
inputs: {}
outputs:
  from: stdout
  format: json
  schema: { type: object, required: [run], properties: { run: { type: string } } }
side_effect: write-local
privacy: local
capabilities: [mail.read]
timeout: 120s
```

```yaml
node: inbox/decide@1
kind: command
lowers_to: exec
description: Typed decisions for the run's messages, from headers only.
argv: [inbox, decide, "{{ in.run }}", --format, json]
inputs: { run: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  schema: { type: object, properties: { count: { type: integer } } }
side_effect: read
privacy: cloud                  # headers go to a hosted classifier
capabilities: [mail.read]
timeout: 300s
cost: { class: metered, estimate: { usd: 0.05 } }
```

```yaml
node: inbox/plan@1
kind: command
lowers_to: engine               # reshapes its output with `map`
description: Split decisions into automatic, pending and invalid.
argv: [inbox, plan, "{{ in.run }}", --format, json]
inputs: { run: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  map: "{{ {'auto': out.auto, 'invalid': out.invalid, 'pending': out.pending.map(p, {'id': p.id, 'action': p.action, 'why': p.why, 'class': p.class})} }}"
  schema:
    type: object
    required: [auto, pending]
    properties:
      auto:    { type: array, items: { type: object } }
      invalid: { type: array, items: { type: object } }
      pending:
        type: array
        items:
          type: object
          required: [id, action, why]
          properties:
            id: { type: string }
            action: { type: string }
            why: { type: string }
            class: { type: string }
side_effect: write-local
privacy: local
capabilities: [mail.read]
timeout: 60s
```

```yaml
node: inbox/apply@1
kind: command
lowers_to: engine               # irreversible nodes never lower to exec
description: Apply the plan's automatic and approved actions to the mailbox.
argv: [inbox, apply, "{{ in.run }}", --format, json]
inputs: { run: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  schema: { type: object, properties: { applied: { type: integer }, receipts: { type: array } } }
side_effect: irreversible       # label, archive, reply and unsubscribe
approval: required
idempotent: true                # the script skips actions it already applied for this run
privacy: local
capabilities: [mail.write, mail.send]
timeout: 300s
```

```yaml
trust: inbox
kind: command
description: Trust state held by the inbox script.
read:
  argv: [inbox, trust, list, --format, json]
decide:
  argv: [inbox, approve, "{{ in.context.run }}"]
  stdin: { json: "{{ in.decisions }}" }
set:
  argv: [inbox, trust, set, "{{ in.class }}", "{{ in.level }}"]
```

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
  (node.md §10.3).
- **A command node can be irreversible.** `inbox/apply@1` wraps a script
  with real effects; the approval requirement lives on the node spec, so a
  flow cannot forget it (node.md §10.1).
