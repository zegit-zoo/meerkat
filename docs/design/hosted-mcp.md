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

**One level down.** #27 added a second narrowing of the same shape, for
the one kind of page whose ID says whose it is: a **personal memory** is
visible only to the principal who saved it.
`Registry.ViewedBy(kb.Viewer)` is `Restrict`'s per-page counterpart —
a derived, borrowing view carrying a viewer, consulted by every read
below it rather than checked per operation — and it inherits every
consequence listed above, including the ambiguity count and the
identical-error property. Search additionally carries a mandatory
visibility clause *inside* the bleve query, because ranking and
truncation happen there and a post-filter over the top N cannot be made
correct. The collection is still the unit of read access for ordinary
content; see [memory.md](memory.md#private-personal-reads-27).

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
| collections visible | all | the caller's — including, since #36, the ones explicitly published to callers with no token |

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
| no `Authorization` header, an `anonymous:` rule publishes something | proceed with the **anonymous grants** in context (#36) |
| no `Authorization` header, nothing published | 401 + challenge |
| `Authorization` present but unusable (`Bearer` with no value, another scheme) | 401 + challenge — an *attempted* credential is not an absent one |
| token fails signature / issuer / audience / expiry / tenant | 401 + challenge, `error="invalid_token"` — **never** a downgrade to anonymous |
| token verifies, no rule matches, nothing published | 403, naming nothing |
| token verifies, no rule matches, something published | proceed with the published set |
| token verifies, rules match | proceed with grants in context (own rules ∪ anonymous rules) |

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

    - name: public-handbook
      anonymous: true                  # no token at all (#36)
      collections: [handbook]
      capabilities: [read]             # the only capability an anonymous rule may hold
```

**Selectors** are `subjects` (exact), `emails` (case-insensitive),
`groups` (any-of, case-insensitive), `tenant`, `issuer` and `anonymous`.
Within a selector, any value matches; **across** selectors, all must
match. A rule with no selector matches every authenticated caller.

**Evaluation is union-only.** Every matching rule contributes; there is
no ordering, no first-match-wins, and no deny rule. That means a
policy's effect can be read off any single rule without holding the
whole document in your head to check whether a later line takes it away,
and the safe edit — narrowing access — is deleting a rule, which is hard
to get wrong.

Validation runs at **load** time, not at first request: an unknown
capability, a rule with no `collections`, a non-https issuer, a duplicate
issuer, `rules:` with no `providers:`, `allow_unauthenticated` combined
with `providers`, and — since #36 — an `anonymous:` rule carrying a claim
selector, carrying a write capability, or combined with
`allow_unauthenticated` all fail the process. The class of error this
prevents is the quiet one — a policy that looks configured and grants
nothing (or everything), discovered weeks later as "why can't this user
see anything".

## Explicit anonymous access to selected collections (#36)

A hosted meerkat served nothing without a token, or — via
`allow_unauthenticated` — everything without one. The middle ground
operators actually want is a *published* collection: a handbook, a
policy set, a public API guide, readable by anyone, while every other
collection keeps requiring a token on the same endpoint.

### The shape: a rule selector, not a second mechanism

```yaml
auth:
  resource: https://mcp.example.com/mcp
  providers:
    - issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
      audience: api://meerkat

  rules:
    - name: public-handbook
      anonymous: true                  # matches the caller with NO token
      collections: [handbook, published-policies]
      capabilities: [read]             # anonymous rules are read-only

    - name: sre
      groups: [sre]
      collections: [runbooks]
```

The alternative considered was a separate top-level block —
`auth.anonymous: {collections: [...]}` — and it was rejected for three
reasons.

1. **It would have forked the schema.** A block needs its own
   collections list, its own capability list, its own wildcard
   handling, its own validation and its own documentation, all of it a
   near-copy of `rules:`. A selector reuses every one of them, so
   `collections: ["*"]` and `capabilities:` mean exactly what they mean
   everywhere else, and a future capability is covered by existing code.
2. **Union-only evaluation stays readable.** meerkat's policy has one
   list, no ordering and no deny rule, precisely so a rule's effect can
   be read off that rule. A second block would be a second place to
   look before you know what a caller can see.
3. **It puts the fact in the diff.** `anonymous: true` on a line next to
   `groups: [sre]` is a review artifact. A block somewhere else in the
   file is one an eye slides past.

The selector is **mutually exclusive** with `subjects`, `emails`,
`groups`, `tenant` and `issuer`, and validation refuses the combination
rather than ignoring it: those select on claims of a verified token,
there is no token here, and a rule that reads as a narrowing while acting
as none is the exact failure this file's validation exists to prevent.

### Grant synthesis, and the absence of a second path

There is no anonymous *code path*. The gate calls
`Policy.EvaluateAnonymous()`, which runs the same rule loop
`Policy.Evaluate` runs with one bit flipped, and installs the resulting
ordinary `*authz.Grants` — over an `Identity{}` with no subject — in the
request context. Everything downstream is the code that was already
there:

| surface | why it is already correct |
| --- | --- |
| `Registry.Restrict(g.CanRead)` | the anonymous caller is a restricted caller; the published set is their whole registry |
| `WithToolFilter` | rebuilds descriptions against that registry, so tools name the published set and nothing else |
| `mk_list_collections` | `g.Capabilities(name)` reports `["read"]`, because that is what the rule granted |
| update contracts | `EffectiveContract(g)` demotes on the caller's capabilities exactly as for a read-only authenticated caller |
| memory reads | `memory.Anonymous(id)` is true for a subjectless identity, so the viewer owns nothing (#27) |
| memory writes | anonymous grants hold no write capability, so `Restrict(CanWrite)` is empty and the tool is filtered out |

The one asymmetry is in which rules are *eligible*, and it runs both
ways on purpose:

- an `anonymous: true` rule contributes to **every** caller. Public means
  public: an operator who publishes a collection should not also have to
  add it to every other rule, and the day they forget, the people who
  lose access are their own staff while the internet keeps reading it.
- **every other rule** contributes to no anonymous caller — including a
  rule with no selector, which means "every *authenticated* caller".
  This is the whole of authenticated-by-default, and it is why an
  existing policy full of selector-less rules publishes nothing by
  upgrading.

### Anonymous grants are read-only

Validation refuses `personal-write`, `team-write`, `global-write` and
`admin` on an anonymous rule. A write by a caller nobody can name has no
owner, no audit trail and no reviewer; a personal memory would have no
namespace to land in (`internal/memory.Authorize` refuses it
independently, and both refusals are tested); and `admin` would confer
whatever capability a later meerkat adds — to the internet.

### Reconciliation with `allow_unauthenticated`: refused, not ranked

`allow_unauthenticated` already publishes **everything** and exists to
delegate authentication to a gateway. Combining it with an
`anonymous: true` rule is refused at load time.

Defining a precedence was the alternative, and either direction is
worse. If the flag wins, the rule is decorative — an operator reads a
carefully scoped list and gets their whole knowledge base. If the rule
wins, a flag documented as "no token is checked, nothing is filtered"
silently starts filtering, and the gateway deployment it exists for
breaks. Neither is discoverable from the config file, which is exactly
the class of quiet misconfiguration validation runs at load time to
catch. Refusing states the contradiction where it can be fixed.

An anonymous rule also requires `providers:`. Anonymous access is a
carve-out from an authenticated server; with no provider configured
there is nothing to carve out of, because every collection is already
readable without a token.

### The gate, precisely

The security-critical property is that **a present-but-invalid token is
never downgraded to anonymous**. The anonymous branch is reachable only
when the request carried no `Authorization` header at all — not when it
carried an expired one, a forged one, one for another audience, a
malformed one, `Bearer` with an empty value, or a scheme meerkat does
not accept. All of those keep their 401 and their `WWW-Authenticate`
challenge, whatever the policy publishes.

Two reasons, and both are worth the inconvenience of a client that has
to notice its own expiry:

- an expiry that silently degrades into partial data is an outage nobody
  sees. The client keeps working, keeps getting answers, and the answers
  keep quietly missing the collections the caller is entitled to.
- the 401 challenge is the *only* thing that tells a client to refresh.
  Answering 200 removes the signal that would have fixed the problem.

One pre-existing status code changes, deliberately: a verified token
matched by no rule used to get **403**, and now gets the published set
when there is one. Refusing that caller would be strictly stranger than
admitting them — they can read exactly the same bytes by dropping their
token. With nothing published, the 403 is unchanged.

### Telemetry and logs

| surface | what it says |
| --- | --- |
| `meerkat.authn.result` span attribute | `anonymous` — a sixth value in a closed set of six, no new key, no new dimension |
| `meerkat.authz.decide` span | the same `granted` boolean and `collections` **count** an authenticated decision carries |
| `meerkat_auth_anonymous_total` | a counter with **no labels**. Not a `reason="anonymous"` value on `meerkat_auth_failures_total`, because an admitted request is not a failure; and no label naming what was published, because /metrics is unauthenticated |
| access log | `"auth":"anonymous"` and **no** `sub`, `issuer` or `tenant` — not even empty ones. There is no principal, and a log line that printed `"sub":""` would invent one |
| `mcp.auth_anonymous` log line | Debug, not Warn: on a server that publishes a collection, anonymous traffic is the intended traffic |

`GET /` states the posture ("except for selected collections published
to unauthenticated callers") and names nothing — that page is
unauthenticated, and the mounted set is not public information. Which
collections are published is `mk_list_collections`'s answer, and it
answers each caller with their own set.

### Zero behaviour change without the feature

A policy with no `anonymous:` rule produces empty anonymous grants,
which the gate reads as "nothing to admit an anonymous caller to" and
401s exactly as before. `meerkat_auth_anonymous_total` is registered and
reads 0; the access log gains no field; the banner is unchanged. This is
asserted, not assumed — see `TestHostedAnonymous_NoAnonymousRuleChangesNothing`.

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

- **`/readyz` reports counts and state, never names** (`{"status":"ready",
  "collections":{"ready":3,"degraded":0,"total":3}}`). The mounted set is
  not public information; the names, and the bucket/generation/error text
  behind a failure, go to the structured log.
- **No metric carries a collection name, a bucket, an object path, a page
  ID, a query or a caller subject as a label.** The `route` label is the
  server's own matched mux pattern from a closed set, never `r.URL.Path`
  — so a scanner probing `/wp-admin` collapses to `route="other"` instead
  of adding a time series. Since #28 the same rule excludes a *source
  generation or fingerprint*, for a second reason: it increments forever,
  so one series per publication would be an unbounded cardinality leak.
  The refresh series are keyed by the collection's configuration ordinal
  instead.

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

**Ready and degraded are different axes (#28).** A collection whose last
runtime refresh failed is *degraded*: it is serving the last known-good
snapshot, answering every query correctly, with content that is merely
older than the bucket's. That is a staleness problem, not an availability
one, so under the default `failure_policy: serve-last-good` it stays
**ready** and `/readyz` answers 200 with `"status":"degraded"` and a
non-zero `degraded` count — something worth looking at, nothing that
should drain the replica. (It would usually drain *every* replica: they
all read the same bucket and would fail together.) `failure_policy:
unready` couples the two for a collection whose whole value is being
current, and then the ordinary not-ready path produces the 503. See
[hot-reload.md](hot-reload.md).

### Metrics

```
meerkat_http_requests_total{route,method,status}
meerkat_http_request_duration_seconds{route,method}
meerkat_auth_failures_total{reason}          # missing_token | invalid_token | no_grants
meerkat_auth_anonymous_total                 # admitted without a token (#36); no labels
meerkat_mcp_tool_calls_total{tool,outcome}   # ok | tool_error | error
meerkat_mcp_tool_duration_seconds{tool}
meerkat_mcp_sessions_active
meerkat_collections_mounted
meerkat_collections_ready
meerkat_collections_degraded
meerkat_ready
meerkat_build_info{version}

# only when a collection opts into runtime reconciliation (#28).
# `collection` is the configuration ORDINAL, never a name;
# `kind` is content | memory.
meerkat_refresh_attempts_total{collection,kind}
meerkat_refresh_changes_total{collection,kind}
meerkat_refresh_failures_total{collection,kind}
meerkat_refresh_skipped_total{collection,kind}
meerkat_refresh_duration_seconds{collection,kind}
meerkat_refresh_last_success_timestamp_seconds{collection,kind}
meerkat_refresh_degraded{collection,kind}
```

Since #30 an opt-in `observability:` block adds bounded domain series
beside these — index build, source resolution by bounded type, content
cache hit/miss, search duration and result-count, memory outcomes by
scope, coarse tool payload sizes, and the exporter's own health — all
under the same label discipline. Nothing above is removed or renamed,
and with no `observability:` block none of it is registered at all. See
[observability.md](observability.md).

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

Since #30 the line additionally carries `trace_id` and `span_id` when
tracing is on. The bridge is two IDs wide and goes one way: identity
stays in the log and never reaches a span, because a span is exported
out of the process and the log is not. See
[observability.md](observability.md).

## Back-compat

| deployment | after this change |
| --- | --- |
| `mk mcp serve` (stdio) | byte-identical; no grants ever enter the context, `visible()` returns the registry unchanged |
| `mk http serve` | unchanged; static bearer token, all collections |
| `content-source.yaml` with no `auth:` | unchanged everywhere; `mk mcp serve-http` serves every collection to any caller and says so in its banner |
| an `auth:` block with no `anonymous:` rule | unchanged: 401 for a token-less request, challenge included; the new counter reads 0 and the access log gains no field (#36) |
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
- **Anonymous access** (#36, `internal/authz/anonymous_test.go`,
  `internal/authn/anonymous_test.go`, `internal/mcp/anonymous_test.go`):
  the config-validation matrix (selector exclusivity per selector, one
  refusal per write capability, the `allow_unauthenticated` and
  no-`providers` combinations); the gate matrix (no header / valid /
  expired / forged / wrong-audience / wrong-issuer / subjectless /
  malformed / empty-`Bearer` / non-Bearer scheme × published or not);
  the invisibility oracles re-run for the anonymous caller, including
  "hidden and absent differ by nothing but the name the caller typed";
  union semantics for authenticated callers; anonymous memory refusal at
  both the tool filter and `memory.Authorize`; and a
  zero-behaviour-change test for a policy that writes no anonymous rule.

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
