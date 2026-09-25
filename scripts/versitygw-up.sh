#!/usr/bin/env bash
# versitygw-up.sh — start a single-node Versity Gateway (posix backend)
# in Docker for the S3 conformance tests, create a bucket, and print the
# environment the tests read. Companion of garage-up.sh; same contract.
#
# It replaced scripts/minio-up.sh on 2026-09-25: MinIO archived its
# community edition and withdrew its public container images (Docker Hub
# on 2026-09-11, quay.io answering 401 since), so the matrix lost its
# only provider that ENFORCES conditional writes — versitygw v1.8.0 does
# (atomic per-key conditional PUT on the posix backend, and read
# preconditions), is Apache-2.0 and is actively released.
#
#   eval "$(scripts/versitygw-up.sh)"      # start (idempotent) and export
#   scripts/versitygw-up.sh down           # stop and remove
#
# Used by CI (.github/workflows/ci.yml, job "Object store conformance")
# and by anyone running `go test ./... -run S3Conformance` locally. The
# credentials are ephemeral: they exist only inside this container.
set -euo pipefail

NAME="${VERSITYGW_CONTAINER:-meerkat-versitygw}"
IMAGE="${VERSITYGW_IMAGE:-ghcr.io/versity/versitygw:v1.8.0@sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499}"
PORT="${VERSITYGW_PORT:-7070}"
BUCKET="${VERSITYGW_BUCKET:-meerkat-conformance}"
ACCESS_KEY="${VERSITYGW_ACCESS_KEY:-meerkat-ci}"
SECRET_KEY="${VERSITYGW_SECRET_KEY:-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')}"

if [[ "${1:-}" == "down" ]]; then
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  exit 0
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  env_of() { docker inspect "$NAME" --format '{{range .Config.Env}}{{println .}}{{end}}' | awk -F= -v k="^$1=" '$0 ~ k {print $2}'; }
  ACCESS_KEY="$(env_of ROOT_ACCESS_KEY)"
  SECRET_KEY="$(env_of ROOT_SECRET_KEY)"
else
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # posix backend: the data directory must exist and be writable. A
  # tmpfs keeps the conformance objects off the host and discards them
  # with the container.
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:7070" --tmpfs /data \
    -e ROOT_ACCESS_KEY="$ACCESS_KEY" -e ROOT_SECRET_KEY="$SECRET_KEY" \
    "$IMAGE" posix /data >/dev/null

  ready=""
  for _ in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 0.5
  done
  if [[ -z "$ready" ]]; then
    echo "versitygw-up: no answer from GET http://127.0.0.1:${PORT}/health after 30s" >&2
    docker logs "$NAME" >&2 || true
    exit 1
  fi

  # versitygw bundles no bucket CLI; create it with the SDK in go.mod.
  # stdout of this script is eval'd, so the helper's output goes to stderr.
  (
    cd "$repo_root"
    AWS_ACCESS_KEY_ID="$ACCESS_KEY" AWS_SECRET_ACCESS_KEY="$SECRET_KEY" \
      go run scripts/s3mkbucket.go \
      -endpoint "http://127.0.0.1:${PORT}" -bucket "$BUCKET"
  ) >&2
fi

cat <<ENV
export MEERKAT_TEST_S3_ENDPOINT="http://127.0.0.1:${PORT}"
export MEERKAT_TEST_S3_BUCKET="${BUCKET}"
export MEERKAT_TEST_S3_REGION="us-east-1"
export MEERKAT_TEST_S3_PATH_STYLE="true"
export MEERKAT_TEST_S3_PROVIDER="versitygw"
export AWS_ACCESS_KEY_ID="${ACCESS_KEY}"
export AWS_SECRET_ACCESS_KEY="${SECRET_KEY}"
ENV
