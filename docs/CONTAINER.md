# Container image

Every `vX.Y.Z` release tag publishes a hardened, multi-arch OCI image to
[`ghcr.io/zegit-zoo/meerkat`](https://github.com/zegit-zoo/meerkat/pkgs/container/meerkat)
alongside the binary release described in [docs/INSTALL.md](INSTALL.md).
See [.github/workflows/release.yml](../.github/workflows/release.yml) and
the root [`Dockerfile`](../Dockerfile) for exactly how it's built.

**Git tags and image tags are not spelled the same way.** The git tag is
`vX.Y.Z`, but the published image tags drop the leading `v` — a
`v1.2.3` git tag publishes the image aliases `1.2.3`, `1.2`, and
`latest`. Requesting the `v`-prefixed form as an image tag (e.g. tag
`v1.2.3` on `ghcr.io/zegit-zoo/meerkat`) does not exist and pulling it
fails with `MANIFEST_UNKNOWN`.

Jump to:

- [Pulling the image](#pulling-the-image)
- [Running it](#running-it)
- [Read-only root filesystem](#read-only-root-filesystem)
- [Persisting the content cache](#persisting-the-content-cache)
- [Verifying the image (cosign)](#verifying-the-image-cosign)
- [SBOM and provenance](#sbom-and-provenance)
- [Kubernetes / Cloud Run notes](#kubernetes--cloud-run-notes)

## Pulling the image

```bash
docker pull ghcr.io/zegit-zoo/meerkat:1.2.3   # pin the exact patch release
docker pull ghcr.io/zegit-zoo/meerkat:1.2     # pin the minor line, auto-follows patches
docker pull ghcr.io/zegit-zoo/meerkat:latest  # newest tagged release
```

Both `linux/amd64` and `linux/arm64` manifests are published under the
same tag; Docker/Kubernetes pick the right one for the host automatically.

## Running it

The image's `ENTRYPOINT` is the `meerkat` binary itself; pass the same
subcommands/flags you'd pass on the CLI (see [docs/CLI.md](CLI.md)). With
no arguments it prints `--help` and exits 0 — a quick way to sanity-check
a pull.

```bash
# HTTP/OpenAPI server (for OpenWebUI) — see docs/INTEGRATION-OPENWEBUI.md
docker run --rm -p 4004:4004 \
  -e MEERKAT_API_KEY=change-me \
  ghcr.io/zegit-zoo/meerkat:1.2.3 \
  http serve --host 0.0.0.0

# MCP server over stdio (for agent harnesses) — see docs/INTEGRATION-OPENCODE.md
docker run --rm -i \
  ghcr.io/zegit-zoo/meerkat:1.2.3 \
  mcp serve
```

`--host 0.0.0.0` is required for the HTTP server: its default,
`127.0.0.1`, is unreachable from outside the container's network
namespace. The default port is `4004` (`--port` to change it).

## Read-only root filesystem

The image runs as the non-root, numeric user `65532:65532` (`gcr.io/
distroless/static-debian12:nonroot`) and — served from the **default
embedded content** (no `content-source.yaml` / `--content-source`
configured) — never writes to disk at all. That makes `--read-only` safe
to use unconditionally, not just permitted:

```bash
docker run --rm --read-only --user 65532:65532 \
  -p 4004:4004 -e MEERKAT_API_KEY=change-me \
  ghcr.io/zegit-zoo/meerkat:1.2.3 \
  http serve --host 0.0.0.0
```

(`--user 65532:65532` is redundant with the image's own `USER`
directive — included above for defense in depth / clusters that reset
it, and because Kubernetes' `runAsNonRoot: true` + `runAsUser: 65532`
pod security context fields express the same thing declaratively; see
[Kubernetes / Cloud Run notes](#kubernetes--cloud-run-notes).)

## Persisting the content cache

The one case that **does** need a writable path: pointing the running
container at external content via `--content-source`/
`MEERKAT_CONTENT_SOURCE` with `content.type: url` (or discovery of a
`type: local`/`url` `content-source.yaml` under `$XDG_CONFIG_HOME`). See
the README's ["Serving content at
runtime"](../README.md#serving-content-at-runtime) for the full
resolution order and `type: url`'s cache semantics.

The image sets `HOME=/home/nonroot`, `XDG_CACHE_HOME=/home/nonroot/.cache`,
and `XDG_CONFIG_HOME=/home/nonroot/.config`, and ships that directory
pre-created and owned by UID/GID `65532`. Mount a volume there to
persist the fetched-content cache across container restarts (otherwise
every restart re-fetches and re-verifies the archive — correct, just
slower, since `type: url` content is immutable-by-digest and does no
extra work beyond that on a cache hit):

```bash
docker run --rm --read-only --user 65532:65532 \
  -v meerkat-cache:/home/nonroot/.cache \
  -v ./content-source.yaml:/home/nonroot/.config/content-source.yaml:ro \
  -e MEERKAT_CONTENT_SOURCE=/home/nonroot/.config/content-source.yaml \
  -e MEERKAT_API_KEY=change-me \
  -p 4004:4004 \
  ghcr.io/zegit-zoo/meerkat:1.2.3 \
  http serve --host 0.0.0.0
```

`--read-only` is still in effect here: `/home/nonroot/.cache` is the
*only* writable path, supplied as an explicit named volume (or a
`tmpfs` mount, if persistence across restarts doesn't matter and you'd
rather avoid a volume entirely — swap `-v meerkat-cache:...` for
`--tmpfs /home/nonroot/.cache`).

`type: local` (a bind-mounted directory) needs no cache mount at all —
only a read-only bind mount of the content directory itself, plus the
`content-source.yaml` pointing at it.

## Verifying the image (cosign)

Images are signed keylessly at release time via GitHub OIDC — the same
Sigstore posture `docs/RELEASE.md` documents for the binary checksums
file, applied to the image manifest instead of a blob:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/zegit-zoo/meerkat:1.2.3
```

(The `--certificate-identity-regexp` still matches `refs/tags/v1.2.3` —
that's the *source* git ref the workflow was triggered from, baked into
the OIDC identity. The image reference on the last line is the
*published* tag, which — per the note above — has no `v`.)

A successful `cosign verify` confirms the image manifest was produced by
the `release.yml` workflow in this repository at the exact tagged ref,
and that the signature's inclusion in the public Rekor transparency log
was checked (`cosign verify` queries Rekor directly for images, unlike
`verify-blob --bundle`'s offline inclusion-proof check for the binary
release).

## SBOM and provenance

Each pushed image carries two BuildKit attestations, generated at build
time by `docker buildx build --sbom=true --provenance=true` (see
`.github/workflows/release.yml`):

- an SPDX SBOM (BuildKit's default SBOM scanner,
  [`docker/buildkit-syft-scanner`](https://github.com/docker/buildkit-syft-scanner),
  wraps Anchore's [Syft](https://github.com/anchore/syft) — the same tool
  the binary release's SBOMs use, per `docs/SECURITY.md`)
- a SLSA provenance predicate describing the build (source repo, commit,
  workflow, builder identity)

Inspect either without pulling the full image:

```bash
docker buildx imagetools inspect ghcr.io/zegit-zoo/meerkat:1.2.3 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/zegit-zoo/meerkat:1.2.3 --format '{{ json .Provenance }}'
```

## Kubernetes / Cloud Run notes

A pod security context matching the guarantees above:

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
```

On Cloud Run, the equivalent is the default (Cloud Run always runs
containers as non-root and offers no writable filesystem beyond `/tmp`
unless an in-memory volume is configured) — point `--content-source`'s
cache dir at `/tmp` via `XDG_CACHE_HOME=/tmp/meerkat-cache` if `type:
url` content is in use; the default embedded-content path needs no
change at all.
