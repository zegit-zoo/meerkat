# The copied-authority refactor workflow

meerkat-mob issue I (#9), requirements MK-VND-01 to MK-VND-04. A
design, not a shipped feature: it records the workflow the librarian
and its sibling roles run when a knowledge base has been copying the
body of an **authority** and that authority now answers live through
tools. Every step names the meerkat command or librarian action that
performs it today; what the code does not yet do is listed under
[Needed in meerkat](#needed-in-meerkat).

An authority is whoever owns the answer: a vendor with a hosted MCP
server, a library whose current documentation a docs server such as
Context7 serves, an open-source project that runs its own docs server,
another team's internal service. The workflow is the same for each;
only the inputs differ. Datadog is the first run and the
[worked example](#worked-example-datadog-the-first-run).

## Why

meerkat holds the **fastest route** to an answer, never the
authoritative body of it (`AGENTS.md`, section 6: "Never quote a
vendor's authority when the vendor exposes it as a tool"; the rule is
phrased for vendors and holds for any authority). Content repositories
drift from that rule in a predictable way: an agent researches an
authority once, the answer is filed as a page, and the page is then
served as if it were the authority's documentation. It is not. It is a
snapshot of it, taken on one day, by one reader, and it goes stale the
next time the authority ships.

An authority that exposes itself as tools makes that copy strictly
worse than a pointer. A live server answers from the current
documentation, knows the current field names and schemas, and can often
act on the answer. A copied page can do none of those, and when it
disagrees with the authority it is simply wrong.

The model for the replacement is a **capability search** on the
authority's own surface: one question ("I have to do X") answered with
the relevant docs, skills and examples in one hit. That is exactly the
tier-0 answer shape meerkat already has — a hub search returning
[capability bundles](links.md#wire-shapes) grouped by pointer target.
An authority KB built in that shape routes an agent to the authority in
one hop and carries only what the authority cannot tell it: which
capability to reach for, how to connect, and the quirks we observed
ourselves.

## Inputs

| Input | What it is |
|---|---|
| Authority name | the collection name of the new authority KB and the server name in `mcp://<authority>` |
| MCP server | endpoint(s) and how capabilities are selected at connect time (toolsets, scopes, parameters) |
| Docs tool | the authority's own search over its documentation, or the docs server's query tool for it |
| Ask-then-do tools | tools that return a spec or schema before a write, where the authority has any |
| Skills | agent skills the authority publishes, if any |
| CLI (optional) | the fallback when no MCP client is available |
| Content collections | the collection(s) holding copied pages, and each one's update contract |

When a shared docs server answers for many authorities (one server, one
library per query), the pointer's `target:` names that server and the
`hint:` names the library; the authority KB is still one per authority.

The table deliberately stops at names. Toolset lists, tool parameters
and limits are the authority's own and change without notice; step 2
reads them from the live surface on every run instead of from this
page.

## The shape of an authority KB

- **Placement.** In a tree deployment, a leaf knowledge base at tree
  path `root/<group>/<authority>` (collection name `<authority>`, a
  `mount: lazy` child of a hub grouping such KBs, `vendors` in
  [tree.md](tree.md)'s example). In a flat deployment, a collection
  named `<authority>`. Collection names are unique across the tree, so
  `collection:<authority>` is an unambiguous pointer target from the
  root.
- **Pages.** At most ~30, of three kinds ([links.md](links.md#page-types)):
  - `type: pointer`, `target: mcp://<authority>`, one per task class,
    listing the server's capabilities in MCP vocabulary (`tools:`,
    `resources:`, `prompts:`, `toolsets:`) and a `hint:` of one
    sentence (≤ 300 characters, MK-LNK-02) that uses the words agents
    ask with;
  - `type: skill`: the connect recipe, the "ask the authority's docs
    tool first" rule, one page per published skill worth installing;
  - `type: example`: a worked call sequence, never a copied payload.
- **Links.** Each pointer names its skills and examples in `related:`,
  so `mk_search bundle=true` groups them under the pointer's target.
- **Ranking.** An authority KB ranks like a **hub** in the sense of
  [links.md](links.md#ranking): its pointers hop out to the authority's
  server, so the pointer should outrank thin content. meerkat reads the
  role from the deployment's shape (#95), and both placements above are
  leaves by shape, which rank with no type boost. The authority KB
  therefore states the hub weights itself in its `content-source.yaml`
  entry, `search: {type_boosts: {pointer: 4, skill: 2, example: 1.5}}`,
  and never `{pointer: 1.0}`, which is the leaf default for pointers
  that are citations.
- **Auth** is instructions, never material: which flow to use and which
  permission scopes to request. No keys, tokens, org names or site
  URLs specific to a deployment (MK-LNK-03).
- **Fair use** is one skill page naming the limits the authority
  publishes, with a short `stale_after`, so the librarian re-checks it.
- **Kept quirks** (class b below) live here too, each with a
  `verified:` entry from the last re-check and a `stale_after`.

A pointer page, in the fields `kb.Frontmatter` has today, for a
placeholder authority `example-authority`:

```yaml
---
id: pointers/reports
title: Example Authority reports
type: pointer
target: mcp://example-authority
hint: Build or change Example Authority reports; ask get_report_schema for the schema before writing, never copy one.
toolsets: [reports]
tools: [get_report_schema]
related: [skills/connect, skills/docs-first, examples/report-from-query]
tags: [example-authority, reports]
stale_after: 2027-03-31
---
```

Toolset and tool names in a page are routing hints, not a contract;
the validator re-checks them against the live server (step 3).

## The workflow

Five steps. Each produces a report before it changes anything, and
every change goes through the owning collection's update contract
([update-contract.md](update-contract.md)).

### 1. Inventory the pages that copy the authority

| Action | Command |
|---|---|
| list candidate pages by facet | `mk list --category <authority> --json`, `mk list --prefix <path>/ --json` per collection (`--collection <name>`) |
| find pages that cite the authority's docs | `mk search "<authority domain>" --json`; in the content working copy, `grep -rl '<authority domain>' wiki/` for bodies the tokenizer splits |
| check what already links there | `mk show <id> --json` (`linked_from`) |

Each page is classified:

- **(a) copied authority docs** — API field tables, schema or query
  definitions, parameter lists, limits, anything the authority's docs
  tool returns for the same question. Archived in step 4.
- **(b) self-observed quirk** — behaviour we saw that the authority's
  documentation does not state (a search that does not match what it
  should, a merge the API performs silently, a client that rejects the
  tool schemas). Kept, re-verified, given `stale_after`.
- **(c) own runbook or decision** — our procedures and choices that use
  the authority. Kept where it is; its authority-specific steps become
  links to the authority KB's pointers.

The classification is evidence-based: a page is (a) only when a call
to the authority's surface answers the same question, and the finding
records which call. The inventory result names internal pages and is
filed in the private tracker, never in a public repo.

### 2. Discover the authority's surface

An executor agent with the authority's MCP server connected (read-only
toolsets and read-only scopes) enumerates what the server offers in
the protocol's own terms: `tools/list`, `resources/list`,
`prompts/list`, the toolsets selectable at connect time, plus the
authority's published skills and examples. It then runs the
authority's capability search (or its docs tool, where there is no
capability search) for each task class the inventory found ("build a
report", "configure retries", "rotate a credential") and records which
tools, skills and examples come back.

meerkat itself never connects to an external MCP server; the executor
(`mk ingest`'s agent CLI) does. The output is a capability map
deposited as research (`mk_report_outcome` with a fallback, or a raw
intake item), which is what the researcher role turns into pages.

### 3. Build the capability-pointer KB

| Action | Command |
|---|---|
| draft pages from the capability map | `mk ingest --role researcher --from intake` (target KB `<authority>`) |
| confirm each page against the live server | `mk ingest --role validator --from intake` twice, distinct models (Q7) |
| file confirmed pages | `mk ingest --role librarian --apply` through the authority KB's contract |
| check shape and links | `mk lint` in the authority KB's CI (invalid pointer, dangling `related:`) |

The validator's brief for an authority KB (a content-repo override,
`ingestion/prompts/validator.md`) adds: every `tools:` and `toolsets:`
entry exists on the server today; the hint names a real task class;
the body quotes no field table, schema or payload; no credential,
org identifier or deployment URL appears. The ~30-page cap is a
review rule; the size itself is visible in `mk list --collection
<authority>`.

The root gets one pointer, `<authority>` → `collection:<authority>`,
whose hint carries the authority's everyday vocabulary so a root search
for one of its topics lands on it. It is filed exactly like a promotion
([intake.md](intake.md#the-librarian)): a `direct` root gets
`global/pointers/<authority>.md` in its overlay, a `merge-request` root
gets the page spelled out in the instructions.

### 4. Archive the copies, keep the quirks

Class (a) pages move to `_archive/<authority>/<original-id>.md` with:

```yaml
status: archived
failure_reason: superseded-by-vendor-mcp
related: [<authority>:<pointer-id>]
```

The `failure_reason` value keeps the name issue #9 gave it; it marks
any page an authority's live tools superseded, vendor or not.

Nothing is deleted (MK-DAT-01, MK-LIB-03); the archive keeps the
history and the link to what replaced it.

**Where the archive lives.** Today every page under the served `wiki/`
directory is indexed, whatever its status. Until meerkat has an
archive-aware exclusion (below), `_archive/` sits **beside** `wiki/`
in the content repository, not inside it: browsable in the forge, not
served, not indexed. A move out of the served tree is a content-repo
change, so the archive step always travels as a merge request on the
repository that owns the copied pages, whatever the collection's
contract says for new pages; `direct` writes land in a memory overlay
and cannot move a page.

Class (b) pages are re-verified against the current server (the quirk
may be fixed), given a fresh `verified:` entry and `stale_after`, and
moved into the authority KB with a `related:` entry to the pointer they
qualify. Class (c) pages stay; their copied steps are replaced by
links.

### 5. Measure, then decide what remains

MK-VND-03 asks for time to first relevant context before and after,
over one week each side.

- **Window.** Seven days of retrieval before the root pointer is filed,
  seven days after the archive merge lands.
- **Sessions.** Those whose initial query names the authority or whose
  attempted path includes the authority's collections, selected from
  the traversal log by the librarian. The initial query is in the log
  only when `observability.traversal_log` says `query: plaintext`
  (#124), so a deployment that measures a run opts in before the first
  window opens; the default 90-day retention covers both windows.
  Collection names are matched by hashing the registry's own names, as
  the missing-link analysis does.
- **Signals.** Outcome (`found` versus `gave_up`), hops, wrong turns
  and the planner stages that answered — all in the log today — and
  time to first relevant context, which today exists only as a global
  histogram (MK-SES-03) and has no per-session value in the log (see
  below).
- **Probe set.** Because meerkat cannot see the call to the authority
  that follows a pointer, an agent also runs a fixed set of ten
  questions about the authority before and after, timing end to end
  (meerkat plus the authority's tool).

The decision is a human's: any remaining page whose question the
authority's docs tool answers in one call is archived too; what remains
is the pointers, the connect recipe, the limits and the quirks the
authority's tools get wrong.

## Roles

| Step | Runs it | How a change travels | Human decides |
|---|---|---|---|
| 1 inventory | librarian (report); researcher classifies with evidence; validator confirms (a) | nothing changes | disputed classifications (parked `needs-human`) |
| 2 discover | researcher's executor, read-only scopes | intake deposit | which authority, which toolsets may ever be write-enabled |
| 3 build | researcher drafts, validator confirms twice, librarian files | authority KB contract (`merge-request` recommended for a new KB); root pointer through the root's contract | review of the authority KB merge request |
| 4 archive | librarian proposes the moves | always a merge request on the owning content repo | merging it |
| 5 measure | librarian reads the traversal log; an agent runs the probe set | report only | whether any copied page remains |

The librarian never applies without `--apply`, and `--apply` never
does more than the contract allows (`none` names the change for a
human to place).

## Acceptance

From MK-VND-02, checked after each run:

- **No served page quotes the authority's field tables or schema
  definitions.** The validator's brief checks each authority-KB page;
  the inventory grep from step 1, re-run over the served tree, returns
  only pointers, skills, examples, quirks and runbooks.
- **A root search for one of the authority's topics returns the pointer
  first.** `mk search "<authority> <topic>" --json` against the root:
  the first hit has `kind: pointer`. In a tree the root is the only
  collection an unqualified search asks, so it is the root's pointer
  (`target: collection:<authority>`); in a flat deployment an
  authority-KB pointer (`target: mcp://<authority>`) is an equally good
  first hit. The pointer boost (×4, [links.md](links.md#ranking)) and
  the indexed `hint:` make this hold whenever the hint uses the query's
  words. A tree root gets the boost as a hub by default; a flat
  authority KB gets it only from the `type_boosts` it states (see
  [the shape](#the-shape-of-an-authority-kb)).
- **The archive is browsable but not indexed.** No `_archive/` page
  appears in `mk list` or `mk search` on any mounted collection; the
  pages are readable in the content repository.

## Needed in meerkat

Gaps found while writing this; each is a follow-up, none blocks a first
run.

- **Archive-aware exclusion.** Index and list skip `status: archived`
  (or an `_archive/` prefix) while `mk show` still reaches the page, so
  the archive can live inside the served tree. Seam:
  `collections.Collection.Index()` and `search.NewFromPages`, the
  index-time filter assessed in [index-filtering.md](index-filtering.md).
- **Copied-authority lint.** A warn-only `mk lint` rule flagging pages
  that cite a configured authority domain and carry field tables or
  schema blocks. Seam: `internal/cli/lint.go`, beside
  `Registry.LinkReport()`.
- **Librarian authority mode.** `mk ingest --role librarian
  --authority <name>` emitting the step-1 inventory as findings and the
  step-4 moves as archive proposals. Seam: finding kinds and `Apply` in
  `internal/ingest/librarian.go`; moves stay merge requests.
- **Per-session time to first relevant context.** A duration on
  `traversal.Entry`, so MK-VND-03 can be measured for one authority's
  sessions; the histograms cannot carry a collection label under the
  disclosure rule. Seam: `internal/traversal` and the retrieval session.

## Worked example: Datadog, the first run

Datadog is the first authority the workflow runs against, and the one
the issue was written from. Its hosted MCP server has a capability
search of exactly the shape above: it answers "I have to handle
security signals" with the relevant docs, skills and examples in one
hit.

| Input | Datadog |
|---|---|
| Authority name | `datadog` |
| MCP server | `https://mcp.datadoghq.com/v1/mcp`, EU `mcp.datadoghq.eu`; toolsets chosen with `?toolsets=…` (`core` is the default) |
| Docs tool | `search_datadog_docs` (per the issue's sources) |
| Ask-then-do tools | `get_widget_reference`, `ddsql_get_spec` |
| Skills | `datadog-labs/agent-skills` (MCP mode, CLI fallback) |
| CLI (optional) | `pup` |
| Content collections | inventory result, kept private |

Public sources:
<https://docs.datadoghq.com/mcp_server/tools/>,
<https://www.datadoghq.com/blog/engineering/mcp-server-agent-tools/>,
<https://github.com/datadog-labs/agent-skills>,
<https://github.com/DataDog/pup>. The server itself is closed; its
docs and examples are public (`datadog-labs/mcp-server`, MIT).

A Datadog pointer page:

```yaml
---
id: pointers/dashboards
title: Datadog dashboards and widgets
type: pointer
target: mcp://datadog
hint: Build or change Datadog dashboards; ask get_widget_reference for the widget schema before writing, never copy one.
toolsets: [dashboards]
tools: [get_widget_reference]
related: [skills/connect, skills/docs-first, examples/dashboard-from-query]
tags: [datadog, dashboards, widgets]
stale_after: 2027-03-31
---
```

The run, step by step:

- [ ] Inventory: `mk list --category datadog --json` per collection and
      a body grep for `datadoghq`; classify a, b or c with the Datadog
      call as evidence; file the result in the private tracker.
- [ ] Discover: connect `https://mcp.datadoghq.com/v1/mcp` (or
      `mcp.datadoghq.eu`) with `core` and read-only scopes; enumerate
      tools, resources, prompts and toolsets; list the skills in
      `datadog-labs/agent-skills`; run the capability search per task
      class (security signals, dashboards, logs).
- [ ] Build `datadog` under `vendors`: one pointer per task class with
      `target: mcp://datadog`, a docs-first skill ("call
      `search_datadog_docs` before answering any Datadog how-to"), the
      connect recipe (OAuth through the MCP client preferred, scoped
      service-account keys otherwise; request the MCP read scope and
      the MCP write scope only where a task class writes), `pup` as
      the CLI fallback, Datadog's published fair-use limits with a
      short `stale_after`, one skill page per Datadog skill worth
      installing. ≤ 30 pages.
- [ ] Root pointer `datadog` → `collection:datadog`, hint in everyday
      Datadog words (monitors, dashboards, logs, APM, security signals).
- [ ] Archive class (a) to `_archive/datadog/` by merge request;
      re-verify and move class (b) quirks with `stale_after`.
- [ ] `mk lint` clean; acceptance checks above pass, including
      `mk search "datadog monitor" --json` returning a pointer first.
- [ ] One week each side: traversal-log comparison and the probe set;
      record the keep-or-drop decision on the issue.

A non-vendor authority runs the same steps with different inputs: for
a library served through Context7, the docs tool is Context7's query
tool, there are usually no ask-then-do tools or published skills, and
the KB is mostly pointers plus the quirks we observed.

## Not in this design

- meerkat calling an authority's MCP server, proxying it, or holding
  its credentials; the agent's own MCP client does all three.
- Enabling write toolsets by default; a pointer may name one, a human
  enables it.
- Automated page moves outside the archive merge request.
