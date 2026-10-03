# SeatLock: write-up

The full decision log, including what changed while building and why, is in
[DESIGN.md](DESIGN.md). This is the short version.

## 1. The atomic decision

Every `POST /shows/{id}/reserve` is **one Postgres transaction at READ
COMMITTED**, and it always takes its locks in the same order:

1. **Claim the idempotency key.**
   `INSERT INTO reservations (...) ON CONFLICT (user_id, idempotency_key) DO NOTHING`.
   If a request with the same key is still in flight, this insert waits on the
   unique index until that request commits or rolls back. So a key can never
   produce two bookings.
2. **Lock a per-(show, user) row** (`SELECT ... FOR UPDATE`), then count the
   user's confirmed seats. All requests from one user for one show now run one
   at a time, which closes the gap where two parallel requests both see
   "3 of 4 used". The row stores no count: counting from `seats` can't drift.
   This relies on READ COMMITTED, where each statement sees everything committed
   before it started. Under REPEATABLE READ the count would come from an older
   snapshot, and the limit would break.
3. **Lock the requested seats, sorted by label**:
   `SELECT ... WHERE seat_label = ANY($1) ORDER BY seat_label FOR UPDATE`.
   This is where "is it free?" and "take it" become one atomic step. If any
   seat isn't available, the whole transaction rolls back and returns 409
   `seat_taken` (all-or-nothing).
4. **Confirm with a state-guarded UPDATE** (`... AND status = 'available'`) and
   check that the number of rows updated equals the number requested. With the
   locks above this can never fail. It's a safeguard: if the locking ever had a
   bug, the result would be a 409, not a double-sell. A trip increments
   `seatlock_safeguard_trips_total`.

**Why it's race-free:** the decision for each seat is made while holding that
seat's row lock, and the confirming write re-checks the state. **Why
multi-seat requests can't deadlock:** a deadlock needs two transactions to lock
the same two rows in opposite orders. Every transaction locks seats in label
order, and always takes the key claim → user lock → seats in that order, so no
cycle can form. I checked with `EXPLAIN` that Postgres sorts before it locks
(`LockRows` sits above `Sort`).

A database CHECK constraint makes "a seat has an owner exactly when it isn't
available" impossible to violate from any code path.

I chose **pessimistic locking**. A conditional `UPDATE ... WHERE
status='available'` alone would be fine for a single seat: losing a seat is
final, so no retry loop is needed. But the per-user limit spans many rows, and
multi-seat requests need a deterministic lock order. That makes optimistic
locking either retry-heavy or back to `FOR UPDATE`. The comparison is in
DESIGN.md §4a.

**Pre-check (added after measuring).** In a storm, about 95% of requests lose.
Before the transaction, the same single read that loads the show also returns
the requested seats' status and whether this key was already used. A fresh
request for an already-sold seat gets a 409 with no transaction, lock or write.
It can only decline, never grant, so the transaction remains the authority.
Live, this halved the pool acquires, cut the share of acquires that had to wait
for a connection from 72% to 14%, and brought server-side p50 from ≤50ms to
≤5ms.

**Verified:**
- Integration tests against a real Postgres, with `-race`, cover a 500-way
  hot-seat storm, overlapping multi-seat requests, the per-user limit, same-key
  races, cancel and rebook, and a spoofed `user_id`.
- I broke the code on purpose to check that these tests catch it. Without the
  per-user lock, one user got 7 seats on a limit of 4. With a read-then-write
  seat decision, 28–32 users each got a 201 for the same seat.
- Live, every 20k-request burst shows exactly one winner per hot seat.

## 2. Idempotency

- **Where the key lives:** on the reservation row itself, as
  `UNIQUE(user_id, idempotency_key)`. There's no separate table or cache. Keys
  must be UUIDs and are scoped per user, so identical keys from two users never
  collide.
- **Exactly once:** the key claim is step 1 of the same transaction that books
  the seats, so the claim and the booking commit or roll back together. A
  concurrent duplicate waits on the unique index, then sees the committed
  original.
- **Same key, same seats** (in any order): **200** with the original
  reservation. That's 201 vs 200, so a client can tell "this call booked it"
  from "this call matched an earlier one". This includes a reservation that was
  later cancelled.
- **Same key, different seats or a different show:** **409
  `idempotent_replay_conflict`**, with the original reservation's id and seats.
- **A failed attempt doesn't use up the key:** a decline rolls back the claim,
  so a retry with the same key is a fresh attempt.

This mattered for real. At 2,000 in flight, Railway's edge reset some client
connections, and some of the lost responses were for bookings that **had
committed**. A client that retries with the same key gets those back as 200
replays instead of booking twice. The burst tool does exactly that.

## 3. Holds and expiry

I chose **direct confirm with an explicit, owner-only cancel**, not a
time-boxed hold. I considered the hold model (reserve → hold → pay within N
minutes → confirm, or the hold expires). It's what real ticketing sites do,
because a payment happens in between. But this spec has no payment step: its
example response returns `confirmed` synchronously. And "never double-charge a
retried request" refers to idempotency, not payment. A hold model would have
added a TTL, a sweeper, an invented confirm endpoint, lazy reclaim of expired
holds, and more lock-ordering cases, for no benefit against the spec.

Cancel locks the reservation, then its seats in label order (the same order as
reserve, so the two can't deadlock). It releases only seats still pointing at
that reservation, so it can never free someone else's seat. Released seats are
immediately rebookable: in a live race of 5 buyers per released seat, exactly
one won each time. Cancelling twice returns the cancelled reservation;
someone else's reservation returns 404, so its existence isn't revealed.

## 4. Consistency vs availability under a partition

**I chose consistency.** Postgres is the only source of truth, and the app never
decides a seat from memory or a cache. If the app can't reach the database,
`/readyz` fails closed (503 within 2s) and reserves fail rather than guess. No
seat is ever granted without the database agreeing. That's deliberate: for
selling unique items, an outage is recoverable, while a double-sale isn't.

Where it could still go wrong, and what I'd do:
- **Failover to an asynchronous replica** could lose the last few committed
  bookings. For zero loss, use synchronous replication, at the cost of commit
  latency.
- **Lost responses during a network failure** are covered by idempotency: retry
  with the same key.
- **Region placement is part of correctness, not just performance.** For a
  while the app ran in Singapore with Postgres in Virginia. A reserve took ~3.1s
  because row locks were held across trans-Pacific round trips, which would have
  turned every hot-seat queue into a crawl. They now run in the same region.

## 5. Observability: what would page me at 2am

**Page immediately:**
- `seatlock_safeguard_trips_total` > 0. A locking bug has stopped a double-sell;
  the next one might not be stopped.
- Any sustained rate of 5xx in `seatlock_http_requests_total{status=~"5.."}`.
- `/readyz` failing or `seatlock_seat_gauges_up == 0`: the database is
  unreachable.
- `seatlock_seats_available + held + confirmed != seats_total`. It's impossible
  by construction, so if it ever fires, something is badly wrong.

**Ticket, not page:**
- Server-side reserve p99 above about 1s.
- `seatlock_db_pool_acquire_wait_seconds_total` climbing (the pool is
  saturated: revisit the pool size or the pre-check).
- A spike in `idempotent_replay_conflict` (a client reusing keys wrongly).
- Railway's "Messages dropped" log warnings.

**Not a page:** `seat_taken` spikes. During an on-sale, thousands of declines a
second is the system working.

Every request has a `request_id`, echoed in `X-Request-Id`, so a single request
can be traced through the logs. Metrics have no per-show or per-user labels
(unbounded cardinality), and per-show detail comes from `GET /shows/{id}` and
the logs. Two things I only learned by running it live:
- Railway drops log lines above 500/s per replica. So 1 in 10 hot-seat decline
  lines is logged, and a per-second summary counts every outcome; those counts
  matched `/metrics` exactly.
- A bug: bodies that arrived too slowly were reported as "invalid JSON". The
  access logs gave it away: `latency_ms` ≈ 15000, exactly the read timeout. They
  are now 408 `request_timeout` and 400 `incomplete_body`.

**Measured live** (Railway, 32-connection pool, client in India):
- 20k requests at 500 in flight and 100k requests at 500 in flight both passed
  every check with zero 5xx; server-side p99 ≤100ms and ≤50ms respectively.
- At 2,000 in flight, Railway's edge reset some client connections (0.6% of
  responses lost), while the server answered in ≤0.5s and correctness held.
- The most expensive case, 100k requests for mostly free seats, ran at about
  1,600 req/s with a server-side p99 ≤2.5s.

## 6. AI usage: directed vs decided

> **Draft from the build session. Review and correct it so it's accurate in
> your own words before submitting.**

I used Claude (Claude Code) throughout, as a pair. It wrote most of the code
and docs; I directed the design and made the calls. The decision log in
DESIGN.md records who decided what as it happened.

**I set the approach and constraints:**
- I wanted a design discussion before any code. Early on, the AI started
  scaffolding after I'd asked only for its thoughts. I stopped it, deleted the
  scaffold, and we rebuilt task by task.
- Other ground rules from me: small commits per task; DESIGN.md as an
  append-only decision log; the handler → service → db layering; goose for
  migrations; error responses that tell the client what to do next.

**Where I pushed and the design changed:**
- I proposed a 5-minute hold for payment. The AI worked through the hold, TTL
  and confirm design (it was never built). I then realised "double-charge" in the spec meant idempotent
  retries, not payment, and dropped it as over-engineering.
- I asked whether the system was unfair to a buyer who loses a multi-seat
  bundle, and whether a queue was needed at a 10:1 contention ratio. That led
  to the resource-protection items (pre-check, pool sizing, timeouts) being
  scoped, deferred, and then decided from live measurements.
- I asked how a 100k-request burst would behave, and later the 20k-at-once
  scenario. That drove the 100k live runs, the edge-reset investigation, and
  the client-side retry in the burst tool.
- Choices I made from the options offered:
  - UUID idempotency keys; the token named `user_token`, with signed tokens
    documented rather than built;
  - per-show metrics (with pruning) documented as the production design rather
    than built;
  - HTTP/2 by default in the burst tool;
  - not building wait deadlines or a cache, and documenting why;
  - 1-in-10 log sampling.
- I chose the deploy setup and moved both services to the same region.

**Where the AI proposed and I accepted:**
- Dropping Redis and the frontend from my initial stack.
- The lock order (key claim → per-user lock → sorted seats) and the
  state-guarded safeguard.
- Replacing a stored per-user counter with a lock-only row. This was its own
  self-correction, after it noticed a stored count would drift once holds
  could expire.
- The pre-check, measured before and after.
- Breaking the code on purpose to prove the tests catch bugs.

**What the AI found and fixed while operating it:**
- The live service was pinned to an old commit.
- The app and database were in different regions.
- Railway's log rate limit.
- The misclassified read-timeout errors.
- Flawed checks in its own burst tool: a vacuous greedy-user check and an
  order-dependent conflict check.

**Where the AI was wrong:**
- It started coding before I asked.
- It recommended `railway.json`, then found it deprecated and reversed itself.
- It made a commit that didn't compile on its own, fixed in the next commit.
- It first blamed the hot-seat storm for latency, which turned out to be the
  network path.

## 7. What I'd do next

1. **Signed user tokens** (DESIGN.md §13 E1): HMAC or JWT with an expiry and key
   rotation, instead of token = user id.
2. **Admission control:** cap in-flight reserves and answer beyond the cap with a
   fast 429 + `Retry-After`, instead of queueing until timeouts. It only matters
   far beyond the loads tested here.
3. **Per-show metrics with bounded cardinality** (E2), once shows have an
   on-sale window and an end time.
4. **Load testing from datacenter clients** (several machines near the region),
   so the 20k-simultaneous case measures the service and the edge, not one
   laptop's connections.
5. **CI:** run `make test-integration` and a small burst against a preview
   deploy on every push, and fix Railway auto-deploy (it currently needs a
   manual deploy because the repo isn't linked to that account).
6. **A hold + TTL checkout**, if a payment step is ever added: the schema
   already has the `held` state.
7. **High availability for Postgres**, with synchronous replication for
   zero-loss failover.
