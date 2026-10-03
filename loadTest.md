# Load test results

Three live load tests against the deployed service, run on 2026-10-03.

## Setup

| | |
|---|---|
| Service | `https://seatlock-production-4dd4.up.railway.app`, commit `f1d45a3` |
| Infrastructure | Railway, Singapore (`asia-southeast1`), **1 replica**; Railway Postgres in the same region |
| Database pool | 32 connections (`DB_MAX_CONNS`) |
| Load generator | The burst tool ([`cmd/burst`](cmd/burst/main.go)) running as a one-off Railway job ([`Dockerfile.burst`](Dockerfile.burst)) in the same region. It calls the **public URL**, so traffic goes through Railway's edge like any client's. Uses HTTP/2. |
| Per-request client timeout | 60s; a request with no response is retried once with the same idempotency key |

Each test creates a **fresh show** and fires every request at once (workers wait on a shared start signal). The request mix:
- **Hot-seat storm:** 25% of requests compete for 5 seats.
- **General buyers:** 1–2 random seats each.
- **Retries:** the same key with the same seats, and the same key with different seats.
- **A greedy user:** 10 parallel requests on a separate limit-4 show.
- **Spoofers:** a `user_id` in the body.

Test 3 has no hot seats. While each test runs, the tool samples `GET /shows/{id}` every 500ms. Afterwards it:
1. probes idempotency;
2. cancels 20 reservations and races 5 buyers for each released seat;
3. reconciles `/metrics` with what it observed.

## Summary

| | **1. 20k all at once** | **2. 100k, hot seats** | **3. 100k, seats free** |
|---|---|---|---|
| Requests | 20,000 | 100,000 | 100,000 |
| In flight at once | **20,000** | 2,000 | 2,000 |
| Seats in the show | 1,000 (5 hot) | 1,000 (5 hot) | **100,000** (none hot) |
| **Result** | **PASS** | **PASS** | **PASS** |
| Duration | 24.0s | 34.3s | 62.2s |
| **Throughput** | 834 req/s | **2,912 req/s** | 1,609 req/s |
| Client latency p50 | 18.4s | 325ms | 973ms |
| Client latency p95 | 23.1s | 1.77s | 1.84s |
| Client latency p99 | 23.4s | 10.4s | 10.3s |
| Client latency max | 23.8s | 16.1s | 15.0s |
| **Server-side reserve latency** p50 / p95 / p99 | ≤25ms / ≤250ms / ≤250ms | ≤250ms / ≤500ms / ≤500ms | ≤1s / ≤2.5s / ≤2.5s |
| 5xx responses | **0** | **0** | **0** |
| Responses lost (no answer even after retry) | **0** | **0** | **0** |

*Client latency* is measured by the load generator, end to end. *Server-side
latency* comes from the service's own `seatlock_http_request_duration_seconds`
histogram, measured from when a request reaches the app until it responds. The
values are histogram bucket bounds, hence "≤".

## Outcomes

| Outcome | 1. 20k all at once | 2. 100k, hot seats | 3. 100k, seats free |
|---|---|---|---|
| 201 confirmed | 786 (3.9%) | 791 (0.8%) | **46,448 (46.4%)** |
| 409 `seat_taken` | 19,085 (95.4%) | 99,054 (99.1%) | 50,460 (50.5%) |
| 200 idempotent replay | 78 | 84 | 1,742 |
| 409 `idempotent_replay_conflict` | 44 | 63 | 831 |
| 409 `per_user_limit` | 7 | 8 | 519 |
| 5xx / other | 0 | 0 | 0 |

In test 3, half the requests still get `seat_taken` even with 100,000 seats:
100k requests for 1–2 random seats each collide often, and once a seat is sold,
later requests for it lose.

## Correctness checks

Every check passed in every test.

| Check | 1 | 2 | 3 |
|---|---|---|---|
| Zero 5xx | PASS | PASS | PASS |
| Every request answered (directly or by one idempotent retry) | PASS | PASS | PASS |
| No seat confirmed twice | PASS | PASS | PASS |
| Each hot seat: exactly one winner | PASS (5 × 1,000 contenders) | PASS (5 × 5,000 contenders) | n/a |
| Idempotency: one reservation per key | PASS | PASS | PASS |
| Idempotency probe: same key + same seats → 200, same key + different seats → 409 | PASS | PASS | PASS |
| Per-user limit: 10 parallel requests on a limit-4 show → exactly 4 | PASS | PASS | PASS |
| Identity from the token; spoofed `user_id` ignored | PASS | PASS | PASS |
| `available + held + confirmed == total` during the burst | PASS (3 samples) | PASS (32 samples) | PASS (30 samples) |
| Confirmed count never decreased during the burst | PASS | PASS | PASS |
| Same invariant after the burst; API count == reservations seen | PASS | PASS | PASS |
| Cancelled seats become available; each re-booked by exactly one of 5 racers | PASS | PASS | PASS |
| `/metrics` counter deltas == what the client observed | PASS | PASS | PASS |
| `safeguard_trips_total` unchanged (no locking bug caught) | PASS | PASS | PASS |

## Server resources

| | 1. 20k all at once | 2. 100k, hot seats | 3. 100k, seats free |
|---|---|---|---|
| Pool acquires | 21,381 | 101,482 | 150,755 |
| Acquires that waited for a free connection | 74% | 96% | **99%** |
| Total time spent waiting for connections | 752s | 19,862s | 93,218s |
| Reserve requests over 1s server-side | 70 | 203 | **45,404** |
| Memory (RSS) after the test | 111 MB | 185 MB | 175 MB |
| App log lines dropped by Railway (limit: 500/s) | 192 | 6,878 | **30,001** |

## What the numbers show

**1. Correctness held at every load.** Under each test's peak there were no
double-sells, no 5xx and no lost responses, and the invariant held throughout.

**2. "20,000 at once" is a queue, not a slowdown.** Each request takes ≤250ms
inside the server, but 20,000 arrive together and about 830–1,700 complete per
second, so the last one waits about 20,000 ÷ throughput. That's 12–24s here,
and it matches the measured client latency (Little's law). The wait happens in
front of the handlers and stays under the 30s server write timeout, so nothing
was dropped.

**3. Run-to-run variation on shared infrastructure is large.** An earlier cloud
run of test 1, on the same application code (commit `722406b`), took **11.9s at
1,687 req/s** (client p50 4.9s, p99 11.5s), against **24.0s at 834 req/s** in
this run. Server-side latency was the same in both (p99 ≤250ms). The difference
is in how quickly the queue in front of the app drained (load generator
placement, edge load). Treat single-run throughput as an indication, not a
precise figure.

**4. With hot seats, almost all of the work is cheap.** In test 2, 99% of
requests are rejected by an unlocked pre-check read before any transaction. So
2,000 in flight sustains about 2,900 req/s, the highest throughput of the three.

**5. Free seats are the true capacity bound.** In test 3, every request needs
a full transaction: claim the key, lock the user, lock the seats, confirm. 99%
of connection acquires waited, and server-side p50 rose to ≤1s. The 32-connection
pool and Postgres are the limit, at about 1,600 req/s with mostly real bookings.
The next levers are a larger pool (Postgres allows ~100 connections), a larger
Postgres instance, and fewer round trips per transaction.

**6. Logs hit Railway's 500 lines/s limit at these rates; metrics did not.**
The service writes one line per request, but only 1 in 10 hot-seat declines, plus
a per-second summary. That's enough up to about 2,000 req/s of mostly-declined
traffic.
- **Test 2** (~2,900 req/s): Railway dropped 6,878 lines.
- **Test 3**: 46% of requests are confirmed bookings, which are always logged,
  so Railway dropped 30,001 lines.

Counts stay exact in `/metrics`, which every run reconciled. The fix options:
- a higher seat-taken sample rate (`LOG_SAMPLE_SEAT_TAKEN=50`), which helps
  test 2 but not test 3;
- sampling successful bookings as well;
- shipping logs to a dedicated log store instead of the platform's stdout
  pipeline.

## Reproduce

Locally or from any machine (but one laptop can't open thousands of
simultaneous connections; see DESIGN.md):

```sh
B=https://seatlock-production-4dd4.up.railway.app
./burst.sh $B -requests 20000  -concurrency 20000 -timeout 60s                                       # 1
./burst.sh $B -requests 100000 -concurrency 2000  -seats 1000 -timeout 60s                           # 2
./burst.sh $B -requests 100000 -concurrency 2000  -seats 100000 -hot-share 0 -users 100000 -timeout 60s  # 3
```

From the cloud, as these results were produced: create a service from this repo
with `RAILWAY_DOCKERFILE_PATH=Dockerfile.burst`, restart policy "never", in the
app's region. Set the variables `BASE_URL`, `ADMIN_KEY`, and `BURST_ARGS` (the
flags above). Deploy, and read the result from the job's logs.
