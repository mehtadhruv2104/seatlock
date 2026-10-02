# SeatLock — Design Log

Running record of major design decisions, in the order they were made. This is the
working doc; WRITEUP.md (the graded deliverable) distills the final version of this
into the required sections.

## Stack

- **Go + Gin** — HTTP API. Fast cold start matters on a free-tier deploy.
- **PostgreSQL only** — no Redis, no cache. The spec explicitly says a single
  database is fine and encouraged, and the entire correctness challenge (no
  double-sell, per-user limit, idempotency) can be pushed into one ACID
  transaction with row locks. A second dependency (Redis) would only add a
  second thing the readiness probe has to track and a second way a cold start
  can fail, for no correctness benefit at single-instance scale.
- **No frontend.** Spec: "We do NOT grade the UI. A JSON API is enough." All
  time goes into correctness, observability, deploy reliability, the burst
  script, and the write-up.
- **Deploy**: Railway (Docker container + managed Postgres).

## Reservation model: direct confirm, not hold-then-confirm

The spec allows either an explicit-cancel model or a time-boxed auto-expiring
hold. The spec's own example response for `POST /reserve` returns
`"status": "confirmed"` **synchronously** — there is no separate
payment/confirm endpoint defined anywhere in the required API surface.

Decision: `POST /reserve` atomically confirms the seat(s) in one step.
Release happens only via explicit `POST /reservations/{id}/cancel` by the
owner. This is simpler (no background expiry sweeper needed), matches the
given example exactly, and is fully spec-compliant since `held` remains a
valid seat status in the data model — it's just not reachable in the current
flow. (Left as a clean extension point: a future "hold during checkout" step
would only need to add a TTL column and a sweeper; the schema doesn't change.)

## Multi-seat requests: all-or-nothing

If a user requests `["A12","A13"]` and only one is free, the whole request is
declined (409, reason `seat_taken`) rather than partially fulfilled. Rationale:
"give me 2 seats together" is the real intent behind a multi-seat request;
silently handing back a booking for fewer seats than asked for is a worse
default than a clean decline the client can retry differently. Enforced by
rolling back the whole transaction if any requested seat isn't won.

## The atomic decision — mechanism

One Postgres transaction per `/reserve` call. Locks are acquired in a fixed
global order across *every* transaction, which makes deadlock structurally
impossible rather than something to detect-and-retry around:

1. **Claim the idempotency key.**
   `INSERT INTO reservations (..., user_id, idempotency_key, seats, amount_paise=0, status='confirmed') ON CONFLICT (user_id, idempotency_key) DO NOTHING RETURNING id`.
   - If this transaction wins the insert, it proceeds to step 2 holding that
     row (uncommitted) as its idempotency claim.
   - If it loses (0 rows returned), a prior *committed* attempt with the same
     key exists (Postgres makes the second `INSERT` block on the unique index
     until the first transaction resolves, so there's no window where a
     reader sees "no row" while a concurrent writer is mid-flight — it either
     sees it after commit, or the conflict vanishes after rollback and the
     second insert just succeeds). Fetch that row:
     - same seat set (order-independent) → return it as a **replay** (200,
       not 201 — see Idempotency below).
     - different seat set → **409** `idempotent_replay_conflict`.
   - If the first attempt's transaction *rolled back* (e.g. it lost the seat
     race), its claim row never existed from any other transaction's point of
     view, so the key is free and this attempt proceeds fresh. This is what
     lets a legitimately-failed attempt be retried with the same key.

2. **Lock the per-(show,user) counter row.**
   `INSERT INTO user_show_counters (show_id, user_id, held_count) VALUES (...) ON CONFLICT DO NOTHING`,
   then `SELECT held_count FROM user_show_counters WHERE show_id=$1 AND user_id=$2 FOR UPDATE`.
   This row always exists after the first touch, so locking it closes the
   phantom-read gap that a plain `COUNT(*) ... FOR UPDATE` over seat rows
   would leave open (there's nothing to lock for seats that don't exist yet).
   If `held_count + requested > per_user_limit` → rollback → 409
   `per_user_limit`.

3. **Lock the requested seats, in sorted order.**
   `SELECT id, status FROM seats WHERE show_id=$1 AND seat_label = ANY($labels) ORDER BY seat_label FOR UPDATE`.
   Every transaction sorts its seat labels before locking, so two
   transactions racing over an overlapping seat set (e.g. A wants
   `[A12,A13]`, B wants `[A13,A14]`) always attempt to acquire locks in the
   same relative order. Classic resource-ordering argument: a cycle requires
   some pair of transactions to lock the same two resources in opposite
   order, which this rules out by construction. This is also why the seat
   lock step always comes *after* the counter-row lock in every transaction —
   consistent relative ordering between lock types matters as much as within
   one type.

4. If all requested seats came back `available` → flip them to `confirmed`,
   set `amount_paise = price_paise * n`, link them to the reservation id,
   bump the counter, commit → **201**.
   If any requested seat is missing or not `available` → rollback the whole
   transaction (all-or-nothing) → **409** `seat_taken`.

A plain read-then-write ("is A12 free? ok take it") is exactly what this
avoids — the `FOR UPDATE` in step 3 is the single point where "is it free"
and "take it" become one atomic step, and the sorted order is what makes that
safe for multi-seat requests.

## Idempotency details

- Key is scoped to `(user_id, idempotency_key)` — identity is token-derived,
  so no spoofing risk, and it means two different users can coincidentally
  reuse the same literal key string without colliding.
- Stored directly on the `reservations` row (no separate idempotency table) —
  there's no state to track outside a single DB transaction, since
  Postgres's MVCC guarantees no other transaction ever observes a
  partially-built reservation row.
- Same key + same seat set → replay, returns the original reservation,
  **HTTP 200** (vs 201 for a genuinely new reservation) so a client can tell
  "this call created it" from "this call matched an earlier one" without
  parsing the body.
- Same key + different seat set → **409**, reason `idempotent_replay_conflict`.
- A failed attempt (seat-taken / over-limit) does **not** consume the key —
  the whole transaction rolls back, including the claim insert — so the
  caller can retry the same key and it will attempt fresh.

## Identity / auth (deliberate simplification)

No user signup/login system — out of scope for what's being graded, and the
spec's own interest is specifically "identity is token-derived, not
body-derived," not a full auth system. `Authorization: Bearer <user_id>` is
treated as an opaque user identifier (the token *is* the user id). Admin
(`POST /shows`) uses a separate `X-Admin-Key` header checked against an env
var, to keep the two schemes visually and structurally distinct. Documented
honestly in WRITEUP.md as a scoped-out simplification, not hidden.

## Schema

- `shows(id, name, price_paise, per_user_limit, created_at)`
- `seats(id, show_id, seat_label, status, held_by, confirmed_reservation_id, UNIQUE(show_id, seat_label))`
- `reservations(id, show_id, user_id, idempotency_key, seats TEXT[], amount_paise, status, created_at, cancelled_at, UNIQUE(user_id, idempotency_key))`
- `user_show_counters(show_id, user_id, held_count, PRIMARY KEY(show_id, user_id))`

`reservations.seats` is deliberately denormalized (text array, not a join
table) — it's what lets the idempotency replay check compare "requested
seats == original seats" with a single row fetch, and `seats.confirmed_reservation_id`
is enough to answer per-seat state for `GET /shows/{id}` without needing to
join back through a seats-of-reservation table.

## Cancel

`POST /reservations/{id}/cancel`: locks the reservation row `FOR UPDATE`
first (prevents double-cancel races), checks `user_id` matches the caller
and `status='confirmed'`, then releases its seats (`status='available'`,
clear `held_by`/`confirmed_reservation_id`) and decrements the counter — all
in one transaction. Because a seat only transitions *into* `confirmed` by
being claimed exclusively by one reservation, there's no other transaction
that could be racing to touch those specific seats at the same time, so no
extra seat-level locking dance is needed beyond locking the reservation row
itself.

## Observability

- Prometheus metrics at `/metrics`: `seatlock_reservations_confirmed_total`,
  `seatlock_reservations_declined_total{reason}`, `seatlock_seats_available{show_id}` gauge.
  The gauge is both updated incrementally on each mutation and swept
  periodically (every few seconds) from an authoritative `COUNT` query per
  show, so it self-heals and doubles as a live reconciliation signal.
- Structured JSON logs (zerolog) with a per-request correlation id
  (generated or taken from `X-Request-Id`), echoed back in the response
  header and included in every log line for that request.
- `GET /healthz` — liveness, always 200 if the process is up.
- `GET /readyz` — readiness, pings the DB with a short timeout, fails closed
  (503) if unreachable.

## Open items / next

- Burst script: Go program under `cmd/burst`, invoked via `./burst.sh <BASE_URL>`.
- Dockerfile + docker-compose for local Postgres.
- WRITEUP.md to be filled in as each section lands.
