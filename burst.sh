#!/usr/bin/env bash
# One-command on-sale stampede against a SeatLock deployment.
#
#   ./burst.sh https://seatlock-production-4dd4.up.railway.app
#   ./burst.sh http://localhost:8080 -requests 5000 -concurrency 200
#
# Any extra arguments are passed to cmd/burst (run with -h to list them).
# Uses a local Go toolchain if there is one, otherwise runs inside Docker.
# Exits non-zero if any correctness check fails.
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: ./burst.sh <BASE_URL> [burst flags]" >&2
  exit 2
fi
BASE_URL="$1"
shift
cd "$(dirname "$0")"

if command -v go >/dev/null 2>&1; then
  exec go run ./cmd/burst -base "$BASE_URL" "$@"
fi

# No Go: run in the builder image. Inside the container, "localhost" is the
# container itself, so point it at the host instead.
DOCKER_URL="${BASE_URL/localhost/host.docker.internal}"
DOCKER_URL="${DOCKER_URL/127.0.0.1/host.docker.internal}"
exec docker run --rm -v "$PWD":/src -w /src \
  --add-host=host.docker.internal:host-gateway \
  -e ADMIN_KEY="${ADMIN_KEY:-}" \
  golang:1.24.13-alpine go run ./cmd/burst -base "$DOCKER_URL" "$@"
