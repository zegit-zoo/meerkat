# Spec: Content source as configuration

**Status:** Implemented (`internal/contentsource`, `internal/contentsync`; wired into the Makefile and `.goreleaser.yaml`) · **Scope:** build-time content sourcing only, as originally specced — runtime resolution followed later (2026-07-29); see the README

*(`docs/design/` is a historical design record, not current reference — see [content-source.example.yaml](../../content-source.example.yaml) for the up-to-date schema.)*

## Summary

Today meerkat's build is hardwired to a single content source: a git
submodule at `kb/`, copied into the package-local embed directories by
`internal/kb/sync.sh` and baked in via `//go:embed`. This spec replaces that
hardcoded assumption with a declarative **`content-source.yaml`** so any
meerkat fork or deployment can point the build at its own content — a local
directory, a git repo, or the existing submodule — without editing
`sync.sh`, the `Makefile`, or any Go code.

The engine stays **self-contained**: content is still embedded at build time
and the binary still runs offline with zero runtime dependencies. We are
**not** adding a runtime content path (a stock binary loading content from
disk at run time). One binary still equals one knowledge base.

## Non-goals

- **Runtime content loading** (`--content-dir` / `MEERKAT_CONTENT_DIR`).
  Explicitly out of scope; the offline single-binary guarantee stays strict.
  *(2026-07-29: implemented, under different flag names — `--kb-dir`/
  `MEERKAT_KB_DIR`, `--content-source`/`MEERKAT_CONTENT_SOURCE`, and a
  `type: url` content-source.yaml source. See the README's ["Serving
  content at runtime"](../../README.md#serving-content-at-runtime) section
  for the current reference; the single-binary offline guarantee is
  unchanged when none of these are configured.)*
- **Ingestion pipeline rebuild** (generalizing `mk ingest`'s planner/executor
  and multi-host source fetching). Tracked as a follow-up (see §11).
- **Multiple/merged content roots.** One content source per build for now.

## Background — state before this spec

This section describes the pre-implementation baseline that motivated the
design below; it predates the current `internal/contentsource` /
`internal/contentsync` implementation (see Status above) and `sync.sh` no
longer exists.

`make build` → `make sync` → `go build`:

- `internal/kb/sync.sh` assumes `$REPO_ROOT/kb` is a submodule with a fixed
  layout and copies:
  - `kb/wiki/**.md` → `internal/kb/content/` (embedded as `kb.wikiFS`,
    `//go:embed all:content`)
  - `kb/ingestion/sources.yaml` → `internal/sources/etc/sources.yaml`
  - `kb/ingestion/prompts/*.md` → `internal/sources/etc/prompts/`
  - `kb/templates/*.md` → `internal/sources/etc/templates/`
    (embedded as `sources.etcFS`, `//go:embed all:etc`)
- If `kb/` is absent, `sync.sh` no-ops and the build embeds placeholders
  (the current open-source default — empty KB).
- `Makefile` derives `kbCommit` from `git -C kb rev-parse --short HEAD` and
  injects it via `-ldflags` so `mk version` reports the content pin.
- `make kb-init` / `make kb-update` manage the submodule.

The coupling is entirely in `sync.sh` + the `Makefile` `kb*`/`KBCOMMIT`
bits. The Go packages already read only from their embed FS, so **no Go API
changes are required** by this spec — only how the embed dirs get populated.

## Goals

1. The content source is declared in one version-controlled file, not baked
   into shell/Make logic.
2. Support three source types out of the box: `local`, `git`, `submodule`.
3. Preserve today's behavior exactly for existing submodule users and for the
   no-content default.
4. Private GitHub sources authenticate via the existing `internal/auth`
   `TokenProvider` (a cached `gh` OAuth token) — no bespoke secret handling.
   GitLab and other hosts have no credential borrowing: private access there
   goes through normal git credentials (a full clone URL / SSH spec, a
   credential helper, etc.).
5. Reproducible builds: the embedded content is pinned to a specific commit,
   recorded in `mk version`, and therefore covered by the cosign-signed
   release.

## Design

### `content-source.yaml` (repo root)

Optional. Absent (or `type: none`) ⇒ build with empty placeholders, exactly
as today.

```yaml
# content-source.yaml — where `make sync` pulls embeddable KB content from
# before the build embeds it. Single source per build.
content:
  type: git            # local | git | submodule | none

  # --- type: local ---
  # path: ../meerkat-kb            # content root (relative to repo root, or absolute)

  # --- type: git ---
  repo: your-org/meerkat-kb        # owner/repo slug, or a full clone URL
  host: github                     # github | gitlab — github gets the TokenProvider for private repos; gitlab uses normal git credentials
  ref: v1.2.0                      # tag, branch, or commit SHA (pin a tag/SHA for reproducibility)

  # --- type: submodule ---
  # submodule: kb                  # submodule path (the current default)

  # Layout: where each artifact lives WITHIN the resolved source.
  # Defaults shown; override only if your content repo differs.
  layout:
    wiki:      wiki/                     # markdown pages   -> internal/kb/content/
    sources:   ingestion/sources.yaml    # source registry  -> internal/sources/etc/sources.yaml
    prompts:   ingestion/prompts/        # per-source prompts -> internal/sources/etc/prompts/
    templates: templates/                # page templates   -> internal/sources/etc/templates/
```

Notes:
- `layout.wiki` is required for a non-`none` source; `sources`/`prompts`/
  `templates` are optional (a KB with no ingestion config still serves
  search/show/list).
- For `type: git`, `repo` may be a slug (`owner/repo`, combined with `host`)
  or a full URL. `ref` SHOULD be a tag or SHA; a bare branch is allowed but
  the sync tool warns that the build is not reproducible.

### Sync flow

Replace `sync.sh` with a small Go tool (`go run ./internal/contentsync`,
backed by the `internal/contentsource` library). Go (not bash) because git
resolution for private repos must reuse `internal/auth`.

```
read content-source.yaml
  ├─ absent | type:none → clear embed dirs to placeholders; print notice; exit 0
  ├─ type:local         → SRC = layout root at `path`
  ├─ type:submodule     → ensure `git submodule update --init <submodule>`; SRC = submodule path
  └─ type:git           → resolve via internal/auth TokenProvider:
                           clone/fetch `repo`@`ref` into a cache dir
                           (e.g. $XDG_CACHE_HOME/meerkat/content/<repo>@<ref>),
                           checkout the pinned ref, capture the commit SHA
copy layout subpaths → embed dirs (markdown-only, clean-first, idempotent;
                       same rules as today's sync.sh)
write .meerkat-content-stamp (resolved type, repo, ref, commit SHA)
```

The copy step keeps `sync.sh`'s existing guarantees: `*.md`-only for
wiki/prompts/templates, delete-then-copy so source deletions propagate, and a
non-rsync fallback.

### Build integration

- `Makefile`: `sync` target calls the Go tool. `KBCOMMIT` is read from
  `.meerkat-content-stamp` (falling back to `unknown`) instead of
  `git -C kb`. `kb-init`/`kb-update` become thin wrappers over the tool
  (or are dropped once `type: git` covers the workflow).
- `.goreleaser.yaml` `before.hooks` calls the same tool, so released binaries
  embed the configured, pinned content.
- `mk version` `kb_commit` = the stamp's commit SHA (git/submodule) or
  `local:<tree-hash>` for `type: local`. Unchanged for the no-content case
  (`unknown`).

### Empty / default behavior

No `content-source.yaml`, or `type: none`: the tool clears the embed dirs to
the committed placeholders and exits 0. `go build`/`go test` stay green with
an empty KB (the engine already tolerates this — `kb.List()`/`sources.All()`
return empty, content-dependent tests skip). This keeps the public repo
building cleanly without shipping anyone's content.

### Validation & errors

The tool validates the config and fails fast with actionable messages:
- `type: git` without `repo`/`ref`; `type: local` without `path`;
  `type: submodule` without an initialized submodule.
- Missing `layout.wiki` for a non-`none` source.
- Git auth failure on a GitHub source → point at `gh auth login` (mirrors
  the `mk update` error style). Other hosts fail with a plain git error;
  point at the host's own git credential setup.
- A moving `ref` (branch) → warning, not error.

A `mk content sync --check` (or `--dry-run`) resolves and validates without
copying — usable as a CI lint on `content-source.yaml`.

## Security considerations

- **Private GitHub content** authenticates through `internal/auth`'s
  `TokenProvider` (a cached `gh` OAuth token). meerkat mints no tokens and
  stores no secrets; same posture as `mk update`. GitLab and other hosts
  have no credential borrowing built in — private access there relies on
  the user's own git credential configuration.
- **Reproducibility / provenance:** pinning `ref` to a tag or SHA makes the
  embedded content deterministic. The resolved commit is recorded in
  `mk version` and, because content is embedded, it is covered by the
  cosign-keyless signature on the release — a consumer can tie a binary to the
  exact content commit it was built from.
- **Cache trust:** the git cache dir is user-owned; the tool checks out the
  exact pinned ref and records the resolved SHA (so a mutated cache is
  detectable via the stamp).

## Migration

1. Existing submodule users add:
   ```yaml
   content: { type: submodule, submodule: kb }
   ```
   → byte-for-byte identical build; `sync.sh` retired (kept briefly as a shim
   that execs the Go tool, then removed).
2. The public repo ships **no** `content-source.yaml` (default empty build).
3. A deployment switches to its own KB by committing a `type: git` (or
   `local`) config — no code or Makefile edits.

## Testing strategy

- Unit: config parse/validate (all types + bad inputs); layout resolution;
  the copy logic against a temp fixture content tree (local + submodule
  paths). Git path tested against a local bare-repo fixture (no network).
- Build smoke: `type: local` pointing at a tiny fixture KB → `go build` →
  assert `kb.List()` returns the fixture pages and `mk version` reports the
  stamp commit.
- Default: no config → build green, empty KB, content tests skip.

## Open questions / follow-ups

- **Ingestion pipeline rebuild** (deferred): generalize `mk ingest` planner/
  executor and multi-host source fetching to match this config. Largest
  remaining generalization piece; spec separately.
- **Runtime content** (explicitly rejected here): revisit only if a
  "one binary, many KBs" deployment need appears. *(2026-07-29: that need
  appeared; implemented — see the Non-goals note above and the README.)*
- **Multiple content roots / overlays:** compose several sources into one KB
  (e.g. a base KB + a team overlay). Out of scope; note if demand appears.
- **Config file name:** `content-source.yaml` (single-purpose) vs a broader
  `meerkat.yaml` that could later hold other build/runtime config. Recommend
  starting single-purpose; promote to `meerkat.yaml` with a `content:` block
  if more build config accrues.
