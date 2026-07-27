# OpenWebUI integration

Wire `meerkat` into [OpenWebUI](https://docs.openwebui.com/) so any
chat session can search the knowledge base. Same three tools as the OpenCode
integration, exposed over HTTP/JSON instead of MCP/stdio.

## Run the server

Default bind is loopback-only (`127.0.0.1`) — safe when OpenWebUI
runs on the same host as meerkat:

```bash
export MEERKAT_API_KEY=$(openssl rand -hex 32)
mk http serve --port 4004
```

Required:
- `--api-key <key>` flag **or** `MEERKAT_API_KEY` env (env wins if
  both are set, with a startup warning). Prefer the env var — a
  value passed via `--api-key` is visible to other local users via
  `ps`.
- The server refuses to start with no key configured. There is no
  anonymous mode.

Optional:
- `--host` defaults to `127.0.0.1`.
- `--port` defaults to `4004`.

If OpenWebUI runs elsewhere, read the next section before reaching
for `--host 0.0.0.0`.

## OpenWebUI on another host

meerkat has no TLS of its own (`ListenAndServe`, never
`ListenAndServeTLS`), so reaching it from another host needs TLS
from somewhere else in the path.

### Reverse proxy (recommended)

Keep meerkat bound to loopback; terminate TLS at a proxy in front of
it.

`nginx` example:

```nginx
server {
  listen 443 ssl http2;
  server_name meerkat.internal.example.com;
  ssl_certificate     /etc/letsencrypt/live/meerkat.internal.example.com/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/meerkat.internal.example.com/privkey.pem;

  # OpenWebUI sends a long-running search; loosen the read timeout.
  proxy_read_timeout 60s;

  location / {
    proxy_pass http://127.0.0.1:4004;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
  }
}
```

`systemd` unit:

```ini
[Unit]
Description=meerkat http server
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/meerkat/env        # contains MEERKAT_API_KEY=...
ExecStart=/usr/local/bin/meerkat http serve --host 127.0.0.1 --port 4004
Restart=on-failure
RestartSec=5
DynamicUser=yes
ProtectSystem=strict
ProtectHome=yes
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
```

### Direct exposure (`--host 0.0.0.0`)

Only for networks you'd actually trust with an unencrypted bearer
token — that's rare enough that the reverse proxy above should be
the default choice. Without TLS somewhere in the path, the
`Authorization: Bearer` header and every response body cross the
network in plaintext to anyone positioned to observe it:

```bash
export MEERKAT_API_KEY=$(openssl rand -hex 32)
mk http serve --host 0.0.0.0 --port 4004
```

## Register with OpenWebUI

1. Open OpenWebUI → Admin Settings → Tools → "Add Tool Server"
2. URL: `http://<meerkat-host>:4004/openapi.json` — or, behind a
   reverse proxy, the proxy's own `https://` URL
3. Auth: Bearer Token, paste your `MEERKAT_API_KEY`
4. Save.

OpenWebUI fetches the OpenAPI 3.1 schema, registers three
operations (`mk_search`, `mk_show`, `mk_list`), and they become
available to any chat that has the tool enabled.

## Endpoint surface

| Method | Path | Body | Auth | Returns |
|--------|------|------|------|---------|
| POST | `/search` | `{query, limit?}` | Bearer | `[{id, title, score, snippet, category, status}]` |
| POST | `/show` | `{id}` | Bearer | `{id, title, body, front}` |
| POST | `/list` | `{prefix?, category?, status?, owner?}` | Bearer | `[{id, title, category, status, owner, source}]` |
| GET | `/openapi.json` | — | none | OpenAPI 3.1 schema |
| GET | `/healthz` | — | none | `{"status":"ok"}` |
| GET | `/` | — | none | plain-text help banner |

Surface mirrors MCP 1:1 so an agent that knows one knows the other.

## Smoke test

```bash
KEY=$MEERKAT_API_KEY
HOST=http://127.0.0.1:4004

# health (no auth)
curl -sS $HOST/healthz

# search (auth required)
curl -sS -X POST $HOST/search \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"query":"rate limiting","limit":3}' | jq '.[].id'

# list — filter to placeholders
curl -sS -X POST $HOST/list \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"category":"concepts","status":"placeholder"}' | jq

# show one page
curl -sS -X POST $HOST/show \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"id":"systems/backend/rate-limiter"}' | jq -r .body | head -30
```

## Auth model

- One static bearer token per server instance.
- Verified with `subtle.ConstantTimeCompare` (no early-exit timing
  side-channel).
- Rotate by restarting the process with a new value.
- For multi-user / per-tool revocation, sit behind an authenticating
  reverse proxy (oauth2-proxy, Pomerium, etc.) instead of trying to
  layer it inside meerkat.

## OpenAPI schema notes

- The schema declares a `bearerAuth` security scheme so OpenWebUI's
  tool-registration UI prompts for the token.
- `/healthz` and `/openapi.json` carry no security requirement so
  liveness probes and tool registration work without the token.
- All three data endpoints respond with documented 4xx codes:
  - `400` — body invalid / required field missing
  - `401` — bearer header missing or wrong
  - `404` — page not found (only `/show`)

## See also

- `docs/SEARCH.md` — what `mk_search` actually does under the hood
- `docs/INTEGRATION-OPENCODE.md` — same tools over MCP/stdio
- `docs/INSTALL.md` — install + verify checksums + cosign
- `docs/SECURITY.md` — full threat model
