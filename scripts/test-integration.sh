#!/usr/bin/env bash
# Runs the concurrency tests against a throwaway Postgres in Docker.
# The container is removed afterwards, even if the tests fail.
set -euo pipefail
cd "$(dirname "$0")/.."

NAME="seatlock-test-pg-$$"
PORT="${TEST_PG_PORT:-55433}"
docker run -d --rm --name "$NAME" -p "$PORT:5432" \
  -e POSTGRES_USER=seatlock -e POSTGRES_PASSWORD=seatlock -e POSTGRES_DB=seatlock \
  postgres:16-alpine >/dev/null
trap 'docker stop "$NAME" >/dev/null' EXIT

for _ in $(seq 1 30); do
  docker exec "$NAME" pg_isready -U seatlock -d seatlock >/dev/null 2>&1 && break
  sleep 1
done

TEST_DATABASE_URL="postgres://seatlock:seatlock@localhost:$PORT/seatlock?sslmode=disable" \
  go test -race -count=1 -v ./internal/integration/ "$@"
