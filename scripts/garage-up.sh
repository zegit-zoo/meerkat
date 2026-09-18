#!/usr/bin/env bash
# garage-up.sh — start a single-node Garage in Docker for the S3
# conformance tests, create a bucket and a key, and print the
# environment the tests read.
#
#   eval "$(scripts/garage-up.sh)"          # start (idempotent) and export
#   scripts/garage-up.sh down               # stop and remove
#
# Used by CI (.github/workflows/ci.yml, job "Object store conformance")
# and by anyone running `go test ./... -run S3Conformance` locally. The
# credentials are ephemeral: they exist only inside this container.
set -euo pipefail

NAME="${GARAGE_CONTAINER:-meerkat-garage}"
IMAGE="${GARAGE_IMAGE:-dxflrs/garage:v2.4.1}"
PORT="${GARAGE_PORT:-3900}"
BUCKET="${GARAGE_BUCKET:-meerkat-conformance}"
KEY="${GARAGE_KEY:-meerkat-ci}"

if [[ "${1:-}" == "down" ]]; then
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  exit 0
fi

g() { docker exec -e RUST_LOG=error "$NAME" /garage "$@"; }

if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  cfg="$(mktemp -d)/garage.toml"
  cat >"$cfg" <<TOML
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
root_domain = ".s3.garage.localhost"

[admin]
api_bind_addr = "[::]:3903"
admin_token = "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
TOML
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:3900" \
    -v "$cfg:/etc/garage.toml:ro" "$IMAGE" >/dev/null

  # Wait for the node, then lay out one zone with one node.
  for _ in $(seq 1 60); do
    if g status >/dev/null 2>&1; then break; fi
    sleep 0.5
  done
  node_id="$(g status 2>/dev/null | awk '/^[0-9a-f]{16}/ {print $1; exit}')"
  if [[ -z "$node_id" ]]; then
    echo "garage-up: could not read the node id from 'garage status'" >&2
    g status >&2 || true
    exit 1
  fi
  g layout assign -z dc1 -c 1G "$node_id" >/dev/null
  g layout apply --version 1 >/dev/null
  g bucket create "$BUCKET" >/dev/null
  g key create "$KEY" >/dev/null
  g bucket allow --read --write --owner "$BUCKET" --key "$KEY" >/dev/null
fi

info="$(g key info "$KEY" --show-secret)"
access_key="$(printf '%s\n' "$info" | awk -F': *' '/^Key ID/ {print $2; exit}')"
secret_key="$(printf '%s\n' "$info" | awk -F': *' '/^Secret key/ {print $2; exit}')"
if [[ -z "$access_key" || -z "$secret_key" ]]; then
  echo "garage-up: could not parse 'garage key info' output:" >&2
  printf '%s\n' "$info" >&2
  exit 1
fi

cat <<ENV
export MEERKAT_TEST_S3_ENDPOINT="http://127.0.0.1:${PORT}"
export MEERKAT_TEST_S3_BUCKET="${BUCKET}"
export MEERKAT_TEST_S3_REGION="garage"
export MEERKAT_TEST_S3_PATH_STYLE="true"
export MEERKAT_TEST_S3_PROVIDER="garage"
export AWS_ACCESS_KEY_ID="${access_key}"
export AWS_SECRET_ACCESS_KEY="${secret_key}"
ENV
