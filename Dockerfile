# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# Hardened, multi-arch OCI image for meerkat (see docs/CONTAINER.md for
# how to run it — read-only-fs flags, the cache-dir mount, etc).
#
# Two stages:
#   builder — golang:1.26.5-bookworm, matching go.mod's toolchain
#             (`toolchain go1.26.5`) exactly, so the binary this image
#             ships is built with the same compiler `make build`/CI use.
#   final   — gcr.io/distroless/static-debian12:nonroot: no shell, no
#             package manager, ca-certificates already present, and
#             already configured to run as the numeric non-root
#             "nonroot" user (65532:65532). USER is restated below
#             anyway so the non-root requirement is visible in this
#             file, not just inherited from the base image tag.
#
# Both base images are pinned by digest, not just tag — the same
# posture this repo uses for GitHub Actions (pinned to a commit SHA,
# not a floating tag) in .github/workflows/*.yml. `docker buildx build
# --platform linux/amd64,linux/arm64` resolves the right per-arch
# manifest from each digest's index automatically.
#
# TARGETOS/TARGETARCH are populated automatically by buildx; see
# .github/workflows/release.yml.

ARG GO_VERSION=1.26.5

# --platform=$BUILDPLATFORM pins the builder to the *host* (runner)
# architecture regardless of the target platform buildx is currently
# producing: Go cross-compiles natively via GOOS/GOARCH (see the `go
# build` RUN below), so this avoids running the compiler itself under
# QEMU emulation for the non-native arch — only the trivial final-stage
# COPY needs to happen per target platform, not the compile.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS builder
WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Same before-hooks .goreleaser.yaml runs at release time:
#   - populate the embed dirs from content-source.yaml (absent in this
#     repo => type: none => the committed empty placeholders — no
#     network access needed to build this image);
#   - regenerate THIRD-PARTY-LICENSES.txt from the modules actually
#     linked into ./cmd/meerkat (offline; see gen-third-party-licenses.go).
RUN go run ./internal/contentsync
RUN go run gen-third-party-licenses.go

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ARG KB_COMMIT=unknown

# Same flags as the Makefile's LDFLAGS / .goreleaser.yaml's builds.meerkat
# entry — static binary (CGO_ENABLED=0), stripped, version metadata baked
# in via -X.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/zegit-zoo/meerkat/internal/cli.version=${VERSION} \
        -X github.com/zegit-zoo/meerkat/internal/cli.commit=${COMMIT} \
        -X github.com/zegit-zoo/meerkat/internal/cli.date=${DATE} \
        -X github.com/zegit-zoo/meerkat/internal/cli.kbCommit=${KB_COMMIT}" \
      -o /out/meerkat ./cmd/meerkat

# The final stage has no shell to mkdir/chown with, so the nonroot
# user's writable $HOME is assembled here and copied over wholesale.
# It's only ever touched at runtime by --content-source/
# MEERKAT_CONTENT_SOURCE pointing at a `type: url` source (the on-disk
# fetch cache — see internal/contentsource/url.go) or a `type: local`/
# discovered content-source.yaml under $XDG_CONFIG_HOME; the default
# embedded-content path never writes to it, which is what lets the
# image run with a fully read-only root filesystem out of the box.
RUN mkdir -p /home/nonroot/.cache /home/nonroot/.config \
    && chown -R 65532:65532 /home/nonroot

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS final

COPY --from=builder --chown=65532:65532 /out/meerkat /usr/local/bin/meerkat
COPY --from=builder --chown=65532:65532 /src/LICENSE /LICENSE
COPY --from=builder --chown=65532:65532 /src/NOTICE /NOTICE
COPY --from=builder --chown=65532:65532 /src/THIRD-PARTY-LICENSES.txt /THIRD-PARTY-LICENSES.txt
COPY --from=builder --chown=65532:65532 /home/nonroot /home/nonroot

USER 65532:65532
ENV HOME=/home/nonroot
ENV XDG_CACHE_HOME=/home/nonroot/.cache
ENV XDG_CONFIG_HOME=/home/nonroot/.config
WORKDIR /home/nonroot

LABEL org.opencontainers.image.source="https://github.com/zegit-zoo/meerkat" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="meerkat" \
      org.opencontainers.image.description="Knowledge-base CLI/MCP/HTTP server, compiled into a single static binary."

ENTRYPOINT ["/usr/local/bin/meerkat"]
CMD ["--help"]
