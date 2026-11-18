# GitHub probe: calls for the dependency watcher

Probed 2026-11-18 with `gh` 2.x, run through `aps run <profile> -- gh ...`.
GET/POST-graphql-query only; nothing created. Targets: public repos
(`spf13/cobra`, `cli/cli`, `golang/net`, `charmbracelet/bubbletea`,
`hop-top/{tlc,aps,kit,poly-kit}`). Fixtures: `testdata/github/` (trimmed,
public data, no headers or tokens).

## Summary: recommended call set

| Need | Call | Cost |
|---|---|---|
| Dependency list (all ecosystems, one call) | `GET /repos/{o}/{r}/dependency-graph/sbom` | 1 of 100/h `dependency_sbom` bucket |
| Dependency list with direct/transitive + package-to-repo | GraphQL `dependencyGraphManifests` (paginate manifests AND dependencies) | 1 point per page-ish; 5000/h `graphql` |
| Releases since version | `GET /repos/{o}/{r}/releases?per_page=100` + `If-None-Match` | 1 core; 304 is free |
| Fallback: no releases | GraphQL `refs(refPrefix:"refs/tags/")` with commit date | 1 graphql point |
| Advisories, any package | `GET /advisories?ecosystem=go&affects=<pkg>@<ver>` | 1 core per page |
| Advisories, owned repos | `GET /orgs/{org}/dependabot/alerts?state=open` | 1 core per page, all repos in one list |

Watcher shape: SBOM (or GraphQL) per repo, dedupe packages across repos
first, then one `/advisories?affects=a@v,b@v,...` call per batch, releases
only for packages whose source repo is known. Total per run for 4 repos and
~200 unique modules: 4 SBOM + ~2 advisories + N releases.

## Auth and scopes

- Any token works for public data. 5000 core/h, 5000 graphql points/h
  (authenticated); unauthenticated is 60/h, unusable.
- Token used had `repo`, `workflow`, `admin:org` etc. Minimum for this call
  set: public-repo data needs no scope. Private repos need `repo`
  (`X-Accepted-OAuth-Scopes: repo` on SBOM). Dependabot alerts need
  `security_events` (or `repo`) AND repo access with alerts enabled; org
  endpoint needs org owner or security manager.
- Profile finding: the aps profile used has no GitHub secrets in its env;
  `gh` falls back to the host keyring, so the profile is identity-neutral
  for gh today. If the flow must isolate GitHub identity, inject
  `GH_TOKEN` via profile secrets (not tested).
- aps gotcha: `aps run` writes `[exec] gh` / `[exit] gh ok` envelope lines
  into the output stream and applies secret redaction to child stdout.
  Redaction produced a false positive inside SBOM JSON (the copyright text
  `...The Go Authors","externalRefs":[...` became a `<generic-api-key>`
  placeholder), which yields invalid JSON. Workaround that returned
  byte-identical data: `aps run <p> -- sh -c 'gh api ... > file'` and read
  the file. The flo gh node must not parse aps stdout directly.
- `gh api` exits non-zero on 304 (`gh: HTTP 304`) and on every 4xx;
  inspect stderr/status, do not treat as crash.

## 1. Dependency graph

### 1a. SBOM (REST)

```
gh api /repos/spf13/cobra/dependency-graph/sbom
```

Rate limit: separate resource `dependency_sbom`, **100/hour** (shown in
`X-RateLimit-Resource`). A 404 also consumes one. No pagination, one flat
SPDX 2.3 document, full transitive set across all manifests in the repo
(cobra: 11 packages; hop-top/tlc: 180 packages, 170 golang).

Shape (fixture `sbom_spf13_cobra.json`):

```
{ sbom: {
    spdxVersion, dataLicense, SPDXID, name, documentNamespace,
    creationInfo: { creators[], created },
    packages: [ { name, SPDXID, versionInfo, downloadLocation, filesAnalyzed,
                  licenseConcluded?, copyrightText?,
                  externalRefs: [ { referenceCategory:"PACKAGE-MANAGER",
                                    referenceType:"purl",
                                    referenceLocator:"pkg:golang/github.com/spf13/pflag@1.0.9" } ] } ],
    relationships: [ { spdxElementId, relatedSpdxElement, relationshipType } ] } }
```

Notes:

- Ecosystem lives only in the purl: `pkg:golang/`, `pkg:githubactions/`,
  `pkg:npm/`, `pkg:pypi/`, `pkg:cargo/`, `pkg:github/` (the root package).
  Filter on purl type; drop `githubactions` unless wanted.
- Package list includes the repo itself (`com.github.<o>/<r>`, version
  `main`, purl `pkg:github/...`). Skip it.
- `versionInfo` for Go: cobra's own SBOM showed `1.1.0` (no `v`), tlc's
  showed `v0.9.8` (with `v`). Normalize before comparing.
- Action versions can be ranges: `4.*.*`, purl-encoded `4.%2A.%2A`.
- Relationships are DEPENDS_ON edges root to direct and dep to dep. No
  direct/indirect flag, no source-repo mapping.
- Same package at several versions appears once per version.

### 1b. GraphQL `dependencyGraphManifests`

Query: `testdata/github/query_dependency_manifests.graphql` (variables `o`,
`n`, `after`). Run:

```
gh api graphql -f query="$(cat q.graphql)" -f o=hop-top -f n=tlc
```

Shape (fixture `graphql_dependency_manifests_tlc.json`):

```
data.repository.dependencyGraphManifests: { totalCount, pageInfo{hasNextPage,endCursor},
  nodes: [ { filename, parseable, exceedsMaxSize, blobPath, dependenciesCount,
             dependencies: { totalCount, pageInfo, nodes: [
               { packageName, packageManager:"GO", packageUrl:"pkg:golang/cel.dev/cel-go@v0.33.0",
                 requirements:"= v0.33.0", relationship:"direct"|"transitive",
                 hasDependencies, repository:{nameWithOwner:"cel-expr/cel-go"} } ] } } ] }
```

Cost: `rateLimit{cost}` reported 1 for a one-manifest, 100-dependency page.
Fields available beyond the first probe: `relationship`, `packageUrl`
(schema introspection of `DependencyGraphDependency`).

Notes:

- Two nested connections; both paginate. hop-top/tlc `go.mod` has 166
  dependencies, page of 100 returned `hasNextPage:true`.
- Per-manifest, so a monorepo repeats packages across manifests:
  hop-top/poly-kit has 48 manifests (go.mod, uv.lock, Cargo.lock,
  pyproject, workflows). A single query asking `first:20` manifests x
  `first:100` dependencies returned HTTP 502 for poly-kit; ask for fewer
  nodes (`first:1` dependencies worked) and page.
- `requirements` is a string like `= v0.33.0`; strip `= `.
- `repository.nameWithOwner` maps package to its GitHub source repo (may
  differ from the module path, e.g. `cel.dev/cel-go` to `cel-expr/cel-go`).
  This is the piece SBOM lacks and the releases call needs.
- Missing repo: `data.repository:null` + `errors[0].type=NOT_FOUND` (fixture
  `graphql_repository_not_found.json`). Graph disabled or no manifests:
  `totalCount:0, nodes:[]`, no error (fixture
  `graphql_dependency_manifests_empty.json`).

### Comparison

| | SBOM REST | GraphQL manifests |
|---|---|---|
| Calls per repo | 1 | 1 + pages (manifests x deps) |
| Budget | 100/h dedicated | 5000/h shared points |
| direct vs transitive | no (only root edges) | `relationship` |
| package to source repo | no (purl only) | `repository.nameWithOwner` |
| manifest of origin | no | `filename` |
| 404 / disabled | 404, same as missing repo | empty list, missing repo distinguishable |
| Output size | 1 flat doc | needs merging, dedupe |

Recommendation: SBOM as default for the watch list (cheap, 1 call, complete
transitive set); GraphQL when direct-only or source-repo mapping is needed.
For Go, the module path to GitHub repo mapping can also be computed
(`github.com/o/r[/sub]`) or resolved via the Go proxy for vanity paths
(`golang.org/x/net` is repo `golang/net`).

### Edge cases

- `hop-top/kit` (public mirror, dependency graph not enabled): SBOM
  `404 Not Found` (fixture `sbom_404.json`), identical to a nonexistent
  repo. `GET /repos/{o}/{r}` returns 200, so confirm existence there.
  `/dependency-graph/compare/...` returns 403 for it. GraphQL returns
  `totalCount:0`. Watch list should use the canonical source repo
  (`hop-top/poly-kit`, default branch `next`), not mirrors.
- Default-branch only: both calls describe the default branch.
- Graph is eventually consistent after pushes; `created` in SBOM is request
  time, not graph time.

## 2. Releases since a version

```
gh api '/repos/cli/cli/releases?per_page=100'            # one page
gh api '/repos/cli/cli/releases?per_page=100' --paginate # follow Link rel=next
```

Core rate limit, 1 per page. cli/cli: 212 releases = 3 pages of 100.
Pagination: `Link` header (`rel="next"`, `rel="last"`), page-number based.
Order: **created_at descending, not semver**. Stop paginating when you hit
the `since` tag; do not sort by name.

Conditional requests: send `If-None-Match: <ETag>` (weak etag from previous
response). 304 is not counted against the rate limit (remaining unchanged).
`gh api` exits 1 with `gh: HTTP 304`. Response also has
`Cache-Control: private, max-age=60`.

Shape (fixture `releases_cli_cli.json`, trimmed from the full object):

```
[ { id, url, html_url, tag_name, name, draft, prerelease, immutable,
    created_at, published_at, target_commitish, body, assets[], author{}, ... } ]
```

Full object keys: assets, assets_url, author, body, created_at, draft,
html_url, id, immutable, mentions_count, name, node_id, prerelease,
published_at, reactions, tag_name, tarball_url, target_commitish,
updated_at, upload_url, url, zipball_url.

Prerelease and drafts:

- `prerelease:true` entries are interleaved with stable ones
  (bubbletea: 84 releases, 10 prerelease, e.g. `v2.0.0-rc.2`). Filter
  client-side with the semver prerelease part and/or `prerelease`.
- Drafts are returned only to tokens with push access; none seen. Always
  skip `draft:true`.
- `GET /releases/latest` ignores prereleases and drafts and returns 404
  when none qualifies (`hop-top/poly-kit`: every release is a prerelease,
  fixture `release_latest_404.json`). Do not use `latest` for the watcher.
- Tags can carry a module prefix in monorepos: poly-kit tags are
  `kit/v0.5.0-beta.14`, `kit-py/...`, `mcp-tasks/...`, `qmochi/...`. Go
  submodule versions use exactly this convention (`<subdir>/vX.Y.Z`). Match
  the tag prefix to the package path before comparing versions.

### No releases: tags fallback

`golang/net` has zero releases (`[]`, fixture `releases_empty.json`) but 63
tags.

REST: `GET /repos/golang/net/tags?per_page=100` (fixture
`tags_golang_net.json`): `[ { name, commit:{sha,url}, zipball_url,
tarball_url, node_id } ]`. No dates. Order observed newest-first by name
here, but it is ref order, not guaranteed semver. Paginates with Link.

GraphQL gives dates in the same call (fixture
`graphql_releases_refs_golang_net.json`, cost 1):

```
repository{ releases(first:N,orderBy:{field:CREATED_AT,direction:DESC}){totalCount nodes{tagName isPrerelease isDraft publishedAt url}}
            refs(refPrefix:"refs/tags/",first:N,orderBy:{field:TAG_COMMIT_DATE,direction:DESC}){
              totalCount pageInfo{hasNextPage endCursor}
              nodes{name target{__typename ... on Commit{committedDate oid} ... on Tag{tagger{date} target{... on Commit{committedDate oid}}}}}}}}
```

Annotated tags: `target.__typename` is `Tag`; date at `tagger.date`.
Lightweight: `Commit.committedDate`. Recommendation: one GraphQL query
fetching both `releases` and `refs`; use releases if `totalCount>0`, else
refs. Release notes only exist for releases; for tag-only repos the
classifier has no notes (could use commit compare, not probed).

## 3. Advisories

### 3a. Global advisories

```
gh api -X GET /advisories -f ecosystem=go -f affects=golang.org/x/net@0.17.0 -f per_page=100 --paginate
```

Core rate limit, 1 per page. Pagination is **cursor-based** via `Link`
`rel="next"` with an `after` cursor (no page numbers, no `last`).

`ecosystem` values are lowercase and fixed: `rubygems, npm, pip, maven,
nuget, composer, go, rust, erlang, actions, pub, other, swift`. `Go` and
`golang` both return 422 (fixture `advisories_bad_ecosystem.json`).
GraphQL uses enum `GO` (uppercase). purl type `golang` maps to `go`; purl
`pypi` maps to `pip`; `cargo` maps to `rust`; `githubactions` maps to
`actions`.

`affects`: `pkg` or `pkg@version`, comma-separated list (22 results for two
packages in one call). Version filters correctly (`golang.org/x/net@0.17.0`
returned 4, `@0.63.0` returned 0, bare name 21); a leading `v` was
accepted (same 4). Use the module path as in go.mod.

Other filters seen to work: `updated=>=2026-11-01` (date comparison
syntax), `cve_id`, `type` (default `reviewed`; `unreviewed`, `malware`
exist), `severity`. Sort/direction are available. For incremental watch use
`updated=>=<last run>`.

Shape (fixture `advisories_go_x_net.json`, description/credits/references
removed): keys `ghsa_id, cve_id, url, html_url, summary, description,
type, severity, source_code_location, identifiers[], references[],
published_at, updated_at, github_reviewed_at, nvd_published_at,
withdrawn_at, vulnerabilities[], cvss, cvss_severities{cvss_v3,cvss_v4},
cwes[], epss{percentage,percentile}, credits[], comments, repository_advisory_url`.

`vulnerabilities[]`: `{ package:{ecosystem:"go",name}, vulnerable_version_range:"< 0.55.0",
first_patched_version:"0.55.0" (string), vulnerable_functions[] }`.
Note `first_patched_version` is a plain string here and an object
(`{identifier}`) in Dependabot alerts and GraphQL. `severity` is lowercase
here (`medium`), uppercase in GraphQL (`MODERATE`). `withdrawn_at` non-null
means skip.

GraphQL alternative: `securityVulnerabilities(ecosystem:GO, package:"...",
first:N)` (fixture `graphql_security_vulnerabilities_go_x_net.json`, cost
1): per-package, no `affects` version filter, so version matching is
client-side from `vulnerableVersionRange`. REST `affects` list is better
for batches.

### 3b. Dependabot alerts (owned repos)

Per repo: `GET /repos/{o}/{r}/dependabot/alerts?state=open&per_page=100`.
Per org: `GET /orgs/{org}/dependabot/alerts?state=open&per_page=100`
(Link cursor pagination; filters `ecosystem`, `severity`, `package`).
Core rate limit.

- Org call returns every repo's alerts in one list incl. private repos
  (`repository` object, large owner payload). One call replaces N. Do not
  persist private repo data in fixtures.
- Repo with alerts disabled or token without access: `403`
  (`You are not authorized to perform this operation`, fixture
  `dependabot_alerts_403.json`; also what a public foreign repo returns).
  Repo with alerts on and none open: `[]` (hop-top/tlc).
- Needs scope `security_events` or `repo` plus alerts enabled.

Shape (fixture `dependabot_alerts.json`, trimmed): `number, state,
dependency{package{ecosystem,name}, manifest_path, version_requirement,
scope, relationship}, security_advisory{ghsa_id,cve_id,summary,severity,
published_at,updated_at,withdrawn_at,vulnerabilities[],cvss,...},
security_vulnerability{package,severity,vulnerable_version_range,
first_patched_version{identifier}}, html_url, created_at, updated_at,
fixed_at, dismissed_at/by/reason/comment, auto_dismissed_at, reachability`.

Dependabot alerts add what the global feed cannot: the alert is already
matched to the repo's real manifest and version, with a state
(open/dismissed/fixed). Use them as the authoritative source for owned
repos and `/advisories?affects=` for non-owned or cross-checking.

## Rate-limit table (measured)

| Resource | Limit/h | Used by |
|---|---|---|
| `core` | 5000 | releases, tags, advisories, dependabot alerts |
| `graphql` | 5000 points | manifests, refs, securityVulnerabilities (cost 1 each probed) |
| `dependency_sbom` | 100 | SBOM (incl. 404s) |
| `dependency_snapshots` | 100 | not used |

`GET /rate_limit` is free and lists all buckets.

## Gotchas for the flow design

1. Watch list must name source repos, not mirrors (kit mirror: SBOM 404).
2. SBOM bucket is 100/h; fine for 4 repos. SBOM sends an ETag but a
   conditional request still returned 200 and cost 1 (the document
   namespace and `created` change per request), so cache by content hash
   of `packages`, not by ETag. Releases conditional requests do work (304,
   free).
3. Ecosystem mapping needed: purl type to advisory ecosystem
   (`golang`->`go`, `pypi`->`pip`, `cargo`->`rust`, `githubactions`->`actions`).
4. Module path to source repo is not in SBOM; get it from GraphQL
   `repository.nameWithOwner` or compute from the module path.
5. Releases order is creation time; tag prefixes in monorepos; prereleases
   interleaved; `releases/latest` unreliable.
6. Tag-only repos have no notes and no REST dates; use GraphQL refs.
7. Version strings differ across endpoints (with/without `v`, `= ` prefix,
   string vs `{identifier}` for patched version, case of severity).
8. aps stdout redaction can corrupt JSON; write to a file inside the
   profile shell.
9. `gh` exit codes: non-zero for 304 and 4xx; 404 on SBOM is ambiguous
   (missing repo vs graph disabled), disambiguate with `GET /repos/{o}/{r}`
   or use GraphQL.
