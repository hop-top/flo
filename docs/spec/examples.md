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

- A calendar trigger with an offset and a `when` over the event payload;
  the engine keeps one pending fire per upcoming event and re-plans it
  when the event moves (flow.md §6).
- `for_each` over the attendees: one ctxt search per person, four at a
  time, a failed search skipped (`on_item_error: skip`) so one unreachable
  lookup costs a person, not the brief. `flo/dedupe@1` merges the lists by
  object id.
- `people`, `threads` and `open` are independent reads with no
  `depends_on`: they run concurrently. `brief` starts when they are
  terminal, because it reads their results.
- `privacy: local-only` is satisfied because every node here is `local`
  and the model scheme is `ollama://` (flow.md §7.6, V19). A hosted model
  would need `cloud-allowed` and would send the same data off the
  machine.
- Whole-placeholder fields keep their types: `participants` is a list,
  `context` is a map. Lists are capped at ten for a small model, with the
  counts alongside.

The bundle (`flows/meeting-brief/`) holds the flow, the prompt
`prompts/meeting-brief@1.yaml` (the four sections: Event, People & notes,
Threads, Open tasks), `tests/` and the recording in `record/`.

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
description: A brief 30 minutes before each meeting with more than one attendee.
on:
  - calendar:
      source: calendar/events@1
      offset: -30m
      when: "trigger.event.attendees.size() > 1"
vars:
  # Characters of facts the model gets: the event, then notes, threads and
  # tasks as they fit. 6000 is about 1500 tokens, a quarter of a 4B model's
  # 8k window, leaving room for the instructions and the answer.
  context_chars: { default: 6000, type: integer }
run_as: automation
privacy: local-only             # attendees, notes, threads and tasks stay on this machine
budget: { usd: 0.10 }
tasks: none
steps:
  # What the knowledge store holds on each attendee: one search per person. A
  # name is searched as such and narrows to notes that name the person (the
  # store matches the names it extracted, not addresses); an address alone is
  # searched as text, with no narrowing, since no extracted name equals it.
  # A search that fails leaves that person out; with the store down the brief
  # says so. One retry after half a second: a brief is not worth a wait.
  - id: people
    uses: ctxt/find@1
    for_each: "{{ trigger.event.attendees }}"
    concurrency: 4
    on_item_error: skip
    retry: { max_attempts: 2, backoff: 500ms }
    timeout: 5s
    with:
      query: "{{ item.name != '' ? item.name : item.email }}"
      limit: 5
      filter:
        person: "{{ item.name }}"

  # Two attendees often turn up in the same note: one entry per object id.
  - id: notes
    uses: flo/dedupe@1
    with:
      items: "{{ results.people.map(p, p.objects).flatten() }}"
      key: "{{ item.id }}"

  # threads and open run once, over a one-element list, only so that
  # on_item_error: skip can leave the result empty when heo or tlc fails
  # (flow.md §5.6): the step has no failure handler of its own. An empty
  # result means the source was unavailable; a source with nothing to report
  # still returns its one (empty) answer.
  - id: threads
    uses: heo/conversation.list@1
    for_each: "{{ [trigger.event.attendees.map(a, a.email)] }}"
    on_item_error: skip
    retry: { max_attempts: 2, backoff: 2s }   # heo exits 70 while its daemon starts
    timeout: 20s
    with:
      participants: "{{ item }}"
      since: 30d

  - id: open
    uses: tlc/task.list@1
    for_each: "{{ [trigger.event.title] }}"
    on_item_error: skip
    timeout: 10s
    with:
      search: "{{ item }}"
      status: [TODO, IN_PROGRESS]

  # Lists are cut to what fits context_chars, newest first as the sources
  # return them: notes get half of it less the event's own size, threads and
  # tasks a quarter each. An item costs its rendered length, as a string of
  # that length (the expression language has no sum; a note's summary counts up
  # to the 200 characters the prompt keeps); the longest prefix whose strings
  # joined stay within the share is kept. Counts say how many there were.
  - id: brief
    uses: flo/ai.generate@1
    with:
      model: ollama://gemma4:e4b
      prompt: meeting-brief@1
      context:
        event: "{{ trigger.event }}"
        unavailable:
          notes: "{{ size(trigger.event.attendees) > 0 && size(results.people) == 0 }}"
          threads: "{{ size(results.threads) == 0 }}"
          tasks: "{{ size(results.open) == 0 }}"
        notes: >-
          {{ results.notes.items.slice(0, lists.range(size(results.notes.items)).filter(i,
               results.notes.items.slice(0, i + 1).map(n,
                 (size(n.summary) > 200 ? n.summary.substring(0, 200) : n.summary) + n.id + n.type + n.tags.join(',') + '                    ').join('').size()
               <= context_chars / 2 - (size(trigger.event.title) + 200 + size(trigger.event.attendees.map(a, a.name + a.email + a.status).join('')) + 12 * size(trigger.event.attendees))).size()) }}
        threads: >-
          {{ results.threads.map(t, t.conversations).flatten().slice(0, lists.range(size(results.threads.map(t, t.conversations).flatten())).filter(i,
               results.threads.map(t, t.conversations).flatten().slice(0, i + 1).map(c, c.id + c.status + '                                                  ').join('').size()
               <= context_chars / 4).size()) }}
        tasks: >-
          {{ results.open.flatten().slice(0, lists.range(size(results.open.flatten())).filter(i,
               results.open.flatten().slice(0, i + 1).map(t, t.id + t.title + t.status + '                                                  ').join('').size()
               <= context_chars / 4).size()) }}
        counts:
          notes: "{{ size(results.notes.items) }}"
          threads: "{{ size(results.threads.map(t, t.conversations).flatten()) }}"
          tasks: "{{ size(results.open.flatten()) }}"

  - id: out
    uses: flo/output@1
    with:
      kind: brief
      title: "Brief: {{ trigger.event.title }}"
      body: "{{ results.brief.text }}"
```

What this exercises:

- A calendar trigger with an offset and a `when` over the event payload;
  the engine keeps one pending fire per upcoming event and re-plans it
  when the event moves (flow.md §6).
- Three independent read steps with no `depends_on`: they run
  concurrently. `brief` starts when all three are terminal, because it
  reads their results.
- A source that fails does not fail the run: `for_each` with
  `on_item_error: skip` (flow.md §5.6) leaves its result empty, here over
  a one-element list for the single-call steps, and the brief says
  `source unavailable: <source>`. The step has no `on_error` of its own,
  and the skipped element's error code is not readable from `results`, so
  the line names the source only. `retry` and `timeout` on the step
  (flow.md §5.10) override the node's defaults for this flow alone.
- The model's context is cut to the var `context_chars` (default 6000)
  by expressions over the lists, with no function beyond the CEL lists
  and strings extensions (flow.md §4.5).
- `privacy: local-only` is satisfied because every node here is `local`
  and the model scheme is `ollama://` (flow.md §7.6, V19). A hosted model
  would need `cloud-allowed` and would send the same data off the
  machine.
- Whole-placeholder fields keep their types: `participants` is a list,
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
lowers_to: engine               # reshapes the response with `map`
description: >-
  Search the knowledge store, full-text, vector or hybrid, optionally narrowed
  by metadata. Returns a compact projection of each object, never its content.
request:
  method: POST
  url: "{{ config.ctxt.url }}/api/v1/find"
  headers: { Accept: application/json }
  json:
    query: "{{ in.query }}"
    mode: "{{ in.mode }}"
    limit: "{{ in.limit }}"
    filter: "{{ in.filter }}"
  auth: { secret: ctxt_token, scheme: bearer, optional: true }
response: { success: [200] }
inputs:
  query: { type: string, required: true, description: Search text; the server refuses an empty one. }
  mode: { type: string, enum: [fts, vector, hybrid], default: hybrid }
  limit: { type: integer, default: 10, minimum: 1, maximum: 100 }
  filter:
    type: object
    default: {}
    description: >-
      Metadata facets, all optional: meta_type, topic, person (an element of the
      object's metadata.people, a name as ctxt extracted it, not an address),
      source_type, and since and until (YYYY-MM-DD, compared with the dates the
      object mentions).
    properties:
      meta_type: { type: string }
      topic: { type: string }
      person: { type: string }
      source_type: { type: string }
      since: { type: string }
      until: { type: string }
outputs:
  from: body
  format: json
  map: >-
    {{ {'query': out.query, 'mode': out.mode, 'total': out.total,
        'objects': out.objects.map(o,
          {'id': o.id, 'type': o.type,
           'source': has(o.source) ? o.source : '',
           'created_at': o.created_at,
           'summary': has(o.summaries) && size(o.summaries) > 0 ? o.summaries[0] :
                      (has(o.text_content) ? (size(o.text_content) > 600 ? o.text_content.substring(0, 600) : o.text_content) : ''),
           'tags': has(o.tags) ? o.tags.map(t, t.label) : []})} }}
  schema:
    type: object
    required: [objects, total]
    properties:
      query: { type: string }
      mode: { type: string, description: The mode that ran; hybrid answers full-text only when the semantic leg is down. }
      total: { type: integer }
      objects:
        type: array
        items:
          type: object
          required: [id, type]
          properties:
            id: { type: string }
            type: { type: string }
            source: { type: string }
            created_at: { type: string }
            summary: { type: string, description: The first summary, else the first 600 characters of the text. }
            tags: { type: array, items: { type: string } }
side_effect: read
privacy: local
idempotent: true
capabilities: [knowledge.read]
timeout: 10s
retry: { max_attempts: 3, backoff: 2s }
```

```yaml
node: heo/conversation.list@1
kind: command
lowers_to: engine
description: >-
  Conversations with activity since a time in which one of the given people
  takes part, newest first. Snoozed ones included.
argv:
  - heo-conversations
  - --out
  - "{{ run.step_dir }}/out.json"
  - --since
  - "{{ in.since }}"
  - --status
  - "{{ in.status.join(',') }}"
  - --limit
  - "{{ in.limit }}"
  - --
  - "{{ in.participants }}"
inputs:
  participants:
    type: array
    items: { type: string }
    required: true
    description: Email addresses. Matched, case-insensitively, against the address of each identity heo knows.
  since:
    type: string
    default: 30d
    description: A lookback (30d, 12h, 2w) or an RFC 3339 time or date; heo keeps conversations whose last activity is after it.
  status:
    type: array
    items: { type: string, enum: [open, pending, resolved, closed] }
    default: []
    description: Empty is every status.
  limit: { type: integer, default: 25, minimum: 1, maximum: 100, description: At most this many conversations are returned. }
exit_codes: { ok: [0], retry: [70] }    # 70: heo's daemon is not running (transient)
outputs:
  from: file:out.json
  format: json
  schema:
    type: object
    required: [conversations, scanned]
    properties:
      conversations:
        type: array
        items:
          type: object
          required: [id, status]
          properties:
            id: { type: string }
            inbox: { type: string }
            status: { type: string }
            mode: { type: string }
            created_at: { type: string }
            last_activity: { type: [string, 'null'] }
            participants:
              type: array
              description: The identities that matched, with the address that matched.
              items:
                type: object
                properties:
                  identity: { type: string }
                  email: { type: string }
                  role: { type: string }
      scanned: { type: integer, description: Conversations read from heo. }
      matched_identities: { type: integer }
      truncated: { type: boolean, description: True when a page cap or the limit stopped the scan early. }
side_effect: read
privacy: local
idempotent: true
capabilities: [mail.read]
timeout: 60s
retry: { max_attempts: 3, backoff: 5s }
```

```yaml
node: tlc/task.list@1
kind: command
lowers_to: engine               # reshapes its output with `map`
description: Open tasks, across all projects, matching a free-text search.
argv:
  - tlc
  - task
  - list
  - --all-projects
  - --status
  - "{{ in.status.join(',') }}"
  - --limit
  - "{{ in.limit }}"
  - --format
  - json
  - --
  - "{{ in.search }}"
inputs:
  search:
    type: string
    default: ""
    description: Free-text query, one positional argument (after `--`, so a leading dash is text). Empty matches every task of the statuses.
  status:
    type: array
    items: { type: string, enum: [TODO, IN_PROGRESS, DONE, SKIPPED] }
    default: [TODO, IN_PROGRESS]
    description: Uppercase, as tlc spells them. tlc has no BLOCKED status.
  limit: { type: integer, default: 25, minimum: 1, maximum: 200 }
exit_codes: { ok: [0] }
outputs:
  from: stdout
  format: json
  map: >-
    {{ out.map(t,
        {'id': t.display_id, 'title': t.title, 'status': t.status,
         'priority': has(t.priority) ? t.priority : null,
         'due_at': has(t.due_at) ? t.due_at : null,
         'tags': has(t.tags) && t.tags != null ? t.tags : []}) }}
  schema:
    type: array
    items:
      type: object
      required: [id, title, status]
      properties:
        id: { type: string, description: The task display id. }
        title: { type: string }
        status: { type: string }
        priority: { type: [string, 'null'] }
        due_at: { type: [string, 'null'] }
        tags: { type: array, items: { type: string } }
side_effect: read
privacy: local
idempotent: true
capabilities: [tasks.read]
timeout: 15s
```

The three read nodes are the packs `packs/ctxt`, `packs/heo` and `packs/tlc`,
written against the tools as they are, not as a first draft assumed:

- **ctxt** takes one search `query` string (a list is a 400), a `mode`, a
  `limit` and a metadata `filter`; `filter.person` matches the names ctxt
  extracted into an object's `metadata.people`, not addresses. The response
  is `{query, mode, objects, total, diagnostics}` and each object carries its
  whole content, so the node projects `id`, `type`, `source`, `created_at`,
  one `summary` and the tag labels. The server is where `ctxt find` goes
  (`server.url` or the first `server.urls` entry of the ctxt config, else
  `http://127.0.0.1:8080`); the flo side is `packs.ctxt.url`. The token is the
  optional secret `ctxt_token`: sent when set, as a protected or public
  instance needs, else the request goes out without credentials.
- **heo** has no participant filter and no `--since`: `conversation list`
  takes `--updated-since` and lists identity ids, not addresses. The node
  runs the wrapper `heo-conversations`, which resolves the addresses through
  `heo identity list`, pages `conversation list --updated-since --sort
  last-activity --snoozed include` and keeps the conversations with a
  matching participant. It goes when heo ships `--participant`. heo exits 70
  without its daemon, which the node retries. A conversation has no subject
  or snippet: the node returns ids, status and activity.
- **tlc** takes the free-text search as a positional argument, statuses in
  upper case (`TODO`, `IN_PROGRESS`, `DONE`, `SKIPPED`; no `BLOCKED`) and
  must be told `--all-projects`: a read in a directory without a project
  can create a database there. Its output is a bare array, projected to
  `id` (the display id), `title`, `status`, `priority`, `due_at`, `tags`.

---

## 3. inbox-triage

Every 15 minutes: sync headers, judge each message on a model (two
questions over the headers), turn the verdicts into decisions by the
script's own policy, build a plan of what is automatic and what needs a
person, ask the person, record the decisions with the system that owns
trust, and apply.

```yaml
flow: inbox-triage
version: 0.2.0
description: Classify new mail, ask about what needs a person, apply the rest.
on:
  - schedule: { rrule: "FREQ=MINUTELY;INTERVAL=15" }
  - manual: {}
run_as: automation
privacy: local-only             # mail is private: the judge is a local model
tasks: per-run
concurrency: { on_overlap: skip }
vars:
  labels: { type: list, default: [Action Required, Waiting, Finance, Legal, Customers, Sales, Marketing, Operations, Vendors, People, Projects, Travel, Subscriptions, Reference] }
steps:
  # No lanes: fetch only reads and snapshots. The script's own state (seen
  # mail, pending locks, trust) lives between the steps, so they are ordered
  # with depends_on; the run id is the data they share.
  - id: fetch
    uses: inbox/fetch@1

  # The judgment: headers only, two questions per message. The model list
  # names Jev after the local model; under the local-only ceiling the engine
  # skips it (node.md §6.3.1). Opting in to Jev is a decision to let the
  # headers leave the machine: privacy: cloud-allowed on the flow and these
  # two steps.
  - id: rows
    uses: inbox/rows@1
    depends_on: [fetch]
    with: { run_dir: "{{ results.fetch.run_dir }}" }

  - id: label
    uses: flo/ai.classify@1
    privacy: local-only
    with:
      model: [ollama://qwen3:4b-instruct-2507-q4_K_M, jev]
      engine: auto
      labels: "{{ vars.labels }}"
      abstain: true             # routine filing does not clearly cover it: a person looks
      instructions: "Best label for filing this mail in the owner's mailbox? Abstain when …"
      items: "{{ results.rows.items }}"

  - id: weigh
    uses: flo/ai.classify@1
    privacy: local-only
    with:
      model: [ollama://qwen3:4b-instruct-2507-q4_K_M, jev]
      engine: auto
      labels: [irrelevant, low, normal, high, critical]
      instructions: "How much does this mail matter to the owner? …"
      items: "{{ results.rows.items }}"

  # The script's decider policy over the verdicts: low confidence is Needs
  # Review, an abstention stays in the inbox, quiet mail is also read and
  # archived, mail from a person is never filed away. Writes decisions.json.
  - id: decisions
    uses: inbox/decisions@1
    with:
      run_dir: "{{ results.fetch.run_dir }}"
      labels: "{{ results.label.items }}"
      weights: "{{ results.weigh.items }}"
      sop_labels: "{{ vars.labels }}"

  - id: plan
    uses: inbox/plan@1
    depends_on: [decisions]
    with: { run: "{{ results.fetch.run }}" }

  # Everything apply would touch goes through the approval, so the guard of the
  # irreversible step always finds a decision: the script's automatic items
  # (every action at a trusted level) are decided by trust and cost no one a
  # look; only the pending ones park the run. Nothing to do skips both.
  - id: approve
    uses: flo/approval@1
    when: "{{ size(results.plan.pending) + size(results.plan.auto) > 0 }}"
    with:
      title: Triage the inbox
      items: "{{ results.plan.pending + results.plan.auto }}"
      verdicts: [approve, reject]
      trust: inbox
      surface: tlc
      context: { run: "{{ results.fetch.run }}", run_dir: "{{ results.plan.run_dir }}" }

  # Runs only when something was approved or automatic: a run whose pending
  # items were all rejected applies nothing.
  - id: apply
    uses: inbox/apply@1
    when: "{{ size(results.approve.approved) + size(results.approve.auto) > 0 }}"
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
- **A configurable judge behind a privacy ceiling.** The decision is not a
  hosted service wrapped in a command: it is `flo/ai.classify@1` over kit's
  judge, with the engine (`local`, `jev`, `auto`) as configuration
  (`ai.judge.engine`; node.md §6.3). The mail's headers are private, so the
  steps run `local-only` and the engine never picks Jev, however the list is
  written; with no local model detected the step fails (`privacy_violation`)
  rather than send mail anywhere. Choosing Jev for mail is the owner's
  explicit act, made in the flow's `privacy`, not a runtime fallback.
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
  an ancestor.
- **Items that trust decides still go through the approval.** `approve`
  is given `plan.pending + plan.auto`: the items a person must decide and
  the ones trust already decided (every action at a trusted level). With
  `trust: inbox` the node decides the automatic items itself and parks the
  run only for the pending ones, so they cost no one a look, yet the
  irreversible guard always finds a decided approval for every item
  `apply` will touch (flow.md V20). `approve` is skipped only when
  there is nothing at all (`size(pending) + size(auto) == 0`), and `apply`
  runs only when something was approved or automatic: a run whose pending
  items were all rejected applies nothing. At dispatch the engine still
  checks that a decision or an `auto` record exists for the run's items. A
  skipped approval is not treated as a guard: that alternative was rejected
  because it weakens the guard, which would then hold for a run that
  decided nothing.
- **Failures in the output, not the exit code.** The script exits 0 when
  accounts fail or actions fail. The nodes read a JSON summary and fail the
  step with `fail_when` (node.md §3.2).
- **A trust provider.** `trust: inbox` names a provider declared below:
  `read` marks pending items whose actions are already trusted as `auto`;
  `decide` records the person's decisions with the script, which owns the
  counters; `set` backs `flo trust set`. Each approval item names the
  operation it approves (`capability`, `args`; a reply is `mail.send`, the
  rest `mail.write`), so the engine keeps it as a proposal under its
  request digest.
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
