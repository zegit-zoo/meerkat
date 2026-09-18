#!/usr/bin/env bash
# minio-up.sh — start a single-node MinIO in Docker for the S3
# conformance tests, create a bucket, and print the environment the
# tests read. Companion of garage-up.sh; same contract.
#
#   eval "$(scripts/minio-up.sh)"          # start (idempotent) and export
#   scripts/minio-up.sh down               # stop and remove
set -euo pipefail

NAME="${MINIO_CONTAINER:-meerkat-minio}"
IMAGE="${MINIO_IMAGE:-quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z}"
PORT="${MINIO_PORT:-9000}"
BUCKET="${MINIO_BUCKET:-meerkat-conformance}"
ACCESS_KEY="${MINIO_ACCESS_KEY:-meerkat-ci}"
SECRET_KEY="${MINIO_SECRET_KEY:-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')}"

if [[ "${1:-}" == "down" ]]; then
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  exit 0
fi

if docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  SECRET_KEY="$(docker inspect "$NAME" --format '{{range .Config.Env}}{{println .}}{{end}}' | awk -F= '/^MINIO_ROOT_PASSWORD=/ {print $2}')"
else
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:9000" \
    -e MINIO_ROOT_USER="$ACCESS_KEY" -e MINIO_ROOT_PASSWORD="$SECRET_KEY" \
    "$IMAGE" server /data >/dev/null
  for _ in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:${PORT}/minio/health/live" >/dev/null; then break; fi
    sleep 0.5
  done
  # Create the bucket with the bundled mc.
  docker exec "$NAME" sh -c "mc alias set local http://127.0.0.1:9000 '$ACCESS_KEY' '$SECRET_KEY' >/dev/null && mc mb --ignore-existing local/$BUCKET >/dev/null"
fi

cat <<ENV
export MEERKAT_TEST_S3_ENDPOINT="http://127.0.0.1:${PORT}"
export MEERKAT_TEST_S3_BUCKET="${BUCKET}"
export MEERKAT_TEST_S3_REGION="us-east-1"
export MEERKAT_TEST_S3_PATH_STYLE="true"
export MEERKAT_TEST_S3_PROVIDER="minio"
export AWS_ACCESS_KEY_ID="${ACCESS_KEY}"
export AWS_SECRET_ACCESS_KEY="${SECRET_KEY}"
ENV
