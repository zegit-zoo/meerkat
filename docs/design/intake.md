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

```
raw/<namespace>/<yyyy-mm-dd>/<id>/page.md   what mk_report_outcome deposited
staged/<kb>/<id>.md                         a researcher's candidate page
done/<id>.md                                processed marker (idempotency)
done/validated-<id>.md                      confirmed marker
done/filed-<id>.md                          filed marker
parked/<id>.md                              needs-human, with the reason
```

`<namespace>` is `memory.Namespace` of the depositing identity (Q6):
an agent sees only its own deposits (`--namespace`), a librarian sees
all. Every key is unique and written create-only, so the store is
single-writer-per-key on every provider.

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
| researcher | raw items not yet done | `wiki/intake/<id>.md`, from the raw item materialised at `ingestion/intake/<id>.md` | the candidate exists and is not a placeholder | provenance stamped (`generated`, `last_ingested`, `intake_id`, `researcher_model`, `target_kb`), copied to `staged/<kb>/<id>.md`, raw item marked done |
| validator | staged candidates without enough confirmations | the candidate's frontmatter only: a `verified:` entry `agent:validator:<model>` or a `failure_reason` | one or the other is present | two independent agent confirmations mark it validated; a `needs-human:` reason parks it; two failures park it |
| librarian | the whole registry, the intake store, the traversal log | nothing without `--apply` or `--execute` | — | report; `--apply` files confirmed candidates and root pointers through each collection's contract; `--execute` runs the prompt-quality rewrites (below) |
| librarian (rewrite) | the report's hint, description and route findings | ONE frontmatter field (`hint:` or `description:`) on ONE page per task, in the content working copy | the page still parses | `FinalizeRewrites` compares with a pre-run snapshot: body and every other field byte-identical, the field changed, one line under 300 chars — else the snapshot is restored and the task reported `rejected` |

Independence (Q7): a validator run whose `--model` equals the
candidate's `researcher_model` is skipped with the reason; the two
confirmations must come from distinct `agent:validator:<model>`
entries. `kb.Frontmatter.TrustTier()` already reads "verified by an
agent" as machine-confirmed; the pipeline's bar for *filing* is
`ConfirmationsRequired` (2).

Target collection: the deepest collection the reporting agent tried
(the last of `attempted`), or `unrouted` for the librarian to place.

## Escalation

Nothing waits for a human. A validator that answers `failure_reason:
needs-human: …`, or a second disagreement on the same candidate, parks
the item under `parked/<id>.md` with the reason, prints a
`needs-human:` line, and counts
`meerkat_librarian_findings_total{kind="needs_human"}`. The librarian
report lists parked items. Filing a forge issue through the update
contract's host is the next slice.

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
- **needs-human** parked items,
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

Without `--apply` it changes nothing. With it, `direct` contracts get
the page written into the collection's memory store at
`global/intake/<id>.md` and the item marked filed; `merge-request`
contracts get the instructions printed (the candidate is already
committed on the working copy's branch); `none` names the page for a
human to place. A promotion is filed the same way into the root: a
`direct` root gets `global/pointers/<collection>.md` written and
published into its overlay at once, so the next run's link graph sees
the pointer and proposes nothing; a `merge-request` root gets the
pointer spelled out in the instructions.

## Metrics

`meerkat_intake_items_total{stage,outcome}`,
`meerkat_intake_age_seconds` (oldest unprocessed raw item at the last
listing), `meerkat_librarian_findings_total{kind}`,
`meerkat_librarian_rewrites_total{action}`.

## Not in this slice

- A rewrite that edits more than one field or page, or that touches
  the tool text itself; both stay human work.
- Forge issues for parked items (meerkat-mob #19).
- Automated page moves for a promotion; only the root pointer is filed.
- Attachments under `raw/<…>/<id>/attachments/`; the layout leaves
  room for them.
- A mongoose executor; the executor seam is unchanged.
