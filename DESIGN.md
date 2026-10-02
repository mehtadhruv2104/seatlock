# SeatLock — Design Log

Running record of major design decisions and the reasoning behind them. This is
the working doc; WRITEUP.md (the graded deliverable) distills the final version.

**How this doc is maintained.** Sections 1–12 are the baseline design agreed
before any code was written. They are not rewritten later. When implementation
diverges from them, or a decision changes, a dated entry is appended to the
**Change log** at the end, saying what changed, why, and which section it
supersedes. The doc is a guide, not a contract: the change log records how the
design actually evolved while building.

---

## 1. Stack

- **Go + Gin** for the HTTP API. Fast cold start, cheap concurrency, simple to containerize.
- **PostgreSQL** as the single source of truth. The spec says a single database is
  fine and encouraged, and all correctness guarantees (no double-sell, per-user
  limit, idempotency) come from one ACID transaction with row locks.
- **No Redis in the core design.** It isn't needed for correctness. It may come
  back as a performance option (see §10), only if burst tests show we need it.
- **No frontend.** The spec says the UI isn't graded.
- **Deploy:** Railway (Docker container + managed Postgres).

---

## 2. Reservation model: direct confirm + explicit cancel

`POST /shows/{id}/reserve` atomically moves seats from `available` to `confirmed`
in one step and returns `status: "confirmed"`, matching the spec's example
response. Seats are released only by the owner calling
`POST /reservations/{id}/cancel`.

**Considered and rejected: hold + TTL + confirm/pay endpoint.** This is how real
ticketing sites work, because real payment happens between choosing a seat and
the purchase being final. But this spec has no payment step: "never double-charge
a retried request" refers to idempotent retries, not to payment. The hold model
would add a TTL column, a sweeper, an invented confirm endpoint, lazy reclaim and
more lock-ordering cases, with no benefit against the spec. `held` remains a
valid seat status in the schema (the spec lists three states), but the current
flow never produces it.

---

## 3. Multi-seat requests: all-or-nothing

If a user asks for `["A12","A13"]` and either seat isn't available, the whole
request is declined (409 `seat_taken`). Nothing is partially granted.

Rationale: a multi-seat request usually means "seats together"; silently granting
fewer seats than asked for is worse than a clean decline the client can retry.

Consequence worth knowing: a user can lose a seat nobody else wanted. If Alice
asks for `[A12, A13]` and Bob wins A13, Alice gets neither, even though she locked
A12 first. Her lock on A12 was never visible to anyone else (uncommitted), so
nothing was "taken away" from her: her bundle simply failed.

---

## 4. The atomic decision

One Postgres transaction per `/reserve` call, at the default READ COMMITTED
isolation level. Locks are always taken in the same order:

**reservation claim → per-user lock → seat rows (sorted by label)**

1. **Claim the idempotency key.**
   `INSERT INTO reservations (...) ON CONFLICT (user_id, idempotency_key) DO NOTHING RETURNING id`.
   - Insert succeeds: we own this attempt, continue.
   - Conflict: a committed reservation with this key already exists. (If another
     transaction with the same key is still running, Postgres makes our insert
     wait on the unique index until that transaction commits or rolls back, so
     we never see a half-finished attempt.) Handle as described in §5.

2. **Take the per-user lock and check the limit.**
   `INSERT INTO user_show_locks (show_id, user_id) ... ON CONFLICT DO NOTHING`, then
   `SELECT ... FROM user_show_locks WHERE show_id=$1 AND user_id=$2 FOR UPDATE`.
   This row stores **no count**; it exists only as a lock, so all requests from one
   user for one show run one at a time. After taking it, count the user's
   confirmed seats directly:
   `SELECT count(*) FROM seats WHERE show_id=$1 AND user_id=$2 AND status='confirmed'`.
   Because each statement in READ COMMITTED sees the latest committed data, this
   count always includes the user's earlier committed reservations.
   If `count + requested > per_user_limit`, roll back and return 409 `per_user_limit`.

   *Why a lock row rather than just `COUNT(*) ... FOR UPDATE` on seats:* locking
   the user's existing seat rows doesn't stop two parallel requests from the same
   user grabbing *different* available seats at the same time, because those seats
   aren't the user's yet and so aren't locked. Both would see the old count and
   both would pass. The lock row makes them run one after the other.

   *Why no stored counter:* a stored `held_count` has to be decremented correctly
   on every release path. Counting from the seats table is always accurate, with
   nothing to drift.

3. **Lock the requested seats in sorted order.**
   `SELECT id, status FROM seats WHERE show_id=$1 AND seat_label = ANY($labels) ORDER BY seat_label FOR UPDATE`.
   This is the point where "is it free?" and "take it" become one atomic step.

4. **Decide.** If every requested seat exists and is `available`: update them to
   `confirmed` with `user_id` and `reservation_id`, set
   `amount_paise = price_paise × n` on the reservation, commit, return **201**.
   Otherwise roll back everything (including the claim from step 1) and return
   **409 `seat_taken`**.

   **Safeguard (defense in depth):** the confirming UPDATE is itself
   state-guarded and its row count is checked:
   ```sql
   UPDATE seats SET status='confirmed', user_id=$u, reservation_id=$r
   WHERE id = ANY($locked_ids) AND status='available';
   -- rows affected must equal the number of seats requested, else rollback + 409
   ```
   With the locks from step 3 this can never fail. It exists so that if the
   locking logic ever has a bug, the result is a 409 rather than a double-sell.

**Why this can't deadlock.** A deadlock needs a cycle: transaction A holds
something B wants while B holds something A wants. Every transaction takes locks
in the same global order (reservation → user lock → seats, and seats in label
order), so any two transactions competing for two resources always try for them
in the same order, and a cycle can't form. Without the `ORDER BY`, Alice
(A12 then A13) and someone else (A13 then A12) could deadlock; Postgres would
abort one with `deadlock_detected`, which would surface as a 500.

**Hot-seat storm (500 users, one seat).** All 500 queue on the same row lock
inside Postgres. The first to commit wins; each of the others then acquires the
lock, sees `confirmed`, rolls back and gets a 409. Postgres's lock wait queue is
already a queue; we don't build one.

**Fairness.** The winner is whoever acquires the database lock first, which
approximates "first click" but isn't identical (network jitter, pool wait,
scheduling). No high-throughput system guarantees strict first-come-first-served
without serializing everything. The spec requires correctness, not FCFS.

**Transactions stay short.** No network calls or other slow work inside the
transaction, so locks are held for milliseconds.

---

## 4a. Decision: pessimistic locking, with an optimistic safeguard

**Pessimistic** (`SELECT ... FOR UPDATE`): conflicts are detected before acting;
the loser waits for the lock, then sees the new state.
**Optimistic** (version/state check on write): no lock while reading; the write
includes `WHERE version=$v` (or `status='available'`), and 0 rows affected means
someone else got there first.

| | Pessimistic | Optimistic |
|---|---|---|
| Loser experiences | Waits, then sees new state | Fails immediately; gives up or retries |
| Cost when conflicts are rare | Lock overhead not needed | Almost free |
| Cost when conflicts are frequent | Waiters hold DB connections while queued | Wasted work; retry storms if losers retry |
| Rules spanning several rows | Natural (lock a row that represents the rule) | Awkward (shared version row + retries) |
| Deadlock risk | Yes, prevented by consistent lock order | None from reads (writes still lock) |
| Retry logic needed | No | Usually, which is where bugs and 5xx come from |

**Where optimistic would work here: claiming a single seat.** Optimistic locking
usually hurts under contention because losers must retry (e.g. a balance update).
A lost seat is final: retrying can never succeed. So a state-guarded
`UPDATE ... WHERE status='available'` needs no retry loop; 0 rows → 409. The
spec explicitly accepts this ("a conditional update guarded on current state").

**Where it breaks down:**
1. **Multi-seat requests.** A multi-row conditional UPDATE locks rows in scan
   order, so overlapping requests can deadlock (surfacing as 500). Making it safe
   requires sorted locking, i.e. `ORDER BY ... FOR UPDATE`: pessimistic again.
2. **Per-user limit.** The rule spans many rows. The optimistic version needs a
   version on the per-user row, and here a conflict is *not* final: the loser might
   still be within the limit after recounting. So it needs a bounded retry loop and
   a plan for when retries run out, which is the complexity and 5xx risk
   pessimistic locking avoids.

**Postgres specifics.** Even a conditional UPDATE waits if another transaction is
mid-write on the same row, then rechecks its `WHERE` against the committed value.
So under a hot-seat storm the two approaches behave more alike than the textbook
suggests; the real difference is whether a lock is held across the
read → decide → write gap. Postgres's fully optimistic mode is SERIALIZABLE
isolation: no locks, conflicting transactions abort with a serialization
failure and the app retries. Under a 500-way storm almost every transaction would
abort and retry, which is the wrong tool for this workload.

**Decision:** pessimistic locking as described in §4, because the per-user limit
and multi-seat ordering need it and it requires no retry logic. Plus the
optimistic state-guarded UPDATE in step 4 as a safeguard: one extra clause that
turns any future locking bug into a 409 instead of a double-sell.

---

## 5. Idempotency

- Key comes from the request body (`idempotency_key`), and is also accepted from an
  `Idempotency-Key` header. Scoped to `(user_id, idempotency_key)`: identity comes
  from the token, and two users can use the same key string without colliding.
- Stored on the `reservations` row itself, enforced by
  `UNIQUE(user_id, idempotency_key)`. No separate table or cache.
- **Same key, same seats** (order-independent) → return the original reservation,
  **HTTP 200** (201 is reserved for the call that actually created it).
- **Same key, different seats** → **409 `idempotent_replay_conflict`**.
- **Failed attempt** (seat taken, over limit) → the whole transaction rolls back,
  including the claim, so the key is not used up and a retry with it starts fresh.
- Same key for a reservation that was later cancelled → return it as is
  (`status: "cancelled"`). A new attempt needs a new key.

---

## 6. Cancel

`POST /reservations/{id}/cancel`, owner only. One transaction, same lock order:

1. `SELECT ... FROM reservations WHERE id=$1 FOR UPDATE`. Not found → 404. Caller
   isn't the owner → 404 (don't reveal that someone else's reservation exists).
   Already cancelled → return it as is (200; cancelling twice is harmless).
2. Lock its seats (sorted) and set them to `available`, clearing `user_id` and
   `reservation_id`. Only seats still pointing at this reservation are touched,
   so a cancel can never release a seat that belongs to someone else.
3. Mark the reservation `cancelled`, commit.

No counter to update: the per-user count is computed from seats (§4).

---

## 7. Identity / auth (deliberate simplification)

- Users: `Authorization: Bearer <user_id>`. The token *is* the user id. Any
  `user_id` in a request body is ignored. A real system would verify a signed
  token; that's out of scope here and documented as such.
- Admin (`POST /shows`): `X-Admin-Key` header checked against an env var. Kept
  separate from user auth so the two can't be confused.

---

## 8. Schema

- `shows(id, name, price_paise BIGINT, per_user_limit INT DEFAULT 4, created_at)`
- `reservations(id, show_id, user_id, idempotency_key, seats TEXT[], amount_paise BIGINT, status, created_at, cancelled_at, UNIQUE(user_id, idempotency_key))`
- `seats(id, show_id, seat_label, status, user_id, reservation_id, UNIQUE(show_id, seat_label))`
- `user_show_locks(show_id, user_id, PRIMARY KEY(show_id, user_id))`

`reservations.seats` is denormalized (text array) so the idempotency check
can compare seat sets with one row fetch. Money is always integer paise.

---

## 9. API responses

| Case | Status |
|---|---|
| New reservation | 201 |
| Idempotent replay | 200 |
| Seat taken / over limit / key reused with different seats | 409 + `reason` |
| Bad input (empty seats, duplicates, unknown seat label, missing key) | 400 |
| Missing/invalid token | 401 |
| Show or reservation not found (or not yours) | 404 |

**Principle: be as user-responsive as possible.** Every non-2xx response tells
the client exactly what went wrong *and* what it can do next, using data we
already have inside the transaction. Every body has a machine-readable `reason`
(matches the metric label), a human-readable `message`, and context fields:

| `reason` | Extra fields | Example `message` |
|---|---|---|
| `seat_taken` | `unavailable_seats: ["A13"]` | "Seat A13 is no longer available. No seats were reserved." |
| `per_user_limit` | `limit: 4, already_reserved: 2, remaining: 2` | "You can reserve 2 more seats for this show (limit 4)." |
| `idempotent_replay_conflict` | `reservation_id`, `original_seats: ["A12"]` | "This idempotency key was already used for seats [A12]. Use a new key for a different request." |
| `validation_error` (400) | `field`, e.g. `unknown_seats: ["Z99"]` | "Seat Z99 does not exist in this show." |

The extra fields come from rows we've already locked or counted, so they're
accurate at the moment of the decision and cost no extra queries.

---

## 10. Observability

- **Metrics** (`/metrics`, Prometheus):
  - `seatlock_reservations_confirmed_total` (counter)
  - `seatlock_reservations_declined_total{reason}` (counter)
  - `seatlock_seats_available{show_id}` (gauge), plus held/confirmed gauges,
    refreshed from an authoritative `COUNT` query every few seconds so they always
    reconcile with the API.
  - HTTP request count/latency by route and status.
- **Logs:** structured JSON (zerolog) with a request id (taken from `X-Request-Id`
  or generated), returned in the response header and included in every log line.
  Each reserve logs its outcome and reason.
- **Health:** `GET /healthz` (process alive) and `GET /readyz` (pings the DB with a
  short timeout, 503 if unreachable).

---

## 11. Performance improvements — deferred until after burst tests

None of these affect correctness. Each one is decided after running the burst
script against the live deploy, based on what the results actually show.

| # | Improvement | What it prevents | Cost / risk | Build it if burst tests show… |
|---|---|---|---|---|
| P1 | **Limit every wait:** deadline on getting a pool connection (~1–2s) and Postgres `lock_timeout` (a few seconds). When either expires, return a clean 4xx. | Requests waiting until the client or Railway's proxy times out, which shows up as 502/504 (counted as 5xx). | Small. Needs the 409-vs-429 decision below. | Any 5xx, or p99 latency approaching the proxy timeout. |
| P2 | **Pre-check before the transaction:** plain read of the requested seats; if any is already `confirmed`, return 409 with no transaction. Not authoritative: the transaction still decides. | Thousands of requests for already-sold seats each paying for a full transaction. | Very small. | Pool wait time or transaction rate dominated by requests that lose. |
| P3 | **Sold-seat set:** remember confirmed seats so repeat requests get 409 without touching the database. **In memory** only works with a single instance. **In Redis**, it's shared by all instances. Cancel must remove the seat from the set. If Redis is down we fall back to the database (it must not fail readiness, because it isn't needed for correctness). | Database/pool load from the huge "seat already sold" tail at 100:1+ contention. | In memory: small, but single-instance only. Redis: a second dependency to deploy and monitor. | P2 isn't enough, or we scale to more than one instance. |
| P4 | **HTTP server timeouts** (read, write, idle). | Slow or idle clients holding connections open indefinitely. | Trivial. | Practically always worth it; confirm values with the burst. |
| P5 | **Pool sizing** against Postgres `max_connections`, leaving headroom (migrations, admin, readiness). | Exhausting Postgres connections, or a pool too small to use the database. | Tuning only. | Tuned from measured throughput and the plan's connection limit. |

**Open questions tied to these:**
- Response code when we shed load without deciding (P1 expires): **409** when we
  know the seat is gone, **429 + `Retry-After`** when we couldn't decide in time.
  Risk: the spec expects losers of a hot-seat storm to get 409; a strict grader
  might count 429 as wrong even though it's a 4xx. Decide after seeing how often
  it happens.
- Whether a lock timeout gets its own decline reason (`contended_timeout`) in
  metrics. Leaning yes: it separates "lost the race" from "system too busy to answer".

---

## 12. Out of scope (considered)

- **Virtual waiting room** (Queue-it / Cloudflare Waiting Room style). This is
  admission control at the edge: requests are let through at a fixed rate with a
  signed admission token. It protects capacity and improves fairness, but doesn't
  decide who gets which seat, so the transaction in §4 is still needed behind it.
  It would sit in front of this API without changing it.
- **Hold + TTL checkout** (see §2).
- **Real authentication** (see §7).

---

## Change log

Append-only. Format: date, what changed, why, which section it supersedes.

- **2026-10-02 — Baseline.** Sections 1–12 agreed after the design discussion,
  before implementation started.
