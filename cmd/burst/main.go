// Command burst reproduces an on-sale stampede against a SeatLock deployment
// and checks that the service stayed correct.
//
//	go run ./cmd/burst -base https://seatlock-production-4dd4.up.railway.app
//
// It creates a fresh show, fires a concurrent mix of hot-seat storms, general
// buyers, idempotent retries, a greedy user and spoofed bodies, samples the
// show's invariant while the burst runs, then cancels some winners and races
// buyers for the released seats. It prints the outcome distribution, latency,
// and PASS/FAIL checks, and exits 1 if any check fails.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type config struct {
	base         string
	adminKey     string
	requests     int
	concurrency  int
	seats        int
	hotSeats     int
	hotShare     float64
	retryShare   float64
	users        int
	limit        int
	rebookSeats  int
	pollInterval time.Duration
	timeout      time.Duration
	http2        bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.base, "base", "", "base URL of the service (required)")
	flag.StringVar(&cfg.adminKey, "admin-key", envOr("ADMIN_KEY", "admin"), "X-Admin-Key for creating the show")
	flag.IntVar(&cfg.requests, "requests", 20000, "reserve requests in the stampede")
	flag.IntVar(&cfg.concurrency, "concurrency", 500, "requests in flight at once")
	flag.IntVar(&cfg.seats, "seats", 1000, "seats in the show")
	flag.IntVar(&cfg.hotSeats, "hot-seats", 5, "number of hot seats everyone fights over")
	flag.Float64Var(&cfg.hotShare, "hot-share", 0.25, "share of requests aimed at the hot seats")
	flag.Float64Var(&cfg.retryShare, "retry-share", 0.05, "share of requests that are idempotent retries")
	flag.IntVar(&cfg.users, "users", 3000, "distinct general buyers (fewer means more per-user-limit hits)")
	flag.IntVar(&cfg.limit, "limit", 4, "per_user_limit for the show")
	flag.IntVar(&cfg.rebookSeats, "rebook", 20, "reservations to cancel and re-race after the burst")
	flag.DurationVar(&cfg.pollInterval, "poll", 500*time.Millisecond, "how often to sample show state during the burst")
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "per-request timeout")
	flag.BoolVar(&cfg.http2, "http2", false, "allow HTTP/2 (default HTTP/1.1: one connection per client, like real buyers)")
	flag.Parse()
	if cfg.base == "" && flag.NArg() > 0 {
		cfg.base = flag.Arg(0)
	}
	if cfg.base == "" {
		fmt.Fprintln(os.Stderr, "usage: burst -base <BASE_URL> [flags]")
		os.Exit(2)
	}
	cfg.base = strings.TrimRight(cfg.base, "/")
	if cfg.hotSeats >= cfg.seats {
		fmt.Fprintln(os.Stderr, "-hot-seats must be smaller than -seats")
		os.Exit(2)
	}

	ok := run(cfg)
	if !ok {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- HTTP

type client struct {
	cfg  config
	http *http.Client
}

func newClient(cfg config) *client {
	tr := &http.Transport{
		MaxIdleConns:        cfg.concurrency,
		MaxIdleConnsPerHost: cfg.concurrency,
		IdleConnTimeout:     90 * time.Second,
	}
	if !cfg.http2 {
		// An empty TLSNextProto disables HTTP/2, so concurrent requests use
		// separate connections instead of multiplexing over one.
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &client{cfg: cfg, http: &http.Client{Transport: tr, Timeout: cfg.timeout}}
}

type response struct {
	status  int
	body    map[string]any
	raw     []byte
	latency time.Duration
	err     error
}

func (c *client) do(method, path string, headers map[string]string, body any) response {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.cfg.base+path, rd)
	if err != nil {
		return response{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return response{err: err, latency: time.Since(start)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := response{status: resp.StatusCode, raw: raw, latency: time.Since(start)}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (r response) str(key string) string {
	s, _ := r.body[key].(string)
	return s
}

func (r response) strs(key string) []string {
	arr, _ := r.body[key].([]any)
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------- jobs

type kind string

const (
	kindHot      kind = "hot-seat"
	kindGeneral  kind = "general"
	kindRetry    kind = "retry"
	kindConflict kind = "key-conflict"
	kindGreedy   kind = "greedy"
	kindSpoof    kind = "spoof"
	kindRebook   kind = "rebook"
)

type job struct {
	show   string
	kind   kind
	user   string
	seats  []string
	key    string
	extra  map[string]any // extra body fields (spoofed user_id)
	result response
}

func (c *client) reserve(j *job) {
	body := map[string]any{"seats": j.seats, "idempotency_key": j.key}
	for k, v := range j.extra {
		body[k] = v
	}
	j.result = c.do("POST", "/shows/"+j.show+"/reserve", map[string]string{"Authorization": "Bearer " + j.user}, body)
}

// outcome is how a response is tallied: status plus decline reason.
func outcome(r response) string {
	switch {
	case r.err != nil:
		return "network error / timeout"
	case r.status == 201:
		return "201 confirmed"
	case r.status == 200:
		return "200 idempotent replay"
	case r.status >= 500:
		return fmt.Sprintf("%d SERVER ERROR", r.status)
	default:
		reason := r.str("reason")
		if reason == "" {
			reason = "?"
		}
		return fmt.Sprintf("%d %s", r.status, reason)
	}
}

// buildJobs builds the stampede for showID. The greedy user's 10 requests go
// to limitShowID, a separate show whose seats are all free, so the per-user
// limit is tested on seats it could otherwise have won.
func buildJobs(cfg config, showID, limitShowID string, hot, general []string, rng *rand.Rand) []*job {
	var jobs []*job
	newKey := func() string { return uuid.NewString() }
	pick := func(n int) []string {
		seen := map[int]bool{}
		out := make([]string, 0, n)
		for len(out) < n {
			if i := rng.Intn(len(general)); !seen[i] {
				seen[i] = true
				out = append(out, general[i])
			}
		}
		return out
	}

	hotN := int(float64(cfg.requests) * cfg.hotShare)
	for i := 0; i < hotN; i++ {
		jobs = append(jobs, &job{kind: kindHot, user: fmt.Sprintf("hot-%d", i), seats: []string{hot[i%len(hot)]}, key: newKey()})
	}

	// Retry groups: an original plus two identical retries (same user, key and
	// seats), and every other group one request reusing the key for other seats.
	retryN := int(float64(cfg.requests) * cfg.retryShare)
	for g := 0; len(jobs) < hotN+retryN; g++ {
		user, key, seats := fmt.Sprintf("retry-%d", g), newKey(), pick(1+rng.Intn(2))
		for k := 0; k < 3; k++ {
			jobs = append(jobs, &job{kind: kindRetry, user: user, seats: seats, key: key})
		}
		if g%2 == 0 {
			jobs = append(jobs, &job{kind: kindConflict, user: user, seats: pick(1), key: key})
		}
	}

	for i := 1; i <= 10; i++ {
		jobs = append(jobs, &job{show: limitShowID, kind: kindGreedy, user: "greedy", seats: []string{fmt.Sprintf("L%d", i)}, key: newKey()})
	}
	for i := 0; i < 5; i++ {
		jobs = append(jobs, &job{kind: kindSpoof, user: fmt.Sprintf("spoof-%d", i), seats: pick(1), key: newKey(),
			extra: map[string]any{"user_id": "victim"}})
	}

	for len(jobs) < cfg.requests {
		jobs = append(jobs, &job{kind: kindGeneral, user: fmt.Sprintf("buyer-%d", rng.Intn(cfg.users)), seats: pick(1 + rng.Intn(2)), key: newKey()})
	}
	for _, j := range jobs {
		if j.show == "" {
			j.show = showID
		}
	}
	rng.Shuffle(len(jobs), func(i, k int) { jobs[i], jobs[k] = jobs[k], jobs[i] })
	return jobs
}

// runConcurrently runs jobs with at most n in flight. All workers wait on a
// shared start signal so the first wave hits at the same instant (on-sale).
func runConcurrently(n int, jobs []*job, fn func(*job)) time.Duration {
	ch := make(chan *job)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := range ch {
				fn(j)
			}
		}()
	}
	t0 := time.Now()
	close(start)
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return time.Since(t0)
}

// ---------------------------------------------------------------- checks

type report struct {
	failed bool
}

func (r *report) check(name string, ok bool, detail string) {
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		r.failed = true
	}
	fmt.Printf("  %s  %s", mark, name)
	if detail != "" {
		fmt.Printf("  (%s)", detail)
	}
	fmt.Println()
}

func (r *report) warn(name, detail string) {
	fmt.Printf("  WARN  %s  (%s)\n", name, detail)
}

type counts struct{ Available, Held, Confirmed, Total int }

func (c counts) consistent() bool { return c.Available+c.Held+c.Confirmed == c.Total }

func (c *client) showCounts(showID string) (counts, error) {
	r := c.do("GET", "/shows/"+showID, nil, nil)
	if r.err != nil || r.status != 200 {
		return counts{}, fmt.Errorf("GET /shows/%s: status %d err %v", showID, r.status, r.err)
	}
	m, _ := r.body["counts"].(map[string]any)
	n := func(k string) int { f, _ := m[k].(float64); return int(f) }
	return counts{n("available"), n("held"), n("confirmed"), n("total")}, nil
}

var metricLine = regexp.MustCompile(`(?m)^(seatlock_[a-z_]+(?:\{[^}]*\})?) (\S+)$`)

func (c *client) scrapeMetrics() map[string]float64 {
	r := c.do("GET", "/metrics", nil, nil)
	if r.err != nil || r.status != 200 {
		return nil
	}
	out := map[string]float64{}
	for _, m := range metricLine.FindAllSubmatch(r.raw, -1) {
		v, err := strconv.ParseFloat(string(m[2]), 64)
		if err == nil {
			out[string(m[1])] = v
		}
	}
	return out
}

// ---------------------------------------------------------------- run

func run(cfg config) bool {
	c := newClient(cfg)
	rep := &report{}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Printf("SeatLock burst against %s\n\n", cfg.base)
	if r := c.do("GET", "/readyz", nil, nil); r.err != nil || r.status != 200 {
		fmt.Printf("service not ready: status %d, err %v\n", r.status, r.err)
		return false
	}

	labels := make([]string, cfg.seats)
	for i := range labels {
		labels[i] = fmt.Sprintf("S%d", i+1)
	}
	hot, general := labels[:cfg.hotSeats], labels[cfg.hotSeats:]
	created := c.do("POST", "/shows", map[string]string{"X-Admin-Key": cfg.adminKey},
		map[string]any{"name": "burst-" + time.Now().UTC().Format("20060102-150405"), "seats": labels, "price_paise": 25000, "per_user_limit": cfg.limit})
	if created.status != 201 {
		fmt.Printf("creating show failed: status %d %s\n", created.status, created.raw)
		return false
	}
	showID := created.str("id")
	fmt.Printf("show %s: %d seats (hot: %s), per-user limit %d\n", showID, cfg.seats, strings.Join(hot, ", "), cfg.limit)
	limitSeats := make([]string, 10)
	for i := range limitSeats {
		limitSeats[i] = fmt.Sprintf("L%d", i+1)
	}
	limitShow := c.do("POST", "/shows", map[string]string{"X-Admin-Key": cfg.adminKey},
		map[string]any{"name": "burst-limit-check", "seats": limitSeats, "price_paise": 25000, "per_user_limit": cfg.limit})
	if limitShow.status != 201 {
		fmt.Printf("creating limit-check show failed: status %d %s\n", limitShow.status, limitShow.raw)
		return false
	}

	before := c.scrapeMetrics()
	jobs := buildJobs(cfg, showID, limitShow.str("id"), hot, general, rng)
	fmt.Printf("stampede: %d reserve requests, %d in flight at once...\n\n", len(jobs), cfg.concurrency)

	// Sample the show while the stampede runs: the invariant must hold
	// throughout, and with no cancels in flight confirmed must never drop.
	ctx, stopPoll := context.WithCancel(context.Background())
	var samples, badSamples, drops int
	var sampleErr error
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		last := -1
		t := time.NewTicker(cfg.pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cn, err := c.showCounts(showID)
				if err != nil {
					sampleErr = err
					continue
				}
				samples++
				if !cn.consistent() {
					badSamples++
				}
				if cn.Confirmed < last {
					drops++
				}
				last = cn.Confirmed
			}
		}
	}()
	elapsed := runConcurrently(cfg.concurrency, jobs, c.reserve)
	stopPoll()
	<-pollDone

	// ---- distribution
	tally := map[string]int{}
	var lat []time.Duration
	for _, j := range jobs {
		tally[outcome(j.result)]++
		if j.result.err == nil {
			lat = append(lat, j.result.latency)
		}
	}
	printDistribution("Stampede outcomes", tally, len(jobs))
	sort.Slice(lat, func(i, k int) bool { return lat[i] < lat[k] })
	fmt.Printf("\n  %d requests in %.1fs (%.0f req/s)   latency p50 %s  p95 %s  p99 %s  max %s\n",
		len(jobs), elapsed.Seconds(), float64(len(jobs))/elapsed.Seconds(),
		pct(lat, .50), pct(lat, .95), pct(lat, .99), pct(lat, 1))

	// ---- correctness checks on the stampede
	fmt.Println("\nCorrectness")
	serverErrors, netErrors := 0, 0
	for _, j := range jobs {
		if j.result.err != nil {
			netErrors++
		} else if j.result.status >= 500 {
			serverErrors++
		}
	}
	rep.check("zero 5xx", serverErrors == 0, fmt.Sprintf("%d server errors", serverErrors))
	rep.check("every request got a response", netErrors == 0, fmt.Sprintf("%d network errors/timeouts", netErrors))
	if netErrors > 0 {
		printNetErrors(jobs)
	}

	owner := map[string]string{} // seat -> reservation_id that confirmed it
	doubleSold := 0
	confirmedSeats := 0 // on the main show
	seatsBooked := 0    // across both shows, for the metrics check
	for _, j := range jobs {
		if j.result.status != 201 {
			continue
		}
		seatsBooked += len(j.result.strs("seats"))
		if j.show != showID {
			continue
		}
		for _, s := range j.result.strs("seats") {
			if prev, taken := owner[s]; taken && prev != j.result.str("reservation_id") {
				doubleSold++
			}
			owner[s] = j.result.str("reservation_id")
			confirmedSeats++
		}
	}
	rep.check("no seat confirmed twice", doubleSold == 0, fmt.Sprintf("%d seats in more than one 201", doubleSold))

	winners := map[string]int{}
	hotTries := map[string]int{}
	for _, j := range jobs {
		if j.kind != kindHot {
			continue
		}
		hotTries[j.seats[0]]++
		if j.result.status == 201 {
			winners[j.seats[0]]++
		}
	}
	for _, s := range hot {
		rep.check(fmt.Sprintf("hot seat %s: exactly one winner", s), winners[s] == 1,
			fmt.Sprintf("%d of %d requests got 201", winners[s], hotTries[s]))
	}

	// Idempotency: per key at most one 201, and every replay returns that reservation.
	byKey := map[string][]*job{}
	for _, j := range jobs {
		if j.kind == kindRetry || j.kind == kindConflict {
			byKey[j.key] = append(byKey[j.key], j)
		}
	}
	keysWithTwo201, badReplays, conflicts := 0, 0, 0
	for _, group := range byKey {
		var created string
		n201 := 0
		for _, j := range group {
			if j.result.status == 201 {
				n201++
				created = j.result.str("reservation_id")
			}
			if j.result.status == 409 && j.result.str("reason") == "idempotent_replay_conflict" {
				conflicts++
			}
		}
		if n201 > 1 {
			keysWithTwo201++
		}
		for _, j := range group {
			if j.result.status == 200 && (created == "" || j.result.str("reservation_id") != created) {
				badReplays++
			}
		}
	}
	rep.check("idempotency: one reservation per key", keysWithTwo201 == 0, fmt.Sprintf("%d keys, %d with >1 booking", len(byKey), keysWithTwo201))
	rep.check("idempotency: replays return the original", badReplays == 0, fmt.Sprintf("%d mismatched replays", badReplays))
	// Whether an in-burst key reuse conflicts depends on ordering (if the
	// original hadn't committed or lost its seat, the reuse is a fresh attempt),
	// so it's reported, and the deterministic check is the probe below.
	fmt.Printf("  info  same key reused for other seats during the burst: %d got 409 idempotent_replay_conflict\n", conflicts)
	probeTally := probeIdempotency(c, rep, showID, jobs, general)
	for k, v := range probeTally {
		tally[k] += v
	}

	greedySeats := 0
	for _, j := range jobs {
		if j.kind == kindGreedy && j.result.status == 201 {
			greedySeats += len(j.result.strs("seats"))
		}
	}
	rep.check(fmt.Sprintf("per-user limit: greedy user's 10 parallel requests for free seats got exactly %d", cfg.limit),
		greedySeats == min(cfg.limit, 10), fmt.Sprintf("got %d", greedySeats))

	spoofOK := true
	for _, j := range jobs {
		if j.kind == kindSpoof && j.result.status == 201 && j.result.str("user_id") != j.user {
			spoofOK = false
		}
	}
	rep.check("identity from token, not body (spoofed user_id ignored)", spoofOK, "")

	final, err := c.showCounts(showID)
	if err != nil {
		rep.check("read final show state", false, err.Error())
		return false
	}
	rep.check("invariant held during the burst", badSamples == 0 && samples > 0,
		fmt.Sprintf("%d samples, %d violations", samples, badSamples))
	if sampleErr != nil {
		rep.warn("some samples failed", sampleErr.Error())
	}
	rep.check("confirmed count never decreased during the burst", drops == 0, fmt.Sprintf("%d drops", drops))
	rep.check("invariant after the burst: available + held + confirmed == total", final.consistent(),
		fmt.Sprintf("%d + %d + %d vs %d", final.Available, final.Held, final.Confirmed, final.Total))
	rep.check("API confirmed count == seats in all 201 responses", final.Confirmed == confirmedSeats,
		fmt.Sprintf("API %d, responses %d", final.Confirmed, confirmedSeats))

	// ---- release and re-book
	rb := rebook(c, rep, cfg, showID, jobs, final)
	for k, v := range rb.tally {
		tally[k] += v
	}

	// ---- metrics reconciliation
	reconcileMetrics(c, rep, before, tally, seatsBooked+rb.tally["201 confirmed"], rb)

	fmt.Println()
	if rep.failed {
		fmt.Println("RESULT: FAIL")
		return false
	}
	fmt.Println("RESULT: PASS")
	return true
}

type rebookResult struct {
	tally     map[string]int
	cancelled int
	released  int
}

// probeIdempotency takes keys that definitely booked during the burst and
// resends each with the same seats (must replay the original, 200) and with
// different seats (must be 409 idempotent_replay_conflict).
func probeIdempotency(c *client, rep *report, showID string, jobs []*job, general []string) map[string]int {
	tally := map[string]int{}
	var booked []*job
	for _, j := range jobs {
		if (j.kind == kindRetry || j.kind == kindGeneral) && j.result.status == 201 && len(booked) < 20 {
			booked = append(booked, j)
		}
	}
	if len(booked) == 0 {
		rep.warn("idempotency probe skipped", "no reservations to probe")
		return tally
	}
	replayOK, conflictOK := 0, 0
	for _, j := range booked {
		same := append([]string(nil), j.seats...)
		sort.Sort(sort.Reverse(sort.StringSlice(same))) // order must not matter
		r := c.do("POST", "/shows/"+showID+"/reserve", map[string]string{"Authorization": "Bearer " + j.user},
			map[string]any{"seats": same, "idempotency_key": j.key})
		tally[outcome(r)]++
		if r.status == 200 && r.str("reservation_id") == j.result.str("reservation_id") {
			replayOK++
		}
		other := general[0]
		for _, g := range general {
			if !contains(j.seats, g) {
				other = g
				break
			}
		}
		r = c.do("POST", "/shows/"+showID+"/reserve", map[string]string{"Authorization": "Bearer " + j.user},
			map[string]any{"seats": []string{other}, "idempotency_key": j.key})
		tally[outcome(r)]++
		if r.status == 409 && r.str("reason") == "idempotent_replay_conflict" {
			conflictOK++
		}
	}
	rep.check("idempotency probe: same key + same seats -> 200 with the original reservation", replayOK == len(booked),
		fmt.Sprintf("%d of %d", replayOK, len(booked)))
	rep.check("idempotency probe: same key + different seats -> 409 idempotent_replay_conflict", conflictOK == len(booked),
		fmt.Sprintf("%d of %d", conflictOK, len(booked)))
	return tally
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// rebook cancels some confirmed reservations, checks the seats come back, and
// races several buyers for each released seat: exactly one must win each.
func rebook(c *client, rep *report, cfg config, showID string, jobs []*job, before counts) rebookResult {
	var victims []*job
	for _, j := range jobs {
		if j.kind == kindGeneral && j.result.status == 201 && len(victims) < cfg.rebookSeats {
			victims = append(victims, j)
		}
	}
	fmt.Printf("\nRelease and re-book: cancelling %d reservations, then 5 buyers race for each released seat\n", len(victims))
	if len(victims) == 0 {
		rep.warn("release and re-book skipped", "no general reservations to cancel")
		return rebookResult{tally: map[string]int{}}
	}

	var released []string
	cancelOK := 0
	var mu sync.Mutex
	runConcurrently(cfg.concurrency, victims, func(j *job) {
		r := c.do("POST", "/reservations/"+j.result.str("reservation_id")+"/cancel", map[string]string{"Authorization": "Bearer " + j.user}, nil)
		mu.Lock()
		defer mu.Unlock()
		if r.status == 200 && r.str("status") == "cancelled" {
			cancelOK++
			released = append(released, r.strs("seats")...)
		}
	})
	rep.check("owner cancels succeed", cancelOK == len(victims), fmt.Sprintf("%d of %d", cancelOK, len(victims)))

	afterCancel, err := c.showCounts(showID)
	if err == nil {
		rep.check("cancelled seats are available again", afterCancel.Available == before.Available+len(released) && afterCancel.consistent(),
			fmt.Sprintf("available %d -> %d, released %d", before.Available, afterCancel.Available, len(released)))
	}

	var racers []*job
	for _, seat := range released {
		for k := 0; k < 5; k++ {
			racers = append(racers, &job{show: showID, kind: kindRebook, user: fmt.Sprintf("rebook-%s-%d", seat, k), seats: []string{seat}, key: uuid.NewString()})
		}
	}
	runConcurrently(cfg.concurrency, racers, c.reserve)
	wins := map[string]int{}
	tally := map[string]int{}
	for _, j := range racers {
		tally[outcome(j.result)]++
		if j.result.status == 201 {
			wins[j.seats[0]]++
		}
	}
	oneEach := true
	for _, seat := range released {
		if wins[seat] != 1 {
			oneEach = false
		}
	}
	rep.check("each released seat re-booked by exactly one buyer", oneEach,
		fmt.Sprintf("%d seats, %d racers", len(released), len(racers)))
	if final, err := c.showCounts(showID); err == nil {
		rep.check("after re-booking, confirmed is back where it was", final.Confirmed == before.Confirmed && final.consistent(),
			fmt.Sprintf("%d -> %d", before.Confirmed, final.Confirmed))
	}
	return rebookResult{tally: tally, cancelled: cancelOK, released: len(released)}
}

// reconcileMetrics compares /metrics deltas with what this client observed.
// Counters are global, so other traffic during the run shows up as a mismatch;
// that's reported as a warning rather than a failure.
func reconcileMetrics(c *client, rep *report, before map[string]float64, tally map[string]int, seatsBooked int, rb rebookResult) {
	fmt.Println("\nMetrics reconciliation (/metrics deltas vs this run)")
	after := c.scrapeMetrics()
	if before == nil || after == nil {
		rep.warn("skipped", "/metrics not available on this deployment")
		return
	}
	d := func(k string) int { return int(after[k] - before[k]) }

	pairs := []struct {
		metric string
		want   int
	}{
		{"seatlock_reservations_confirmed_total", tally["201 confirmed"]},
		{"seatlock_seats_confirmed_total", seatsBooked},
		{`seatlock_reservations_declined_total{reason="seat_taken"}`, tally["409 seat_taken"]},
		{`seatlock_reservations_declined_total{reason="per_user_limit"}`, tally["409 per_user_limit"]},
		{`seatlock_reservations_declined_total{reason="idempotent_replay_conflict"}`, tally["409 idempotent_replay_conflict"]},
		{`seatlock_reservations_declined_total{reason="idempotent_replay"}`, tally["200 idempotent replay"]},
		{"seatlock_reservations_cancelled_total", rb.cancelled},
		{"seatlock_seats_released_total", rb.released},
		{"seatlock_safeguard_trips_total", 0},
	}
	for _, p := range pairs {
		got := d(p.metric)
		name := strings.TrimPrefix(p.metric, "seatlock_")
		if got == p.want {
			rep.check(name, true, fmt.Sprintf("delta %d", got))
		} else if p.metric == "seatlock_safeguard_trips_total" {
			rep.check(name, false, fmt.Sprintf("delta %d: the reserve safeguard fired", got))
		} else {
			rep.warn(name, fmt.Sprintf("delta %d, this run saw %d; other traffic during the run?", got, p.want))
		}
	}
	if after["seatlock_seat_gauges_up"] == 1 {
		g := after["seatlock_seats_available"] + after["seatlock_seats_held"] + after["seatlock_seats_confirmed"]
		rep.check("seat gauges: available + held + confirmed == total", g == after["seatlock_seats_total"],
			fmt.Sprintf("%.0f vs %.0f", g, after["seatlock_seats_total"]))
	}
}

// ---------------------------------------------------------------- output

func printDistribution(title string, tally map[string]int, total int) {
	fmt.Println(title)
	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, k int) bool { return tally[keys[i]] > tally[keys[k]] })
	for _, k := range keys {
		fmt.Printf("  %-36s %7d  %5.1f%%\n", k, tally[k], 100*float64(tally[k])/float64(total))
	}
}

// printNetErrors groups requests that got no response by error text (the
// request URL stripped out), with how long they took, to tell client-side
// connection problems from server or proxy behaviour.
func printNetErrors(jobs []*job) {
	type agg struct {
		n        int
		min, max time.Duration
	}
	byErr := map[string]*agg{}
	for _, j := range jobs {
		if j.result.err == nil {
			continue
		}
		msg := j.result.err.Error()
		if i := strings.LastIndex(msg, "\": "); i >= 0 {
			msg = msg[i+3:]
		}
		a := byErr[msg]
		if a == nil {
			a = &agg{min: j.result.latency}
			byErr[msg] = a
		}
		a.n++
		a.min = min(a.min, j.result.latency)
		a.max = max(a.max, j.result.latency)
	}
	for msg, a := range byErr {
		fmt.Printf("        %d x %q after %s-%s\n", a.n, msg, a.min.Round(time.Millisecond), a.max.Round(time.Millisecond))
	}
}

func pct(sorted []time.Duration, p float64) string {
	if len(sorted) == 0 {
		return "-"
	}
	i := int(float64(len(sorted))*p) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i].Round(time.Millisecond).String()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
