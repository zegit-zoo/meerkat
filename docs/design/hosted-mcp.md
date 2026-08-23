# Spec: Hosted Streamable HTTP MCP server with OIDC and fine-grained authorization

**Status:** Implemented (`internal/mcp` hosted transport, `internal/authn`, `internal/authz`; `mk mcp serve-http`) · **Builds on:** [multi-collection.md](multi-collection.md) · **Issue:** #9

*(`docs/design/` is a design record. The up-to-date schema reference is
[content-source.example.yaml](../../content-source.example.yaml); the
user-facing reference is the [README](../../README.md).)*

## Summary

meerkat's MCP server ran on stdio: one process per client, spawned by
that client, trusted because the user started it. That model has no
answer for the deployment every organization actually wants — one
meerkat, many people, many collections, and different people allowed to
see different collections.

This spec adds a second transport (**MCP Streamable HTTP**), an
**OpenID Connect** front door, and **collection-level authorization**
keyed to the caller's verified identity. It also gives the hosted
process the things a hosted process needs: liveness and readiness
probes, Prometheus metrics, and structured access logs.

Two constraints shape everything below.

1. **Back-compat is mandatory.** `mk mcp serve` on stdio is unchanged
   and unauthenticated. `mk http serve` keeps its single static bearer
   token. A `content-source.yaml` with no `auth:` block produces a
   hosted server that behaves exactly like the stdio one, over HTTP.
2. **An unauthorized collection is invisible, not denied.** This is the
   whole design, and the next section is about it.

## Non-goals

- **Writes.** The capability model defines `personal-write`,
  `team-write` and `admin`, and the policy parses, evaluates and
  reports them — but only `read` is enforced, because only reads exist.
  The memory toolset (the next issue) is what consumes the rest.
  *(Superseded by #10: see [memory.md](memory.md). The write
  capabilities are enforced now, and `global-write` was added alongside
  them rather than folded into `admin`.)*
- **Being an authorization server.** meerkat is an OAuth 2.0 *protected
  resource*. It validates tokens; it never issues them, never runs a
  redirect flow, and never sees a client secret.
- **TLS.** Terminate at a reverse proxy, exactly as `mk http serve`
  documents.
- **Per-collection ingestion / write-back.** Unchanged from #8.
- **Row- or page-level authorization.** The collection is the unit. A
  page's visibility is its collection's visibility.

## Invisibility, and why it is the load-bearing decision

The obvious implementation of "restrict access to collections" is a
check at each operation: resolve the request, notice the caller lacks
`read`, return 403. That implementation leaks, and #8's spec called it
out in advance because the multi-collection routing rules make the leak
unusually rich:

| surface | what a *denying* implementation tells an unauthorized caller |
| --- | --- |
| `mk_show` ambiguity error | `"shared/overview" … it exists in 3 collections — ask for one of runbooks:…, architecture:…, secrets:…` — the name of every collection holding that page, and the fact it holds it |
| unknown-collection error | `available: runbooks, architecture, secrets` — the full mounted set, from any authenticated caller |
| MCP tool descriptions | `This server mounts 3 collections (runbooks, architecture, secrets)` — the mounted set, in the one response every MCP client fetches first |
| `GET /collections` | the same, plus each collection's provenance |
| 403 vs 404 on `mk_show` | a per-page existence oracle: probe an ID, read the status code |

Each of those is an *enumeration oracle*: a caller who can reach the
server learns what exists, what it is called, and which documents live
where — from a server that believes it denied them. For a knowledge
base, the names alone are often the sensitive part
(`incident-2026-03-payments`, `acquisition-target-research`).

So authorization is applied by **narrowing the registry**, once, at the
start of the request. The caller's `*collections.Registry` is a view
containing only the collections they may read. Every operation then runs
against a registry that, as far as it can tell, never mounted the rest.

```
request → verify token → evaluate policy → Registry.Restrict(grants.CanRead) → handlers
```

`Registry.Restrict` returns a derived registry sharing the same
`*Collection` values in the same configuration order. The filter is not
inside `target()` — it is *upstream* of `target()`, which is strictly
stronger: `Names()`, `All()`, `Get()`, `Len()`, `Single()`,
`SplitQualified()` and `Provenance()` do not route through `target()`
and are every bit as much enumeration surfaces. Filtering the registry
covers all of them at once, and covers any method added later for free.

The consequences fall out, rather than being implemented one by one:

- **Ambiguity counts the caller's view.** A page in three collections is
  ambiguous for a caller who can read two, and unambiguous — silently,
  correctly — for a caller who can read one.
- **A hidden name errors identically to a missing name.** `Get("secrets")`
  and `Get("nonexistent")` produce the same sentence with a different
  word in it, and neither `available:` list mentions anything hidden.
- **A qualified ID degrades.** `SplitQualified` recognises a
  `<collection>:` prefix only for a *mounted* collection; on a
  restricted view "mounted" means "visible", so `secrets:payroll/x`
  stops parsing as a qualification and 404s as a bare page ID.
- **Tool descriptions are rebuilt per caller.** `WithToolFilter` runs on
  `tools/list` *and* `tools/call`, so the rebuilt descriptions are an
  access boundary, not a display fix. A caller with no readable
  collection is offered no KB tools at all.
- **One visible collection is single-collection meerkat.** `Single()`
  is true, nothing is qualified, and the UX is the pre-collections one.

Sharing the `*Collection` values is what makes this cheap: the
per-collection bleve indexes #8 built exist precisely so a filtered
search needs no rebuild. A view is a slice and a map of at most N
entries. It borrows rather than owns, so `Close` on a derived registry
is a no-op — a per-request view must not tear down indexes the server is
still serving other requests from.

**Where the line is drawn.** Invisibility is about *collections*, not
about the caller. A caller whose token verifies but whom no rule matches
gets **403** — a statement about them, identical whether the deployment
mounts one collection or fifty, and it names none of them. Telling
someone "you have no access here" leaks nothing; telling them "you have
no access *to `secrets`*" leaks everything.

## Transport

`mk mcp serve-http` runs mcp-go's `StreamableHTTPServer` — the MCP
Streamable HTTP transport (POST for requests, GET for the SSE stream,
DELETE to terminate a session), already present in the pinned mcp-go
v0.57.0, so no dependency bump was needed.

The tool set is registered once, by a constructor both transports share
(`newServer`), so stdio and HTTP cannot drift: a tool added for one is
present on the other.

| | `mk mcp serve` | `mk mcp serve-http` |
| --- | --- | --- |
| transport | stdio | Streamable HTTP |
| sessions | one, implicit | many, concurrent |
| auth | none (the user started the process) | OIDC bearer, or none if unconfigured |
| collections visible | all | the caller's |

Defaults: `/mcp` on `127.0.0.1:4005`, stateless session IDs (any
well-formed ID is accepted, so replicas need no sticky routing —
`--stateful` opts into in-process session state), a 30s SSE heartbeat so
proxies don't reap idle streams, and a 30m idle sweep so a client that
vanishes without a DELETE doesn't leak session state.

`WriteTimeout` is deliberately **0**. A Streamable HTTP GET is a
long-lived SSE stream, and a write deadline would sever it mid-session;
per-request work is bounded by the query timeout instead.

DNS-rebinding protection (mcp-go's rejection of loopback requests whose
`Host` is not a localhost value) stays on. `--trust-proxy-host` disables
it for a same-host proxy that preserves the original `Host`; rewriting
`Host` at the proxy is the better fix and the flag says so.

## Authentication

Generic OIDC, via `github.com/coreos/go-oidc/v3`. An issuer URL is
discovered at `<issuer>/.well-known/openid-configuration`, its JWKS is
fetched and cached, and each bearer token's signature, `iss`, `aud` and
`exp` are verified. **Entra ID, Google Workspace and Okta are
configuration, not code** — there is no provider-specific branch
anywhere in `internal/authn`, only a per-provider claim mapping for the
fact that directory products disagree about which claim carries groups,
email and tenant.

Discovery runs **at startup**, once per provider. An unreachable or
misconfigured issuer fails the process where a deployment's own health
gate catches it, rather than surfacing as intermittent 401s under load.

**The token must be a JWT.** meerkat verifies signed JWTs — an ID token,
or (more usually) a JWT access token minted for `auth.resource` as a
custom API audience. Entra ID and Okta issue JWT access tokens for a
registered API; Google's OAuth access tokens are opaque, so a Google
deployment audiences an ID token at the OAuth client ID instead.
Opaque-token introspection (RFC 7662) is not implemented.

**Audience is mandatory.** `auth.providers[].audience` defaults to
`auth.resource`, and a provider with neither is refused at construction.
An audience-unbound resource server accepts any token from the same IdP
— including one minted for an unrelated relying party by a caller who
legitimately holds it. It is the single most consequential
misconfiguration available here, so it is not expressible.

### The 401 handshake (RFC 9728)

```
→  POST /mcp                            (no token)
←  401 Unauthorized
   WWW-Authenticate: Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp",
                     error="invalid_request", error_description="a bearer token is required"

→  GET  /.well-known/oauth-protected-resource[/mcp]      (public)
←  200 {"resource":"https://mcp.example.com/mcp",
        "authorization_servers":["https://login.microsoftonline.com/<tenant>/v2.0"],
        "bearer_methods_supported":["header"]}
```

The client discovers where to get a token with no out-of-band
configuration. RFC 9728 §3.1 puts a path-qualified resource's metadata
under the well-known prefix; meerkat serves **both** that path and the
bare one, because clients guess the bare one.

| condition | response |
| --- | --- |
| no `auth:` block | request proceeds, no policy in force |
| `allow_unauthenticated: true` | as above, explicitly (for a gateway that authenticates first) |
| no token | 401 + challenge |
| token fails signature / issuer / audience / expiry / tenant | 401 + challenge, `error="invalid_token"` |
| token verifies, no rule matches | 403, naming nothing |
| token verifies, rules match | proceed with grants in context |

## Configuration schema

The `auth:` block lives in `content-source.yaml`, or in a standalone
file passed to `--auth-config` (same block, same validation) — content
and access policy often have different lifecycles and different owners,
and neither arrangement is privileged.

```yaml
auth:
  # RFC 9728 resource identifier: the https URL clients reach this
  # server at. Published in the metadata and required in every token's
  # audience.
  resource: https://mcp.example.com/mcp

  providers:
    - issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
      audience: api://meerkat          # defaults to auth.resource
      claims:
        groups: groups                 # "roles" for Entra app roles
        email: preferred_username      # "email" by default
        tenant: tid                    # "tid" by default
      require_tenant: <tenant-id>      # optional; pins a multi-tenant issuer

  rules:
    - name: sre
      groups: [sre, oncall]            # any-of, case-insensitive
      collections: [runbooks, architecture]
      capabilities: [read]             # defaults to [read]

    - name: platform-admins
      groups: [platform-admins]
      collections: ["*"]               # every collection, including future ones
      capabilities: [admin]

    - name: everyone
      collections: [public]            # no selector: every authenticated caller
```

**Selectors** are `subjects` (exact), `emails` (case-insensitive),
`groups` (any-of, case-insensitive), `tenant` and `issuer`. Within a
selector, any value matches; **across** selectors, all must match. A
rule with no selector matches every authenticated caller.

**Evaluation is union-only.** Every matching rule contributes; there is
no ordering, no first-match-wins, and no deny rule. That means a
policy's effect can be read off any single rule without holding the
whole document in your head to check whether a later line takes it away,
and the safe edit — narrowing access — is deleting a rule, which is hard
to get wrong.

Validation runs at **load** time, not at first request: an unknown
capability, a rule with no `collections`, a non-https issuer, a duplicate
issuer, `rules:` with no `providers:`, or `allow_unauthenticated`
combined with `providers` all fail the process. The class of error this
prevents is the quiet one — a policy that looks configured and grants
nothing, discovered weeks later as "why can't this user see anything".

## Capability model

| capability | means | enforced today |
| --- | --- | --- |
| `read` | search / show / list the collection — and therefore whether it is visible at all | **yes** |
| `personal-write` | save a memory into the caller's own namespace | **yes**, since #10 |
| `team-write` | save a memory into the collection's team space | **yes**, since #10 |
| `global-write` | save a memory into the collection's global space | **yes**; added by #10 |
| `admin` | implies every capability, present and future | implication only |

`admin` implying capabilities that do not exist yet is deliberate and
documented: a rule granting `admin` today also grants whatever a later
meerkat adds, which is what "admin" should mean and is why the schema
says to grant it sparingly.

At the time of writing this spec the write capabilities were defined,
parsed, evaluated and reported but unenforced, so that a policy an
operator wrote then would keep meaning the same thing when the memory
toolset started consuming it. #10 is that toolset — see
[memory.md](memory.md) for the scope→capability table, and for why
`global-write` was added as a capability of its own rather than folded
into `admin` (folding it in would have retroactively widened every
`admin` rule already written against this spec).

## Production endpoints

| endpoint | auth | answers |
| --- | --- | --- |
| `/livez` | none | is this process running |
| `/readyz` | none | can it serve requests right now |
| `/metrics` | none | Prometheus |
| `/.well-known/oauth-protected-resource[/…]` | none, by definition | where to get a token |
| `/mcp` | OIDC | the MCP transport |

The probes and metrics are unauthenticated because an orchestrator and a
scrape job have no OIDC token and shouldn't need one — a probe that can
fail for auth reasons is a probe that restarts healthy pods. The cost is
bounded by making them say nothing worth having:

- **`/readyz` reports counts, never names** (`{"status":"ready",
  "collections":{"ready":3,"total":3}}`). The mounted set is not public
  information; the names behind a failure go to the structured log.
- **No metric carries a collection name, a page ID, a query or a caller
  subject as a label.** The `route` label is the server's own matched
  mux pattern from a closed set, never `r.URL.Path` — so a scanner
  probing `/wp-admin` collapses to `route="other"` instead of adding a
  time series.

### What readiness actually checks

`Registry.Check()` re-derives, per collection: the content root is
reachable, the pages enumerate, the search index is built. Cached for 5s,
because enumeration is real work on a large KB and a 1s probe interval
should not be doing it.

The subtle part is the content root. `kb.ListFS` deliberately degrades a
missing `content/` to an *empty page list* rather than an error, so that
a partially-populated `--kb-dir` serves what it has. Right for serving,
useless for a probe: an unmounted volume would read as a healthy, empty
knowledge base forever. So each collection latches, at startup, whether
its content root was reachable, and only a **regression** from reachable
to unreachable un-readies the process. A deployment that legitimately
starts with no `wiki/` stays ready; one whose volume goes away does not.

Liveness never touches content. Restarting the process does not remount
a volume, so a content failure that failed liveness would produce a
crash loop instead of a diagnosis.

### Metrics

```
meerkat_http_requests_total{route,method,status}
meerkat_http_request_duration_seconds{route,method}
meerkat_auth_failures_total{reason}          # missing_token | invalid_token | no_grants
meerkat_mcp_tool_calls_total{tool,outcome}   # ok | tool_error | error
meerkat_mcp_tool_duration_seconds{tool}
meerkat_mcp_sessions_active
meerkat_collections_mounted
meerkat_ready
meerkat_build_info{version}
```

`tool_error` (a bad query, an unknown collection — the model's problem,
handed back as a normal result) is kept apart from `error` (a transport
failure — meerkat's problem). Conflating them would make a dashboard
read as broken every time a model mistyped a page ID.

Collectors go on a registry owned by the server, not
`prometheus.DefaultRegisterer`, so two servers in one test binary don't
collide.

### Access logging

One structured line per request: method, path, status, duration, bytes,
peer, user agent, MCP session ID, and — for an authenticated request —
`sub`, `issuer` and `tenant`.

Deliberately **not** logged: the `Authorization` header or any part of a
token; the caller's group membership (a full directory-group list per
request is a liability and tells an operator nothing actionable); request
bodies. `X-Forwarded-For` is not consulted either — it is
client-controlled unless a trusted proxy is known to rewrite it, and
meerkat has no way to know that here.

The access log sits *above* the authentication gate so that 401s are
logged too, which is the line an operator most wants; the identity
travels back up through a small holder in the request context, since the
gate installs grants on a derived request the outer frame never sees.

## Back-compat

| deployment | after this change |
| --- | --- |
| `mk mcp serve` (stdio) | byte-identical; no grants ever enter the context, `visible()` returns the registry unchanged |
| `mk http serve` | unchanged; static bearer token, all collections |
| `content-source.yaml` with no `auth:` | unchanged everywhere; `mk mcp serve-http` serves every collection to any caller and says so in its banner |
| `--kb-dir` | suppresses `auth:` discovery, exactly as it suppresses content discovery |

`Grants.Can` on a **nil** `*Grants` returns true — "no policy in force"
is unrestricted. That is the back-compat path and it is the reason a nil
`*Grants` is kept distinct from an empty non-nil one ("a policy ran and
granted this caller nothing"). The gate always installs a non-nil
`*Grants` when providers are configured, so the fail-open default is
reachable only where it is correct.

## Testing strategy

- **OIDC against a local fake issuer** (`internal/authn/authntest`):
  httptest serving a real discovery document and JWKS, minting real
  RS256 JWTs against a generated key. Signature, issuer, audience,
  expiry, missing subject, tenant mismatch, forged signature, custom
  claim mappings, a scalar group claim, several issuers at once, and
  startup failure on unreachable discovery. No network, no real IdP.
- **Invisibility** (`internal/collections/restrict_test.go`): one test
  per oracle in the table above — the ambiguity count, the identical
  error shapes for hidden vs absent, the degraded qualified ID, the
  `available:` list, `Names`/`All`/`Len`, order preservation, and that a
  view borrows rather than owns its indexes.
- **End to end over the wire** (`internal/mcp/hosted_test.go`): a real
  mcp-go client against a real hosted server — 401 and its challenge,
  403 for a matched-by-nothing token, the metadata at both paths, and
  every invisibility case again through `tools/list`, `mk_list`,
  `mk_search` and `mk_show`.
- **Probes**: a directory-backed registry whose content is removed
  mid-flight, asserting `/readyz` goes 503 while `/livez` stays 200.
- **Concurrency**: twelve simultaneous sessions from two populations
  with disjoint access, asserting each sees its own collections and
  never the other's — a grant that bled across sessions shows up as a
  wrong answer, not merely as a race.
- **Metrics and logs**: labels asserted present, and page IDs, subjects,
  collection names and scanned paths asserted absent.

## Follow-ups

- ~~**Memory toolset** (the next issue): `mk_save_memory` with
  personal/team/global scopes keyed to the OIDC identity.~~ **Done in
  #10** — see [memory.md](memory.md). It consumes
  `Grants.Capabilities(name)` and `Grants.Identity()`, and adds a second
  registry view (`Restrict(CanWrite)`) alongside `Restrict(CanRead)`
  rather than widening the read one.
- **OIDC for `mk http serve`.** The OpenWebUI-facing server still takes
  a single static token. `internal/authn.Gate` is transport-agnostic and
  would drop straight in.
- **Token caching.** Every request re-verifies its JWT. Signature
  verification is cheap and the JWKS is cached, but a short-lived
  decision cache keyed by token hash would cut it further under load.
- **Scope-based authorization.** Rules match claims, not OAuth scopes.
  The metadata advertises scopes as a hint to the authorization server
  only.
- **Per-collection refresh**, still open from #8: content resolves once
  at startup, and `/readyz` now reports when that content goes away but
  cannot re-resolve it.
