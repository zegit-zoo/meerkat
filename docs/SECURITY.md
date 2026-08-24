# Security

Meerkat ships under a layered security suite that runs on every push
and gates every release. This doc explains what runs, what each tool
catches, and how to fix the things they flag.

---

## What runs

| Tool | What it catches | Where | Severity gate |
|------|-----------------|-------|----------------|
| `govulncheck` | Known CVEs in our **actual import graph** (Go vuln DB) | CI (Vulnerability scan job) + `make vuln` | Hard fail on any reachable vuln |
| `gosec` | Go-specific weaknesses (weak crypto, command injection, file traversal, hardcoded creds) | CI (inside golangci-lint) + `make gosec` | Hard fail on HIGH severity, medium confidence |
| `gitleaks` | Accidentally committed secrets (PATs, OAuth tokens, private keys) | CI (gitleaks job) + `make gitleaks` | Hard fail on any leak |
| `goreleaser sboms` | SPDX SBOM generated per release artifact via syft | CI release workflow | Attached to the GitHub release |
| `goreleaser signs` | Cosign keyless (Fulcio + Rekor) signature on the checksums file | CI release workflow | Cosign-verifiable transparency-logged signature |
| `docker buildx --sbom` | SPDX SBOM attestation for the container image (BuildKit's syft-based scanner) | CI release workflow (`docker` job) | Attached to the image manifest as an OCI referrer |
| `docker buildx --provenance` | SLSA provenance attestation for the container image | CI release workflow (`docker` job) | Attached to the image manifest as an OCI referrer |
| cosign (image) | Cosign keyless (Fulcio + Rekor) signature on the pushed image manifest | CI release workflow (`docker` job) | Cosign-verifiable transparency-logged signature |

CI (GitHub Actions) runs lint, test, govulncheck, and gitleaks as
**parallel** jobs on every push and PR; `release.yml` re-runs the full
gate before GoReleaser, so a vulnerable `main` cannot be tagged.

We don't run `semgrep-sast` (~19 min on a fresh runner); `gosec` (run
inside golangci-lint) already covers the Go-specific weaknesses it
would flag.

---

## Run locally

```bash
make security        # all three at once
make vuln            # govulncheck
make gosec           # gosec
make gitleaks        # gitleaks
```

Each target self-installs the tool from a pinned version (see the
`*_VERSION` block in `Makefile`) so devs don't need a separate
setup step. Pinned versions keep results reproducible across the
team.

---

## Fixing findings

### `govulncheck` — vulnerable dependency

```
Vulnerability #1: GO-2024-XXXX
  Module: golang.org/x/foo
    Found in: golang.org/x/foo@v0.5.0
    Fixed in: golang.org/x/foo@v0.6.1
    More info: https://pkg.go.dev/vuln/GO-2024-XXXX
```

Bump the dependency:

```bash
go get golang.org/x/foo@v0.6.1
go mod tidy
make vuln  # re-run to confirm
```

If the vuln is in a transitive dep we don't directly use, govulncheck
won't flag it (that's the point — it walks the actual call graph,
not just the lockfile). If you see one anyway, the call path it
shows is real.

### `gosec` — weakness in our code

| Rule | What | Typical fix |
|------|------|-------------|
| G101 | Hardcoded credentials | Move to env var; use `MEERKAT_API_KEY` pattern |
| G104 | Unhandled error | Wrap with `if err := ...; err != nil { return err }` or document the ignore with `// nolint:errcheck` and a reason |
| G107 | URL provided to HTTP request as taint source | Validate or allowlist the URL before the call |
| G201/G202 | SQL string formatting | We don't have SQL today; if added, use `database/sql` parameterised queries |
| G401-G403 | Weak crypto | Use `crypto/rand`, AES-256-GCM, ed25519, ChaCha20-Poly1305 |
| G601 | Implicit memory aliasing in for loop | Capture loop var into local before goroutine/closure |

If a finding is a known false positive, suppress with `-exclude=GNNN`
in the Makefile and document why in a comment alongside.

### `gitleaks` — committed secret

If the leak is in your working tree, **delete it and re-commit**.
If it's in history, you must **rotate the secret** first (assume
it's exposed) and then optionally rewrite history with
`git filter-repo` (coordinate the force-push).

Patterns we treat as not-secrets are listed in `.gitleaks.toml`'s
allowlist; add narrow entries there before loosening the rules.

### `goreleaser sboms` / `signs` failures

These run at release time only. Failures mean a release won't be
cut. Most common causes:

- `cosign` not in PATH on the runner — fix the base image.
- Missing `id-token: write` permission — Fulcio keyless signing needs
  OIDC. The CI snippet sets it; if you copy-paste a release job,
   preserve the permissions block.

---

## Audited and accepted findings

These items are regularly flagged by generic scanners but are
intentional in meerkat's design.

- `internal/update/install.go` (`#nosec G702`):
  `syscall.Exec(currentExe, ...)` is an intentional self-reexec after
  atomic binary swap. `currentExe` comes from `os.Executable()` and is
  resolved via `filepath.EvalSymlinks`.
- `internal/update/notify.go` (`#nosec G118`):
  background goroutine intentionally outlives the command to persist
  update-check cache.
- `internal/update/download.go` / `internal/update/cosign.go` (`#nosec G304`):
  local file paths are tempfiles created by meerkat itself and consumed
  in the same execution flow.
- External `cosign` invocation in `internal/update/cosign.go`:
  meerkat uses `exec.CommandContext` with explicit argv (no shell),
  verifies Fulcio certificate identity and OIDC issuer, and requires
  Rekor transparency-log verification.

Additional hardening in place:

- Update HTTP client refuses redirects outside `github.com`,
  `*.github.com`, and `*.githubusercontent.com` for token-bearing
  release/download requests.
- Ingest executor validates that task `page_path` resolves within the
  configured KB workdir before reading/writing page files.
- `type: url` / `type: gcs` content archive extraction (`internal/contentsource/archive.go`)
  treats every entry as hostile: symlink and hardlink entries are skipped
  outright (never created, never followed — the same escape vector
  `internal/kbdir`'s read-side adapter is hardened against, reproduced here
  on the write side); an entry name that's absolute, traverses (`..`), or
  contains a backslash/colon (Windows drive-absolute confusion) is
  rejected outright rather than relying on containment alone; every write
  goes through an `os.Root` rooted at the extraction directory, so
  containment holds even against a name engineered to defeat a
  string-only check; and per-file, cumulative, and entry-count caps bound
  decompression against a zip-bomb-style archive. A `type: gcs` bundle is
  extracted by that same code; a `type: gcs` prefix mount applies the same
  entry-name validation and `os.Root` containment to remote object names,
  with per-file, cumulative and object-count caps of its own
  (`internal/contentsource/gcs.go`).

---

## Threat model (sketch)

Meerkat is **not** read-only: `mk ingest --execute` writes pages to a
working copy, commits, and pushes them upstream. The threat surface
is small but worth being explicit about:

| Asset | Threat | Mitigation |
|-------|--------|-----------|
| Embedded wiki content | Tampering between source and binary | Content is embedded at build time; `mk version`'s `kb_commit` records the content commit it was built from. The release binary's SHA256 is published and cosign-signed, so consumers can verify the exact bytes — **this mitigation covers embedded content only** (see the next row). |
| Runtime KB content — unverified (`--kb-dir` flag / `MEERKAT_KB_DIR` env var, or a `type: local` `content-source.yaml` source) | Tampering, or malicious content, in a directory an operator points meerkat at | **Not covered by the cosign signature, the checksums file, or `kb_commit`.** `kb_commit` always names the build-time embedded content's commit regardless of what's actually being served — it says nothing about this directory's contents. `mk version`'s `kb_source` field (`disk:<path>`) reports that this kind of content is in effect, so operators and auditors can tell it apart from `embedded`/`url:...` (below), but it is a provenance label, not an integrity guarantee: meerkat performs no signature check, hashing, or sandboxing here. An operator who configures one of these is trusting that directory themselves, at the moment of every invocation — comparable in posture to `--trust-sources` for ingestion (below). |
| Runtime KB content — digest-verified (`type: url` `content-source.yaml` source) | Tampering in transit, or at rest wherever the archive is hosted | **Also not covered by the cosign signature, the checksums file, or `kb_commit`** — same as the row above. What *is* different: the archive's sha256 is checked before anything is extracted or cached (`internal/contentsource.FetchURL`), so tampered bytes are rejected outright rather than served, and extraction itself is hardened against a hostile archive (symlink/hardlink entries skipped, absolute/traversing entry names rejected, writes contained by an `os.Root`, per-file/cumulative/entry-count decompression caps — see "Additional hardening in place" above). What the digest does **not** buy: it does not place the archive under the release's cosign signature — that covers the binary's own checksums file, not an arbitrary operator-named URL — and meerkat cannot tell a correct digest for the *wrong* archive from a correct digest for the intended one. The choice of `url`/`sha256` in `content-source.yaml` is still the operator's, unverified by meerkat. `kb_source` reports `url:<url>@<digest12>` so this case is distinguishable from `disk:<path>` at a glance. |
| Runtime KB content — generation-pinned (`type: gcs` `content-source.yaml` source) | Tampering, or an unexpected overwrite, of an object in a Google Cloud Storage bucket an operator points meerkat at | **Also not covered by the cosign signature, the checksums file, or `kb_commit`** — same as the two rows above. What *is* different: GCS assigns a new generation on every write, and meerkat fetches with a conditional read (an explicit generation **and** `ifGenerationMatch`), so the bytes written into a cache entry named `<generation>` cannot be another generation's; the cache key is that generation (bundle mode) or a fingerprint over every listed object's `(name, generation)` (prefix mode), so any overwrite/add/delete invalidates it rather than being served from a stale entry. An explicit `generation:` in the config pins the deployment outright — the current generation is never consulted, so a later overwrite cannot change what this binary serves. `sha256:` is optional here (the generation already pins the bytes) and is verified before extraction when set. Bundle extraction reuses the same hardened `type: url` extractor; prefix mode applies the same entry-name validation and `os.Root` containment to object names, plus per-file/cumulative/object-count caps. **Credentials:** Application Default Credentials / Workload Identity Federation only — the schema has no field for a static service-account key, so meerkat cannot be configured to read one. Access control on the bucket is Google Cloud IAM's; meerkat adds none of its own, and any principal that can read the bucket can serve its content. `kb_source` reports `gcs://<bucket>/<object>@<generation>`. |
| Runtime KB content — **hot-reloaded** (`type: gcs` with a `refresh:` block) | An operator who believes a deployment is pinned when it is in fact following the bucket; a hostile or broken new generation being served, or taking the process down with it | Opting in is explicit and **mutually exclusive with pinning**: `generation:` and `refresh:` together are refused at config load, because a file that carries both has two contradictory readings and one of them silently revokes the reproducibility guarantee. `Source.Refreshable()` re-asserts the same rule at runtime, so the failure direction stays closed — the worst outcome of a validation gap is a source that does not move. Everything a refresh actually does is the startup path: the same conditional reads (explicit generation **and** `ifGenerationMatch`), the same `os.Root`-contained writes and entry-name validation, the same per-file/cumulative/object-count caps, the same ADC-only client with no key-file field. What is *added* is that the expensive work happens off the request path into a **staging cache entry keyed by the new version**, and is published as one atomic snapshot swap — so a partially downloaded, partially parsed or partially indexed generation is never visible, a failed refresh cannot delete or corrupt the last known-good cache entry, and the collection keeps serving the previous generation and is marked **degraded** rather than emptied or taken down. Polling is metadata-only (`refresh.interval` has a 5s floor) and there is no inbound path: no webhook, no Pub/Sub, no HTTP reload endpoint. The manual trigger is `SIGHUP`, authorized by the operating system — you can send it only if you could already signal the process. `kb_source` follows the bytes, so it reports the generation currently being served rather than the one the process started on. See [design/hot-reload.md](design/hot-reload.md). |
| Shared GCS **memory** store reconciliation (`memory.refresh`) | A personal memory becoming readable by the wrong principal when it is re-read on another replica; a staged (unreviewed) proposal being published by a reload | The rebuilt overlay derives every page ID from the **store key** (`memory.Page(key, body)`), never from a `memory_namespace:` field the document's own bytes could claim — so a personal memory re-read on a second replica is private to exactly the principal who wrote it, and is answered as `not found` to everyone else, identically to a page that never existed. The rebuilt index contains **every** document, private ones included, because visibility is a mandatory clause in the query rather than an index-time filter (an index-time filter would hide a memory from its own owner). The reload's cheap probe and its `Load` share one "is this a live document" decision, so the staging prefix is excluded from both: writing a proposal neither publishes it nor triggers a fleet-wide reload. `TestViewedBy_SurvivesARemount` exercises the property over both a fresh mount and a live, serving collection. |
| User's GitHub token (used by `mk update` and `mk ingest` git auth) | Disclosure via argv, on-disk config, or logging | The token (from the `gh` auth cache) is handed to the clone/fetch subprocess only via a `credential.helper` script that reads it back from an env var (`MEERKAT_GIT_TOKEN`) at request time — it never appears in argv, in the persisted remote URL, or in `.git/config`. The remote URL is scrubbed back to its tokenless form in a `defer` immediately after clone/fetch, including on error paths, so a live credential doesn't linger in the cache dir. |
| Downloaded release binary (in `mk update` flow) | Supply-chain swap | SHA256 verified against published `checksums.txt`; cosign signature on the checksums file (Rekor-logged); staged in a user-owned temp dir before final copy/move; `.old` backup during swap |
| `mk mcp serve-http` — who may reach which collection | An authenticated caller reading a collection they aren't entitled to, or *learning it exists* | Bearer tokens are verified as real OIDC tokens (signature against the issuer's JWKS, `iss`, `aud`, `exp`) by `github.com/coreos/go-oidc`; discovery runs at startup so a bad issuer fails the process rather than producing intermittent 401s. **Audience is mandatory** — a provider with no `audience` and no `auth.resource` is refused at construction, because an audience-unbound resource server accepts tokens minted for any other relying party of the same IdP. Authorization is applied by *narrowing the registry* once per request (`collections.Registry.Restrict`), not by a per-operation check: a collection the caller can't read is absent from `Names`/`All`/`Get`/`target`/`SplitQualified` and therefore from search, list, show, the MCP tool descriptions, the `available: …` list in an error, and `mk_show`'s ambiguity count. See the note below on why that distinction is the security property. Policy validation runs at config-load time (unknown capability, rule with no collections, non-https issuer, rules without providers) so a policy that would silently grant nothing fails the process instead. |
| `mk mcp serve-http` — who may read another principal's PERSONAL memory | A caller reading, enumerating or inferring the existence of a memory somebody else saved for themselves | A personal memory is readable only by the principal whose namespace it is in — the verified `(iss, sub)` pair, never a tool argument, and never a mutable claim like email/groups/tenant. Enforcement is by *narrowing the per-request registry view* once (`Registry.ViewedBy`), exactly as collection authorization narrows it, so search, list, show, page counts, snippets and `mk_show`'s ambiguity count all inherit it rather than each checking. In search the filter is a **mandatory clause inside the bleve query**, boosted to zero, so ineligible documents are excluded before ranking and before the `limit` truncation — a post-filter over the top N would both leak metadata and silently return an empty result when the limit was consumed by hidden documents. An unauthorized read answers `not found`, byte-identical to a page that was never written, for bare and qualified IDs alike. `admin` does not confer it: capabilities are held over a collection, and ownership is not a capability. An operator may opt a collection back into the old collection-wide behaviour with `memory.personal_visibility: collection`, which logs a startup warning under OIDC. See [design/memory.md](design/memory.md#private-personal-reads-27). |
| `mk mcp serve-http` unauthenticated endpoints (`/livez`, `/readyz`, `/metrics`, `/.well-known/oauth-protected-resource`) | Enumeration of the deployment by an unauthenticated caller | Probes and metrics are deliberately unauthenticated (an orchestrator and a scrape job have no OIDC token, and a probe that can fail for auth reasons restarts healthy pods), so they are written to carry nothing: `/readyz` reports **counts and state, never collection names** — ready/degraded/total, with the names, bucket, generation and error text behind a failure going to the structured log instead; no metric carries a collection name, bucket, object path, memory key, page ID, query or caller subject as a label; the `route` label is the server's own matched mux pattern from a closed set, never `r.URL.Path`, so a scanner can't add time series. The refresh series (`meerkat_refresh_*`) are labelled by the collection's configuration **ordinal** and a two-value kind, and deliberately **not** by the source generation or fingerprint — that increments forever, so one series per publication would be an unbounded cardinality leak as well as a disclosure; the version travels in the log and in authenticated collection discovery. The RFC 9728 metadata is public by definition — its job is to be readable by a client that has no token yet — and contains only the resource identifier and the configured issuers. `GET /` names no collection. |
| `mk mcp serve-http` access logs | Credential or membership disclosure via logging | The `Authorization` header, the raw token, group membership and request bodies are never logged. Logged: method, path, status, duration, bytes, peer, user agent, MCP session ID, and (authenticated only) `sub`/`issuer`/`tenant` — an audit trail without a directory dump. `X-Forwarded-For` is deliberately **not** consulted: it is client-controlled unless a trusted proxy rewrites it, and meerkat has no way to know that. **The server has no TLS of its own**; default bind is loopback. Exposed beyond one host without a TLS-terminating proxy, bearer tokens and every response body cross the network in plaintext. |
| `mk http serve` API key, and the traffic it guards | Disclosure — in code/logs, or on the wire | Key comparison is constant-time (`subtle.ConstantTimeCompare`), the key is never echoed, and the server refuses to start without one. **The server has no TLS of its own** (`ListenAndServe`, never `ListenAndServeTLS`) — default bind is loopback (`127.0.0.1`). Exposed beyond one host without a TLS-terminating reverse proxy in front, the bearer token and every response body cross the network in plaintext. See `docs/INTEGRATION-OPENWEBUI.md` for the reverse-proxy pattern. |
| `mk ingest --execute` spawns an agent CLI (`opencode` or `claude`) | Prompt injection: `Task.Prompt` is rendered from `ingestion/prompts/*.md` in the ingested content source, so a malicious prompt file in any source repo in `sources.yaml` is an arbitrary-action path, running with `cmd.Dir`/`--dir` set to a working copy that holds push credentials, at the operator's full privilege. The generated instruction itself includes a `git push` recipe. | Ingested content is treated as **trusted input** to the agent — meerkat does not sandbox or vet it. The real control is permission prompts: by default the agent CLI runs *with* its normal permission prompts, so an injected instruction still has to get past those before it acts. `--trust-sources` disables the prompts (passes `--dangerously-skip-permissions` to the agent CLI) for unattended/CI runs; it prints a stderr warning before executing. Operators who enable `--trust-sources` must trust every source repo listed in `sources.yaml` — as much as they trust code they'd merge unreviewed. |
| Templates / prompts / sources.yaml | Tampering at build time | Embedded at build time; `make security` includes them in the gosec walk. **When served from a runtime content source instead** (`--kb-dir`/`MEERKAT_KB_DIR`, or a `content-source.yaml` `type: local`/`type: url`/`type: gcs` source), the three rows above apply here too: unverified for `disk:<path>`, digest-verified (with the same caveats) for `url:<url>@...`, generation-pinned for `gcs://...` — either way, outside the gosec walk and the cosign-signed release. |

### Collection authorization: invisible, not denied

`mk mcp serve-http`'s access control has one rule worth stating on its
own, because the intuitive implementation of it is insecure.

A collection a caller may not read is **invisible** — filtered out of the
registry at the start of the request, so every surface downstream behaves
as if it were never mounted. It is *not* denied per operation.

The difference is not cosmetic. meerkat's multi-collection routing (see
[design/multi-collection.md](design/multi-collection.md)) is full of
messages that name collections, deliberately, to make errors actionable:

- `mk_show`'s ambiguity error lists every collection holding a page ID,
  so the caller can re-ask with a qualified one;
- an unknown-collection error ends `available: <the mounted set>`;
- the MCP tool descriptions name the mounted collections, which is how a
  client discovers them without an extra tool call.

Under a per-operation 403, each of those becomes an **enumeration
oracle**: an authenticated caller with no access to anything learns the
name of every collection, and — via the ambiguity error — which
documents live in which. For a knowledge base the names are frequently
the sensitive part (`incident-2026-03-payments`,
`acquisition-target-research`). A 403-vs-404 split on `mk_show` is a
per-page existence oracle on top of that.

Filtering the registry closes all of them at once, and closes the ones
added later for free. Specifically:

- the ambiguity count and its suggested qualified IDs reflect the
  caller's view, so a page in three collections is unambiguous for a
  caller who can read one;
- a hidden collection name produces a byte-identical error to a
  never-mounted one, `available:` list included;
- `<hidden>:<page-id>` stops parsing as a collection qualification and
  404s as a bare page ID;
- `tools/list` and `tools/call` are both rebuilt through mcp-go's tool
  filter, so a caller with no readable collection is offered no KB tools
  and cannot invoke them either.

The one thing meerkat *does* say plainly is that a caller has no access
at all: a token that verifies but matches no policy rule gets **403**.
That is a statement about the caller, identical whether the deployment
mounts one collection or fifty, and it names none of them.

### Personal memories: private to read, not just to write

`mk_save_memory`'s `personal` scope means private in both directions.
The reasoning is the same one behind invisible collections, and the
mistakes available here are the same shape, so it is worth stating the
three decisions that carry it.

**Ownership comes from the page ID.** A personal memory is stored at
`personal/<namespace>/<slug>.md` and served at page ID
`memory/personal/<namespace>/<slug>`, where the namespace is
`sha256(iss + "\x00" + sub)`. Every read surface derives the owner from
that ID. The alternatives were a frontmatter field (which the document's
own caller-written bytes could claim) or a struct field stamped by a
constructor (which a constructor could forget to set, producing a
*public* page — a silent failure in the wrong direction). Deriving from
the ID has neither failure mode: the ID is built from the verified
identity and from no caller input, so it is an unforgeable carrier.
`memory/personal/` is therefore a **reserved page-ID prefix**: an
ingested content page that happens to sit under it is treated as private
too, which is the safe direction.

**Filtering happens before ranking and truncation.** The visibility
condition is a clause in the bleve query, conjoined with the content
clauses and boosted to zero so it changes eligibility and not relevance.
Post-filtering the top N results would be a security bug *and* a
correctness bug: a caller whose ten best-scoring documents were somebody
else's private memories would receive an empty result and be told,
truthfully from its point of view, that the knowledge base has no
answer. A test pins this with fifty private documents that outrank the
one public match and a limit of five.

**Unauthorized is indistinguishable from nonexistent.** A guessed ID —
bare, qualified as `<collection>:<id>`, or with an explicit `collection`
argument — produces the same answer as an ID nobody has ever written.
The ambiguity error counts only pages the caller may see, so the same
personal key saved into two collections is an ambiguity for its owner
and a plain not-found for everyone else. Page counts in
`mk_list_collections` do not move when another principal saves.

**Where the line is drawn.** `team` and `global` memories are unchanged:
readable by every reader of the collection. This is not a general
row-level authorization language for arbitrary pages — the collection
remains the unit of read access for ordinary content. And a hosted
server that cannot name its caller (no `auth:` block, or
`allow_unauthenticated`) gives that caller **no** personal memories at
all rather than defaulting them into a shared namespace, matching the
fact that it already refuses to let them write one. Local and stdio
usage serve one principal, who owns the `local` namespace their own
memories are written into, and are unaffected.

### `kb_commit` vs. `kb_source`: the provenance split

`mk version` exposes two fields that answer different questions and must
not be conflated:

- **`kb_commit`** — the commit of the content source `content-source.yaml`
  pointed at when *this binary* was built. Fixed at build time. Unaffected
  by any runtime content resolution — it never changes to reflect a
  runtime directory or archive.
- **`kb_source`** — what's actually being served for *this invocation*:
  `embedded`, `disk:<path>` (`--kb-dir`/`MEERKAT_KB_DIR`, or a `type: local`
  `content-source.yaml` source), `url:<url>@<digest12>` (a `type: url`
  `content-source.yaml` source — `<digest12>` is the first 12 hex
  characters of its verified `sha256`), `gcs://<bucket>/<object>@<gen>` /
  `gcs://<bucket>/<prefix>*@<fingerprint>` (a `type: gcs` source), or
  `collections:<n>` when several named collections are mounted at once —
  in which case each collection's own `kb_source`-vocabulary provenance
  is reported in `mk version --json`'s `collections` array. Set once per
  invocation, before any subcommand runs — with one deliberate exception:
  a collection configured for runtime reconciliation (`refresh:`, see
  [design/hot-reload.md](design/hot-reload.md)) updates its own
  provenance when a new generation is swapped in, because the whole
  point of the string is to name the bytes actually being served. A
  long-running hosted process therefore reports the generation it is
  serving now, not the one it booted on.

When `kb_source` is `embedded`, `kb_commit` describes what's being served,
and the "Embedded wiki content" mitigation above applies in full. In every
other case, `kb_commit` is still reporting the build-time embedded
commit — content that is **not** being served — and no part of the
release's SHA256/cosign coverage extends to what `kb_source` names.

For `disk:<path>`, that directory could hold anything: content edited by
hand after `mk ingest`, a stale checkout, or a directory swapped in by
anyone with filesystem access. Meerkat does not check, hash, or sandbox it.

For `url:<url>@<digest12>`, more is true but not everything. What *is*
verified: `FetchURL` refuses to extract or cache anything whose sha256
doesn't match `content.sha256` exactly, so the bytes actually served are
guaranteed to be the bytes that digest names — tampering in transit, or at
rest wherever `url` is hosted, is detected rather than silently served.
What this does **not** buy: the archive is not part of the release's
cosign-signed checksums — that signature covers the binary's own
checksums file, not an arbitrary operator-named URL, so a `type: url`
source sits entirely outside it regardless of its digest — and meerkat has
no way to know whether the `url`/`sha256` pair in the operator's own
`content-source.yaml` names the archive the operator actually intended. A
correct-looking digest for the wrong archive (swapped in at
config-authoring time, say) verifies exactly as cleanly as the right one.
That choice remains fully the operator's, unverified by meerkat — same as
`disk:<path>`, just one step narrower: verified bytes, unverified choice of
which bytes.

This is deliberate, not an oversight: `kb_source` exists as its own field
precisely so `mk version`'s output cannot be read as implying an integrity
guarantee it doesn't have. An operator who configures runtime content —
`--kb-dir`/`MEERKAT_KB_DIR`, or any `content-source.yaml` source — is
trusting its origin themselves, the same way `--trust-sources` requires
trusting every source repo in `sources.yaml`. `type: url`'s digest narrows
*what* is being trusted (exact, verified bytes, not a mutable path) without
removing the need to trust the operator's own configuration — full
operator responsibility either way, no meerkat-side verification of that
choice.

### OKF `trust_tier` is advisory, not verified

Serving a third-party [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle (see [OKF.md](OKF.md)) doesn't change the threat model above —
it's still runtime KB content, unverified or digest-verified per the
same `disk:<path>` / `url:<url>@<digest12>` rows depending on how it's
configured. The one thing worth calling out on its own: `mk show` /
`mk_show` / `POST /show` surface a `trust_tier`
(`unverified`/`machine-confirmed`/`human-reviewed`) derived from the
bundle's own `verified` frontmatter. That value is metadata **asserted
by whoever produced the bundle**, not something meerkat checks — a
producer can write `verified: { by: human:anyone, at: ... }` on any
concept regardless of whether a human actually looked at it. Read
`trust_tier` the same way as `kb_source`'s `disk:<path>` label: a
provenance signal to inform a human or agent's judgment, not an
access-control or integrity guarantee.

---

## Reporting a vulnerability

For a security issue you'd rather not file as a public issue,
open a private security advisory on the project's GitHub repository
(Security → Advisories → "Report a vulnerability"). This keeps the
report confidential until a fix is published. We aim to respond
within 7 days and cut a `vX.Y.Z` patch release once the fix lands.
