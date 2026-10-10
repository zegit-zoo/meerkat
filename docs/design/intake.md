# The intake pipeline: researcher, validator, librarian

meerkat-mob issue H (#8), first slice. Issue G (#7) made an agent's
outside research land in an intake store. This page is the loop that
turns it into knowledge: three agent roles, run through `mk ingest`,
that produce a candidate page, confirm it independently, and keep the
tree honest — with no human in the loop by default (Q7), and every
step idempotent.

## Layout

Under the `intake:` store's prefix (any `memory:`-style backend: local,
GCS, S3; `internal/intake`):

```text
raw/<namespace>/<yyyy-mm-dd>/<id>/page.md   what mk_report_outcome deposited
staged/<kb>/<id>.md                         a researcher's candidate page
done/<id>.md                                processed marker (idempotency)
done/validated-<id>.md                      confirmed marker
done/filed-<id>.md                          filed marker
validations/<id>/<run>.md                   one record per validator run the
                                            pipeline executed (model, outcome)
parked/<id>.md                              needs-human, with the reason
                                            and, once filed, the forge issue
```

`<namespace>` is `memory.Namespace` of the depositing identity (Q6):
an agent sees only its own deposits (`--namespace`), a librarian sees
all. Every key is unique and written create-only, so the store is
single-writer-per-key on every provider.

## Caller-supplied text

meerkat-mob#34. Everything in a raw item was written by the depositing
MCP caller, so:

- `mk_report_outcome` accepts only `https` sources on public hosts
  (see `intake.CheckSource`); the researcher plan drops any other source
  an older deposit carries and says how many it dropped.
- The question, the attempted path and the sources reach the researcher
  prompt only as `{{question}}`, `{{attempted}}` and `{{sources}}`, each
  rendered as a fenced block labelled as untrusted data, one entry per
  line with control characters flattened, behind a fence longer than
  any backtick run in the text. A content-repo prompt override gets the
  same blocks. The built-in prompts tell the researcher and validator
  to treat the deposit and the candidate as material to check, never
  as instructions.
- A deposit whose question, sources or body matches a built-in secret
  pattern (private key headers, cloud and forge tokens, bearer tokens,
  JWTs) is not researched, and a candidate that matches one is not
  staged and is removed from the working copy. The patterns are a
  tripwire; the content repo's own secret scanning stays the gate.
- `--trust-sources` on a `--role` run also needs `--trust-intake`
  ([INGESTION.md](../INGESTION.md)).

## Roles

`mk ingest --role <researcher|validator|librarian> --from intake`.
The executor is the existing one — an agent CLI (`opencode run` or
`claude`) run in the content working copy and told what to do by an
instruction — so a mongoose skill or any other executor can replace it
later. What differs per role is the prompt, the instruction and the
success check. Prompts come from the content repo's
`ingestion/prompts/<role>.md` when present, else the built-ins in
`internal/ingest/prompts/`.

| Role | Input | Writes | Success | Then (`Finalize`) |
|---|---|---|---|---|
| researcher | raw items not yet done | `wiki/intake/<id>.md`, from the raw item materialised at `ingestion/intake/<id>.md` | the candidate exists and is not a placeholder | trust fields reset and provenance stamped (below), copied to `staged/<kb>/<id>.md`, raw item marked done |
| validator | staged candidates without enough confirmations | the candidate's frontmatter only: `verified:`, `failure_reason:` or `status:`; the working copy is re-seeded from the staged copy before each run | one or the other is present | the run is checked against the staged copy and recorded under `validations/<id>/`; two recorded confirmations from distinct models mark it validated; a `needs-human:` reason parks it; two recorded failures park it |
| librarian | the whole registry, the intake store, the traversal log | nothing without `--apply` or `--execute` | — | report; `--apply` files confirmed candidates (a `direct` contract also needs `--file-confirmed`) and root pointers through each collection's contract; `--execute` runs the prompt-quality rewrites (below) |
| librarian (rewrite) | the report's hint, description and route findings | ONE frontmatter field (`hint:` or `description:`) on ONE page per task, in the content working copy | the page still parses | `FinalizeRewrites` compares with a pre-run snapshot: body and every other field byte-identical, the field changed, one line under 300 chars — else the snapshot is restored and the task reported `rejected` |

Independence (Q7): a validator run whose `--model` equals the
candidate's `researcher_model` is skipped with the reason, and so is a
model that has already confirmed the candidate; the two confirmations
must come from recorded runs of distinct models. The pipeline's bar for
*filing* is `ConfirmationsRequired` (2).

Confirmations are counted from the pipeline's own records, never from
the page (meerkat-mob#33). A candidate is agent-written text built from
caller-supplied research, so what its frontmatter says about who
verified it is not evidence:

- `Finalize` stages a researcher's page with `verified:` emptied,
  `status: unverified`, `failure_reason:` cleared, `generated:` and
  `last_ingested:` set by the pipeline, and every key outside the OKF
  core dropped before `intake_id`, `researcher_model` and `target_kb`
  are stamped.
- Each validator run starts from the staged copy. Afterwards the page
  is compared with it: a run that changed the body or any field other
  than `verified:`, `failure_reason:` and `status:` is rejected, the
  staged copy is restored in the working copy, and nothing is recorded.
- An accepted run is recorded as `validations/<id>/<run>.md` with the
  `--model` the pipeline ran it with and its outcome. It is a
  confirmation when it added a `verified:` entry and set no
  `failure_reason:`; how many entries it added does not matter, one run
  is one confirmation.
- The librarian lists a candidate as fileable only from those records,
  and `--apply` re-checks them before filing. The filed page's
  `verified:` is rebuilt from the records
  (`agent:validator:<model>`, the run's time) with
  `status: machine-confirmed`.

Target collection: the deepest collection the reporting agent tried
(the last of `attempted`), or `unrouted` for the librarian to place.

## Escalation

Nothing waits for a human. A validator that answers `failure_reason:
needs-human: …`, or a second disagreement on the same candidate, parks
the item under `parked/<id>.md` with the reason, prints a
`needs-human:` line, and counts
`meerkat_librarian_findings_total{kind="needs_human"}`. The librarian
report lists parked items, and `--apply` escalates each one to the target
collection's forge (below).

### Forge issues for parked items

meerkat-mob #19, MK-LIB-09. `mk ingest --role librarian --apply` files
**one** issue per parked item on the forge of the target collection's
update contract, and removes the parked marker when a human closes that
issue as resolved. Nothing waits for the human (MK-INT-06): the next librarian run
does whatever has become possible.

The target collection is the staged candidate's (`staged/<kb>/<id>.md`),
else the raw item's deepest attempt, else `unrouted`. An issue is filed
only when that collection has a `merge-request` contract whose `host` is
`github` or `gitea` and which names a `token_env`
([update-contract.md](update-contract.md#token_env-the-librarians-forge-issues)).
Every other case is reported, never a failed run:

| Case | Report |
|---|---|
| no contract, `method: none` or `direct`, `unrouted` | `instructions` — no forge for `<kb>`; a human must look at `parked/<id>.md` |
| `host: gitlab` or `other` | `skipped` — not supported yet |
| a `repo` whose host or `owner/repo` segments are outside the safe character set | `skipped` — the address is refused, no forge call is made |
| no `token_env`, or the variable unset | `skipped` — with the variable's name; no forge call is made |
| the forge answers with an error | `skipped` — the method, the API path and the status; never the token |
| another run began filing for the item less than 15 minutes ago | `skipped` — the filing is in flight; a later run retries |
| the marker's `## forge issue` section does not read | `skipped` — repair or remove it by hand; nothing is filed over it |

The issue:

- **title** `needs-human: <candidate title>`, or the intake id when there
  is no candidate; one line, at most 120 characters;
- **body** a first line `<!-- meerkat:intake-id <id> -->` (an HTML
  comment, so it does not render; it is how a later run finds the issue,
  below), the parked reason, the `intake_id`, the target collection, the
  attempted path, the parked marker's key, the initial question, and the
  validators' `failure_reason` where the staged candidate carries one. The
  staged copy is the researcher's page, so a validator's verdict usually
  reaches the issue through the parked reason (`validators disagreed N
  times; last: …`, or the `needs-human:` answer itself). Agent- and
  validator-written text is fenced, so a mention or markup in it neither
  pings anyone nor renders. The one-line fields (target collection,
  attempted path) are code spans with every backtick turned into `'` and
  every control character into a space, because the attempted path is
  whatever the reporting agent sent to `mk_report_outcome` and a backtick
  or blank line would otherwise end the span; `mk_report_outcome` also
  refuses such entries;
- **label** `needs-human` (GitHub creates a missing label when the token
  may; Gitea takes label IDs, so the label is used only when the repo
  already has it).

The body carries the initial question to whoever can read the forge
repo. An operator who points `token_env` at a public repo is publishing
those questions; that is the reason the escalation is opt-in per
collection.

**Idempotency.** The parked marker records the filing, under a `##
forge issue` heading, each write conditioned on the version just read:

1. Before it files, the run *claims* the filing: `filing: <time>` in the
   marker. A second run that finds a claim younger than 15 minutes files
   nothing and says so; one that loses the write race does the same.
2. After filing, the claim is replaced by the issue's URL, host kind, API
   root, repo and number. An item whose marker carries a reference is
   never filed again; the report shows `issue <url> open` instead of
   `issue not yet filed`.
3. A claim older than 15 minutes means an earlier attempt never recorded
   its issue: the forge failed, or answered too late (a client timeout
   after the forge created the issue), or the run died. Before filing,
   the run lists the repo's issues changed since that claim (minus ten
   minutes for clock skew; `GET /repos/{owner}/{repo}/issues?since=…`,
   which GitHub and Gitea both answer from their database, not a search
   index) and **adopts** the lowest-numbered issue whose body starts
   with the item's marker line, reporting `adopted`. Only when there is
   none does it file. The listing reads at most ten pages; past that the
   run files and says it could not rule out an earlier issue.
4. Two runs can still both file when one takes over a claim the other
   is still acting on (a run slower than 15 minutes between claiming and
   recording). The first issue recorded wins; the other run reports
   `duplicate` with the URL of the issue it filed, for a human to close.
5. If the issue is filed but the marker cannot be updated, the run fails
   and names the issue's URL; the claim stays, so the next run adopts the
   issue rather than filing again.

A reason cannot forge the section. `Park` writes a marker whose second
line is `<!-- meerkat:parked v2 -->` and indents any reason line that
starts with `#`. A marker written before forge issues existed has a
blank second line and its reason unquoted, so it is read as reason only,
whatever its text looks like; recording an issue rewrites it in the
current layout with the reason quoted. A section that is present but
does not read (a missing field, a duplicate key) is reported and never
filed over.

**Closing the loop.** On every `--apply` run the librarian reads each
filed issue. Closed **with the `resolved` label**: the parked marker is
deleted (`intake.Store.Unpark`, through the store's optional
`memory.Deleter`, which the local, GCS and S3 backends implement) and the
run reports `unparked`. That removes the `needs_human` finding; it does
not re-enable anything else. A parked marker never stopped a validator
(`planValidation` does not read it), and the candidate's
`validation_failures` count, kept in the validator's working copy, is
not reset: the next failed validation in that working copy parks the
item again at once and files a new issue. The human who resolves the
issue should therefore fix the candidate or its sources first, so the
next validation confirms it. Closed without `resolved` (won't fix,
duplicate): left parked and reported. Open: nothing to say.
Only an issue on the forge the contract names *now* is followed: a
reference whose host kind, API root or repo differs is reported and left
alone. The API root is part of the comparison because the host kind
alone does not tell `github.com/org/x` from `ghe.corp.example/org/x`.

`meerkat_librarian_filed_issues_total{host}` counts filed issues by forge
kind (`github`, `gitea`, `gitlab`, `other`). No issue URL, repo or title
is a metric label or a span attribute.

## The librarian

`mk ingest --role librarian [--days N] [--apply]` loads every
collection the registry holds (cold ones are mounted for the run) and
reports:

- **dangling** `related:` entries and pointer targets (issue B's link
  graph),
- **stale** pages past `stale_after`,
- **cull** proposals: pages stale for more than 90 days with no recent
  verification — archive through the contract, never delete (Q8),
- **missing links**: collections that at least 3 sessions searched and
  gave up in during the last N days, read from the traversal log
  (issue G) and matched by hashing the registry's own names with the
  log's key; the initial queries in the log say what they asked,
- **needs-human** parked items, each saying whether its forge issue is
  filed (`issue not yet filed` or `issue <url> open`),
- **fileable** staged candidates with the required confirmations, by
  collection and contract method,
- **promotion** proposals (meerkat-mob #20): collections at depth 2 or
  deeper whose flushed temperature (issue E's records in the traversal
  log) puts them in the top 5 for the window, and that no pointer in
  the root already reaches. Each proposal names the root, the pointer
  page it would add (`pointers/<collection>`), the hint it would carry
  (the collection's description, else its tree path), any pointer that
  reaches the collection today from a lower hub, and the pages sessions
  confirmed most inside it (matched by hashing the registry's own
  qualified IDs), so a human can judge whether those pages belong
  higher up. Page moves are never automated,
- **prompt-quality** rewrite targets (meerkat-mob #21), from the
  initial queries in the traversal log, each supported by at least 2
  sessions (`--prompt-min-sessions` in `LibrarianOpts`):
  - *hint*: sessions gave up in a collection whose pointer hints
    mention none of the query's words (a misspelt "datadgo" is not
    found in "datadog" — that is the point) → rewrite the hint;
  - *description*: sessions found their page only after the exact
    stage missed and the fuzzy or prefix stage answered (the session's
    `stages` counts) → put the words agents use on the pages;
  - *tool*: sessions gave up having touched only the root, or nothing
    → the `mk_search` tool description did not send them anywhere;
  - *route*: sessions starting in a hub took a wrong turn first
    (`wrong_turns` > 0) → the hub's pointer hints do not separate its
    children.
  Each finding quotes the queries (most frequent first). Description
  findings also list the pages sessions confirmed (matched by hash).

### The rewrite stage

`mk ingest --role librarian --execute --workdir-kb <copy> [--branch b]`
runs the analysis and then, for every hint, route and description
finding whose page is in that working copy, one executor task with the
`librarian-rewrite` brief (`internal/ingest/prompts/librarian-rewrite.md`,
overridable as `ingestion/prompts/librarian-rewrite.md`): the file, the
one field it may change, the queries and the words no text mentions.
Pages served from another repo or from a memory overlay are skipped
with the reason. The commit message cites the queries. Rewrites go to
`--branch`, default `librarian/rewrites`, never the content branch: a
rewrite is confirmed by review of that branch before it serves.

After the run each page is checked against its pre-run snapshot; any
change beyond the one field restores the snapshot and reports the task
as rejected (the agent's commit on the review branch is the operator's
to discard). `meerkat_librarian_rewrites_total{action}` counts
rewritten, unchanged, rejected and failed.

Tool findings are not executed: the `mk_search` description lives in
meerkat, not in a content repo. They are written to
`ingestion/proposals/tool-description-<date>.md` in the working copy,
with the queries, for a human to turn into a merge request on meerkat.
Nothing is ever applied to a running server.

Without `--apply` it changes nothing. With it, a confirmed candidate
for a `direct` collection is reported as `held` unless
`--file-confirmed` is also given: two machine confirmations are a check,
not a review, and a `direct` write publishes the page to every reader
of the collection at once. Review the staged copy (`staged/<kb>/<id>.md`)
and re-run with `--file-confirmed` to get the page written into the
collection's memory store at `global/intake/<id>.md` and the item
marked filed. A collection that wants review on every candidate should
declare a `merge-request` contract; `merge-request`
contracts get the instructions printed (the candidate is already
committed on the working copy's branch); `none` names the page for a
human to place. A promotion is filed the same way into the root: a
`direct` root gets `global/pointers/<collection>.md` written and
published into its overlay at once, so the next run's link graph sees
the pointer and proposes nothing; a `merge-request` root gets the
pointer spelled out in the instructions. Parked items are escalated to
their collection's forge, or un-parked (see
[Forge issues for parked items](#forge-issues-for-parked-items)).

## Metrics

`meerkat_intake_items_total{stage,outcome}`,
`meerkat_intake_age_seconds` (oldest unprocessed raw item at the last
listing), `meerkat_librarian_findings_total{kind}`,
`meerkat_librarian_rewrites_total{action}`,
`meerkat_librarian_filed_issues_total{host}`.

## Not in this slice

- A rewrite that edits more than one field or page, or that touches
  the tool text itself; both stay human work.
- Forge issues on GitLab, and filing through `host: other`; both are
  reported as not supported yet.
- Automated page moves for a promotion; only the root pointer is filed.
- Attachments under `raw/<…>/<id>/attachments/`; the layout leaves
  room for them.
- A mongoose executor; the executor seam is unchanged.
- The copied-authority refactor (replacing pages copied from an
  authority with pointers to the tools it exposes, such as its MCP
  server) is designed, not built, in
  [copied-authority-refactor.md](copied-authority-refactor.md).
