# SeatLock

A seat-reservation service built to stay consistent and available when thousands of buyers hit
"book" in the same second: a seat is never sold twice, the per-user limit
holds under parallel requests, and a retried request never books twice.

**Live:** https://seatlock-production-4dd4.up.railway.app

| | |
|---|---|
| Liveness (and the deployed commit) | `GET /healthz` |
| Readiness (checks Postgres, 503 if unreachable) | `GET /readyz` |
| Prometheus metrics (public) | `GET /metrics` |
| API reference with curl for every endpoint | [`swagger.yaml`](swagger.yaml) |
| Design decisions and how they evolved | [`DESIGN.md`](DESIGN.md) |
| Write-up | [`WRITEUP.md`](WRITEUP.md) |
| Load test results (20k at once, 100k hot seats, 100k free seats) | [`loadTest.md`](loadTest.md) |

Stack: Go, Gin, PostgreSQL (the only datastore), Docker, Railway.

---

## Try it

```sh
BASE=https://seatlock-production-4dd4.up.railway.app

# Create a show (admin)
curl -s -X POST $BASE/shows -H 'X-Admin-Key: admin' \
  -d '{"name":"friday-night","seats":["A1","A2","A12","A13"],"price_paise":25000}'
SHOW_ID=...   # "id" from the response

# Reserve (user identity comes from the token, never the body)
KEY=$(uuidgen)
curl -s -X POST $BASE/shows/$SHOW_ID/reserve -H 'Authorization: Bearer alice' \
  -d "{\"seats\":[\"A12\",\"A13\"],\"idempotency_key\":\"$KEY\"}"     # 201

# Retry with the same key → 200, the same reservation, nothing new booked
curl -s -X POST $BASE/shows/$SHOW_ID/reserve -H 'Authorization: Bearer alice' \
  -d "{\"seats\":[\"A13\",\"A12\"],\"idempotency_key\":\"$KEY\"}"     # 200

# Seat state and counts (available + held + confirmed == total, always)
curl -s $BASE/shows/$SHOW_ID

# Cancel (owner only; anyone else gets 404)
curl -s -X POST $BASE/reservations/$RESERVATION_ID/cancel -H 'Authorization: Bearer alice'
```

### What Users need to know

- **User auth:** `Authorization: Bearer <user_token>`. To make testing easy, the
  token *is* the user id: any 1–128 character string with no spaces is a
  distinct user. A `user_id` in the request body is ignored. (Production would
  use signed tokens; see DESIGN.md §13 E1.)
- **Admin key** (for `POST /shows`): `X-Admin-Key: admin`.
- **Idempotency keys must be UUIDs.** Send one per booking in the body
  (`idempotency_key`) or the `Idempotency-Key` header, and reuse it only to
  retry that same booking. Keys are per user.
- **Multi-seat requests are all-or-nothing.**
- **Every error says what happened and what to do next:** a `reason` code, a
  `message`, and context fields (for example `per_user_limit` includes
  `limit`, `already_reserved` and `remaining`; `seat_taken` includes
  `unavailable_seats`).

| Outcome | Status | `reason` |
|---|---|---|
| Reserved | 201 | |
| Retry of a successful key | 200 | (the original reservation) |
| Seat already taken | 409 | `seat_taken` |
| Over the per-user limit | 409 | `per_user_limit` |
| Same key, different seats or show | 409 | `idempotent_replay_conflict` |
| Bad input (unknown or duplicate seats, key not a UUID, …) | 400 | `validation_error` |
| Body ended early / body too slow | 400 / 408 | `incomplete_body` / `request_timeout` |
| Missing or bad token / admin key | 401 | `unauthorized` |
| Unknown show / reservation | 404 | `show_not_found` / `reservation_not_found` |

---

## One-command burst

```sh
./burst.sh https://seatlock-production-4dd4.up.railway.app
# or
make burst BASE_URL=https://seatlock-production-4dd4.up.railway.app
```

It uses your Go toolchain if you have one and otherwise runs inside the
`golang` Docker image, so Docker alone is enough. It exits non-zero if any
check fails.

**What it does** (all numbers are flags; run `go run ./cmd/burst -h`):

1. Creates a fresh show (1,000 seats, limit 4) and fires **20,000 reservations,
   500 in flight**, all starting at the same instant:
   - **hot-seat storm:** 25% of requests fight over 5 seats;
   - **general buyers** booking 1–2 random seats;
   - **retries** with the same key (same seats, and some with different seats);
   - a **greedy user** firing 10 parallel requests on a limit-4 show;
   - **spoofers** sending another user's id in the body.
2. Samples `GET /shows/{id}` every 500ms during the burst and checks the
   invariant each time.
3. Probes idempotency deterministically, cancels 20 reservations, and races 5
   buyers for each released seat.
4. Prints the outcome distribution (confirmed / declined by reason / 5xx),
   latency, and PASS/FAIL per check, then reconciles `/metrics` deltas with what
   it observed.

Like a careful client, it retries a request **once, with the same idempotency
key**, if no response arrives (or on a 5xx). A booking that had committed but
whose response was lost then comes back as a 200 replay instead of being
booked twice. 4xx is never retried; any 5xx still fails the run.

Excerpt from a live run (20k requests, 500 in flight):

```
Stampede outcomes
  409 seat_taken                         19113   95.6%
  201 confirmed                            774    3.9%
  200 idempotent replay                     70    0.3%
  409 idempotent_replay_conflict            36    0.2%
  409 per_user_limit                         7    0.0%

  20000 requests in 12.9s (1552 req/s)   latency p50 277ms  p95 412ms  p99 1.329s

Correctness
  PASS  zero 5xx
  PASS  no seat confirmed twice
  PASS  hot seat S1: exactly one winner  (1 reservations among 1000 requests)
  ...
  PASS  per-user limit: greedy user's 10 parallel requests for free seats got exactly 4
  PASS  identity from token, not body (spoofed user_id ignored)
  PASS  invariant held during the burst
  PASS  invariant after the burst: available + held + confirmed == total
Metrics reconciliation (/metrics deltas vs this run)
  PASS  reservations_confirmed_total ... PASS  safeguard_trips_total  (delta 0)
RESULT: PASS
```

Client-side latency from far away is mostly network: the server-side p99 for
that run was ≤100ms (from `seatlock_http_request_duration_seconds`). Measured
results for larger runs, up to 100k requests, are in DESIGN.md's change log.

**Running it from one machine:** the default is HTTP/2, which multiplexes many
requests over a few connections. `-http1` opens one connection per in-flight
request. Thousands of simultaneous requests from a single laptop exhaust the
client's own ports and connections long before they load the service; see
DESIGN.md.

---

**From the cloud:** one laptop can't open thousands of simultaneous requests,
so [`Dockerfile.burst`](Dockerfile.burst) runs the tool as a one-off job (for
example a Railway service with restart policy "never", ideally in the same
region). Configure it with `BASE_URL`, `ADMIN_KEY` and
`BURST_ARGS="-requests 20000 -concurrency 20000"`; the result is in the job's
logs. Run that way, **20,000 reservations all at once passed every check**, with
zero 5xx and no lost responses (see DESIGN.md).

## Run locally

```sh
docker compose up --build        # API on http://localhost:8080 (admin key: dev-admin-key)
./burst.sh http://localhost:8080 -admin-key dev-admin-key
```

The same Dockerfile is what Railway builds. Migrations are embedded in the
binary and applied on startup, so a fresh database needs no separate step.

## Tests

```sh
make test               # unit tests
make test-integration   # concurrency tests against a throwaway Postgres (Docker), with -race
```

The integration tests cover the spec's correctness bar against a real Postgres:
- a 500-way hot-seat storm (exactly 1 winner);
- overlapping multi-seat requests (no deadlocks);
- 10 parallel requests from one user on a limit-4 show (exactly 4);
- same-key races (one booking);
- same key with different seats (conflict);
- cancel ownership and rebooking;
- cancels racing reserves;
- a spoofed `user_id` over HTTP.

They were verified by breaking the code on purpose (see DESIGN.md): with the row
lock removed, 28–32 users each got the same seat.

---

## Observability

**Metrics** (`/metrics`, Prometheus format). Labels have small, fixed value sets:
there are no per-show or per-user labels.

| Metric | Meaning |
|---|---|
| `seatlock_reservations_confirmed_total`, `seatlock_seats_confirmed_total` | 201s and the seats they booked |
| `seatlock_reservations_declined_total{reason}` | `seat_taken`, `per_user_limit`, `idempotent_replay_conflict`, `idempotent_replay` (a 200 retry that booked nothing new) |
| `seatlock_reservations_cancelled_total`, `seatlock_seats_released_total` | Cancellations |
| `seatlock_seats_{available,held,confirmed,total}` | Read from the database on every scrape, so they always match it |
| `seatlock_safeguard_trips_total` | Must stay 0. Any increase means a locking bug |
| `seatlock_http_requests_total`, `seatlock_http_request_duration_seconds` | By method, route template and status |
| `seatlock_db_pool_*` | Connections in use, idle, max, and waits for a free connection |

**Logs:** one structured JSON line per request with `request_id` (taken from
`X-Request-Id` if you send one, and always returned in that response header),
`route`, `status`, `latency_ms`, `user_id`, `show_id`, `reservation_id` and the
decline `reason`.
- **Sampling:** Railway drops log lines above 500/s per replica, so 1 in 100
  hot-seat `seat_taken` lines is logged (marked `"sampled": true`).
- **Summaries:** a `reserve summary` line every second counts every outcome.
  Those counts match `/metrics` exactly.
- **Viewing them:** Railway has no public log link. A real sample exported after
  a live burst is in [`docs/sample-logs.jsonl`](docs/sample-logs.jsonl). With
  project access: `railway logs -s seatlock --json`.

---

## Deploy (Railway)

- A Railway service built from this repo (Railway uses the Dockerfile) plus a
  Railway Postgres, **in the same region**. App ↔ database latency sits inside
  the row-lock critical section.
- Health check path: `/readyz`.
- Variables:

| Variable | Required | Default | |
|---|---|---|---|
| `DATABASE_URL` | yes | | `${{Postgres.DATABASE_URL}}` |
| `ADMIN_KEY` | yes | | The app refuses to start without it |
| `PORT` | no | 8080 | Set by Railway |
| `DB_MAX_CONNS` | no | 32 | Connection-pool size; fixed rather than CPU-derived |
| `LOG_SAMPLE_SEAT_TAKEN` | no | 100 | Log 1 in N hot-seat decline lines (1 = all) |

## Layout

```
main.go                     wiring, HTTP server timeouts, graceful shutdown
internal/router             routes and middleware
internal/handlers           request parsing, validation, status mapping
internal/service            the reservation logic (the atomic decision lives here)
internal/db                 SQL, transactions, embedded goose migrations
internal/middleware         auth, request ids, access logs, body limit
internal/metrics            Prometheus metrics
internal/integration        concurrency tests against real Postgres
cmd/burst                   the burst tool
```
