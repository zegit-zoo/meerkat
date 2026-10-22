# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# Hardened, multi-arch OCI image for meerkat (see docs/CONTAINER.md for
# how to run it — read-only-fs flags, the cache-dir mount, etc).
#
# Two stages for the meerkat image (the builder is split into a shared
# `source` stage and the compile; `--target operator` builds the
# operator image, see below):
#   builder — golang:1.26.10-bookworm, matching go.mod's toolchain
#             (`toolchain go1.26.10`) exactly, so the binary this image
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

ARG GO_VERSION=1.26.10

# --platform=$BUILDPLATFORM pins the builder to the *host* (runner)
# architecture regardless of the target platform buildx is currently
# producing: Go cross-compiles natively via GOOS/GOARCH (see the `go
# build` RUN below), so this avoids running the compiler itself under
# QEMU emulation for the non-native arch — only the trivial final-stage
# COPY needs to happen per target platform, not the compile.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS source
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

# Each binary compiles in its own stage from this one, so building the
# operator image does not compile meerkat, and the reverse.
FROM source AS builder
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

# meerkat-operator (meerkat-mob#10, phase 2): `docker build --target
# operator` builds ghcr.io/zegit-zoo/meerkat-operator from the same
# sources and pins. These two stages sit before `final` on purpose: a
# build with no --target builds the last stage, and that must stay the
# meerkat image. BuildKit skips both stages unless --target operator
# asks for them.
FROM source AS operator-builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# The operator links client-go and controller-runtime, so its license
# file lists its own module set, not meerkat's.
RUN mkdir -p /out && go run gen-third-party-licenses.go -pkg ./cmd/meerkat-operator -out /out/THIRD-PARTY-LICENSES.txt
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/meerkat-operator ./cmd/meerkat-operator

# No $HOME, cache or config: the operator writes nothing to disk except
# the webhook serving certificate, which is mounted read-only.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS operator

COPY --from=operator-builder --chown=65532:65532 /out/meerkat-operator /usr/local/bin/meerkat-operator
COPY --from=operator-builder --chown=65532:65532 /src/LICENSE /LICENSE
COPY --from=operator-builder --chown=65532:65532 /src/NOTICE /NOTICE
COPY --from=operator-builder --chown=65532:65532 /out/THIRD-PARTY-LICENSES.txt /THIRD-PARTY-LICENSES.txt

USER 65532:65532

LABEL org.opencontainers.image.source="https://github.com/zegit-zoo/meerkat" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="meerkat-operator" \
      org.opencontainers.image.description="Kubernetes operator for meerkat mob: KnowledgeBase and MeerkatMob validation and reconciliation."

ENTRYPOINT ["/usr/local/bin/meerkat-operator"]

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
