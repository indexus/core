// experiment drives and observes a live network: see docs/benchmarks/p2p-experiment.md.
//
//	experiment load    -targets URL,... -issuer URL -scenario normal -rate 200 -duration 5m -out DIR
//	experiment observe -monitors URL,... -every 2s -duration 10m -out DIR
//	experiment audit   -targets URL,... -monitors URL,... -issuer URL -out DIR
//
// load writes DIR/load.jsonl (one line per second) and DIR/acked.jsonl (every
// acknowledged key). observe writes DIR/metrics.jsonl. audit writes DIR/audit.json
// and exits 1 when a safety check fails.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: experiment load|observe|audit|settlement|agent|ca|sentinel|loadagent [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "load":
		err = load(ctx, os.Args[2:])
	case "observe":
		err = observe(ctx, os.Args[2:])
	case "settlement":
		err = settlement(ctx, os.Args[2:])
	case "audit":
		err = audit(ctx, os.Args[2:])
	case "agent":
		err = agent(ctx, os.Args[2:])
	case "ca":
		err = caCommand(os.Args[2:])
	case "sentinel":
		err = sentinel(ctx, os.Args[2:])
	case "loadagent":
		err = loadAgent(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// client reaches nodes like the mesh does (meshhttp.NewTransport): over TCP
// in this build, which links no QUIC stack; trust comes from SSL_CERT_FILE.
var client = &http.Client{Timeout: 30 * time.Second, Transport: meshhttp.NewTransport(&http.Transport{ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 512})}

func split(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func token(issuer string) (string, error) {
	return tokenContext(context.Background(), issuer)
}

func tokenContext(ctx context.Context, issuer string) (string, error) {
	body := bytes.NewBufferString(`{"client_id":"experiment","scopes":["read","write"]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/v1/issue/token", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct{ Token string }
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("issuer token: %v", err)
	}
	return out.Token, nil
}

// ---------------------------------------------------------------- load

// key is one write. Collections and locations are 16-character identifiers
// (a collection may not be shorter than the location it holds).
type key struct {
	Collection string `json:"c"`
	Location   string `json:"l"`
	ID         string `json:"i"`
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

func randomLocation(rng *rand.Rand, prefix string, n int) string {
	b := []byte(prefix)
	for len(b) < n {
		b = append(b, alphabet[rng.IntN(len(alphabet))])
	}
	return string(b)
}

// scenario returns the key for write number i at elapsed time t, and whether
// it belongs to the scenario's hot part.
type scenario func(rng *rand.Rand, i int64, t time.Duration) (key, bool)

// Collections are named <tag><kind><number>, 16 characters: each run writes
// only its own collections, so an audit can count them exactly.
func makeScenario(name, tag string, collections int, spikeStart, spikeLength time.Duration, duration time.Duration) (scenario, error) {
	named := func(kind string, n int) string { return fmt.Sprintf("%s%s%0*d", tag, kind, 16-len(tag)-len(kind), n) }
	hotCollection, deepCollection := named("h", 0), named("d", 0)
	uniform := func(rng *rand.Rand, i int64) key {
		return key{Collection: named("u", rng.IntN(collections)), Location: randomLocation(rng, "", 16), ID: fmt.Sprint("w", i)}
	}
	switch name {
	case "normal", "steps", "ramp":
		return func(rng *rand.Rand, i int64, _ time.Duration) (key, bool) { return uniform(rng, i), false }, nil
	case "spike":
		// The extra arrivals of the spike window all land under one prefix.
		return func(rng *rand.Rand, i int64, t time.Duration) (key, bool) {
			if t >= spikeStart && t < spikeStart+spikeLength && i%2 == 0 {
				return key{Collection: hotCollection, Location: randomLocation(rng, "Hot", 16), ID: fmt.Sprint("w", i)}, true
			}
			return uniform(rng, i), false
		}, nil
	case "hot":
		// One atomic zone: the single-zone ceiling.
		return func(_ *rand.Rand, i int64, _ time.Duration) (key, bool) {
			return key{Collection: hotCollection, Location: "HotHotHotHotHotH", ID: fmt.Sprint("w", i)}, true
		}, nil
	case "deep":
		// Keys share a prefix that lengthens over the run: an ever deeper trie.
		return func(rng *rand.Rand, i int64, t time.Duration) (key, bool) {
			depth := 1 + int(15*t/max(duration, time.Second))
			return key{Collection: deepCollection, Location: randomLocation(rng, strings.Repeat("D", depth), 16), ID: fmt.Sprint("w", i)}, true
		}, nil
	}
	return nil, fmt.Errorf("unknown scenario %q (normal, spike, steps, ramp, hot, deep)", name)
}

// stepLevel is the steps scenario's rate multiplier at t: a plateau at the
// base rate, then for each step a linear ramp to factor times the previous
// level and a plateau there.
func stepLevel(t time.Duration, factor float64, ramp, plateau time.Duration) float64 {
	level := 1.0
	if t < plateau {
		return level
	}
	t -= plateau
	for period := ramp + plateau; t >= period; t -= period {
		level *= factor
	}
	if t < ramp {
		return level * (1 + (factor-1)*float64(t)/float64(max(ramp, time.Nanosecond)))
	}
	return level * factor
}

// rampLevel is the ramp scenario's rate multiplier at t (RAMP-1): held at 1
// for hold, then doubled every period. Within a period the first share of
// the doubling is linear and the rest is one jump at the period's end:
// share 1 is a continuous linear ramp to 2x, share 0 an instant 2x step
// (DynamoDB on demand absorbs 2x its previous peak at once), 0.5 grows +50%
// linearly then jumps +33%.
func rampLevel(t time.Duration, period, hold time.Duration, share float64) float64 {
	if t < hold || period <= 0 {
		return 1
	}
	t -= hold
	k := int(t / period)
	f := float64(t%period) / float64(period)
	return math.Ldexp(1+min(max(share, 0), 1)*f, k)
}

// knee is the ramp scenario's stop rule: the load stops once fewer than
// below of the writes offered over the last window seconds were
// acknowledged (shed arrivals count as not acknowledged). It never fires in
// the first window of the run.
type knee struct {
	below          float64
	window         int
	offered, acked []int64
}

func (k *knee) add(offered, acked int64) bool {
	if k.below <= 0 || k.window <= 0 {
		return false
	}
	k.offered, k.acked = append(k.offered, offered), append(k.acked, acked)
	if len(k.offered) <= k.window {
		return false
	}
	var o, a int64
	for i := len(k.offered) - k.window; i < len(k.offered); i++ {
		o, a = o+k.offered[i], a+k.acked[i]
	}
	return o > 0 && float64(a) < k.below*float64(o)
}

// histogram mirrors internal/metrics: bucket i holds [2^(i-1), 2^i) µs.
type histogram [40]atomic.Int64

func (h *histogram) observe(d time.Duration) {
	i := bits.Len64(uint64(d / time.Microsecond))
	h[min(i, len(h)-1)].Add(1)
}

func (h *histogram) drain() []int64 {
	out := make([]int64, len(h))
	last := 0
	for i := range h {
		if out[i] = h[i].Swap(0); out[i] != 0 {
			last = i + 1
		}
	}
	return out[:last]
}

type second struct {
	T           float64        `json:"t"`
	Rate        float64        `json:"offered_rate"`
	Offered     int64          `json:"offered"`
	Shed        int64          `json:"shed"`
	Acked       int64          `json:"acked"`
	HotAcked    int64          `json:"hot_acked"`
	Failed      map[int]int64  `json:"failed,omitempty"`
	Errors      map[string]int `json:"errors,omitempty"`
	Latency     []int64        `json:"latency_us_log2"`
	HTTPAttempt []int64        `json:"http_attempt_us_log2"`
	RouteLearn  []int64        `json:"route_learn_us_log2"`
	// Intended latency: from the write's scheduled arrival, queueing in the
	// load machine included (no coordinated omission, as wrk2 and YCSB's
	// intended latency). Latency above starts when a worker takes the write.
	Intended []int64 `json:"intended_latency_us_log2,omitempty"`
	// Terminal failures are client jobs, while Failed counts HTTP attempts.
	Exhausted        int64   `json:"exhausted,omitempty"`
	ExhaustedLatency []int64 `json:"exhausted_latency_us_log2,omitempty"`
	Entries          int     `json:"entries,omitempty"`
	InFlight         int64   `json:"in_flight"`
	Queued           int     `json:"queued"`
	Workers          int     `json:"workers"`
	// SlowByNode counts, per target node index, acknowledged writes that
	// took 250 ms or more: one index holding them all is one slow node.
	SlowByNode map[int]int64 `json:"slow_by_node,omitempty"`
	// CPU is the cores the load process used in that second, and Cores the
	// machine's: near Cores, the loader is the limit, not the cluster.
	CPU   float64 `json:"cpu_cores"`
	Cores int     `json:"machine_cores"`
	// Readers, when enabled: point reads of acknowledged keys and collection
	// totals compared with what was acknowledged before the read started.
	Reads         int64   `json:"reads,omitempty"`
	ReadFailed    int64   `json:"read_failed,omitempty"`
	ReadMiss      int64   `json:"read_miss,omitempty"`
	ReadOversized int64   `json:"read_oversized,omitempty"`
	ReadLatency   []int64 `json:"read_latency_us_log2,omitempty"`
	Totals        int64   `json:"totals,omitempty"`
	TotalsExact   int64   `json:"totals_exact,omitempty"`
	TotalsDeficit int64   `json:"totals_deficit,omitempty"`
	TotalLatency  []int64 `json:"total_latency_us_log2,omitempty"`
	// Freshness probe: after a probe write is acknowledged, how long until
	// the collection's root total (the summary tree, deep=false) counts it.
	FreshLag      []int64 `json:"fresh_lag_us_log2,omitempty"`
	FreshTimeouts int64   `json:"fresh_timeouts,omitempty"`
}

type acknowledgedKey struct {
	key
	owner string
}

// recent keeps the last acknowledged keys and their appliers for point reads,
// and per-collection acknowledged counts for the totals check.
type recent struct {
	mu    sync.RWMutex
	keys  []acknowledgedKey
	next  int
	full  bool
	count map[string]int64
}

func (r *recent) add(k acknowledgedKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys == nil {
		r.keys, r.count = make([]acknowledgedKey, 1<<16), map[string]int64{}
	}
	r.keys[r.next] = k
	r.next = (r.next + 1) % len(r.keys)
	r.full = r.full || r.next == 0
	r.count[k.Collection]++
}

func (r *recent) pick(rng *rand.Rand) (acknowledgedKey, int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.next
	if r.full {
		n = len(r.keys)
	}
	if n == 0 {
		return acknowledgedKey{}, 0, false
	}
	k := r.keys[rng.IntN(n)]
	return k, r.count[k.Collection], true
}

func load(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	targets := fs.String("targets", "", "comma-separated node p2p URLs; each write goes to a random one")
	issuer := fs.String("issuer", "", "issuer URL")
	name := fs.String("scenario", "normal", "normal, spike, steps, hot or deep")
	rate := fs.Float64("rate", 100, "offered writes per second (spike: base rate)")
	ramp := fs.Float64("ramp", 0, "when > 0, add this many writes/s every ramp-every (strong scaling)")
	rampEvery := fs.Duration("ramp-every", 30*time.Second, "ramp step")
	duration := fs.Duration("duration", time.Minute, "load duration")
	workers := fs.Int("workers", 128, "concurrent requests at most")
	drainTimeout := fs.Duration("drain-timeout", 2*time.Minute, "maximum time to drain admitted writes after the offered load stops; 0 waits indefinitely")
	collections := fs.Int("collections", 256, "collections for uniform keys")
	spikeStart := fs.Duration("spike-start", time.Minute, "spike scenario: start")
	spikeLength := fs.Duration("spike-length", 5*time.Minute, "spike scenario: length")
	spikeFactor := fs.Float64("spike-factor", 10, "spike scenario: offered rate multiplier inside the window")
	// steps: an elasticity test as others run it (DynamoDB: up to 2x the
	// previous peak is absorbed at once, faster growth is spread over
	// minutes). Uniform keys; the rate is multiplied by step-factor over
	// step-ramp, then held for step-plateau, step-count times.
	stepFactor := fs.Float64("step-factor", 2, "steps scenario: rate multiplier per step")
	stepRamp := fs.Duration("step-ramp", time.Minute, "steps scenario: linear ramp to the next level")
	stepPlateau := fs.Duration("step-plateau", 2*time.Minute, "steps scenario: time held at each level")
	// ramp (RAMP-1): uniform keys, the rate doubles every double-every after
	// ramp-hold; ramp-share of each doubling is linear, the rest one jump.
	// With stop-below the load ends at the knee instead of at -duration.
	doubleEvery := fs.Duration("double-every", time.Minute, "ramp scenario: the rate doubles this often")
	rampShare := fs.Float64("ramp-share", 1, "ramp scenario: share of each doubling done linearly (1 continuous, 0 instant 2x)")
	rampHold := fs.Duration("ramp-hold", 30*time.Second, "ramp scenario: time held at the base rate first")
	rampMax := fs.Float64("ramp-max", 0, "ramp scenario: offered rate never above this; 0 no ceiling")
	stopBelow := fs.Float64("stop-below", 0, "when > 0, stop once fewer than this share of the writes offered over stop-for were acknowledged")
	stopFor := fs.Duration("stop-for", 20*time.Second, "window of the stop-below rule")
	seed := fs.Uint64("seed", 1, "workload seed")
	tag := fs.String("tag", "", "collection prefix for this run, 1-6 identifier characters; default from the clock")
	out := fs.String("out", "", "output directory")
	readers := fs.Int("readers", 0, "concurrent readers while writes run; 0 disables")
	readRate := fs.Float64("read-rate", 0, "total reads per second across readers")
	totalShare := fs.Float64("total-share", 0.2, "share of reads that ask a collection total instead of one key")
	startAt := fs.Int64("start-at", 0, "unix milliseconds to start at, so several load machines start their steps together; 0 starts now")
	idPrefix := fs.String("id-prefix", "", "prefix of every item id, distinct per load machine so their keys never collide")
	stacksAt := fs.Duration("stacks-at", 0, "when > 0, write this process's goroutine stacks to DIR/stacks.txt that long into the run")
	probeEvery := fs.Duration("probe-every", 0, "when > 0, a freshness probe writes one item to its own collection this often and times until the root total counts it")
	routeMode := fs.String("route", "relay", "relay: write through any entry, which relays to the owner; direct: write to the owner learned from earlier answers (X-Indexus-Owner), through any entry while none is known")
	discoverEvery := fs.Duration("discover-every", 0, "when > 0, learn entry points from the nodes' neighbours this often and spread writes over every live one, as a client entering the network anywhere would; 0 writes through -targets only")
	_ = fs.Parse(args)
	nodes := split(*targets)
	fmt.Fprintf(os.Stderr, "targets (slow_by_node index order): %s\n", strings.Join(nodes, " "))
	if len(nodes) == 0 || *issuer == "" || *out == "" {
		return fmt.Errorf("load needs -targets, -issuer and -out")
	}
	if *tag == "" {
		*tag = randomLocation(rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), *seed)), "", 5)
	}
	fmt.Fprintf(os.Stderr, "workload: scenario=%s seed=%d tag=%s id_prefix=%q collections=%d\n", *name, *seed, *tag, *idPrefix, *collections)
	gen, err := makeScenario(*name, *tag, *collections, *spikeStart, *spikeLength, *duration)
	if err != nil {
		return err
	}
	tok, err := token(strings.TrimRight(*issuer, "/"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	pool := newEntries(nodes)
	if *discoverEvery > 0 {
		go pool.discover(ctx, tok, *discoverEvery, *seed)
		defer func() {
			fmt.Fprintf(os.Stderr, "entries learned (slow_by_node index order): %s\n", strings.Join(pool.list(), " "))
		}()
	}
	ackedFile, err := os.Create(filepath.Join(*out, "acked.jsonl"))
	if err != nil {
		return err
	}
	defer ackedFile.Close()
	acked := bufio.NewWriterSize(ackedFile, 1<<20)
	defer acked.Flush()
	report, err := os.Create(filepath.Join(*out, "load.jsonl"))
	if err != nil {
		return err
	}
	defer report.Close()

	type job struct {
		k   key
		hot bool
		due time.Time // scheduled arrival
	}
	jobs := make(chan job, *workers*4)
	readLoad := *readers > 0 && *readRate > 0
	var (
		offered, shed, ackedN, hotAcked, inFlight, exhausted         atomic.Int64
		latency, intended, httpAttempt, routeLearn, exhaustedLatency histogram
		mu                                                           sync.Mutex
		failed                                                       = map[int]int64{}
		errs                                                         = map[string]int{}
		seen                                                         recent
		reads, readFailed, readMiss, readOver                        atomic.Int64
		totals, totalsExact, totalsDeficit                           atomic.Int64
		readLatency, totalLatency                                    histogram
		freshLag                                                     histogram
		freshTimeouts                                                atomic.Int64
	)
	slow := make([]atomic.Int64, maxEntries)
	fail := func(status int, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			msg := err.Error()
			if len(msg) > 80 {
				msg = msg[:80]
			}
			errs[msg]++
		} else {
			failed[status]++
		}
	}
	// route=direct: owners learned from relayed writes (routes.go).
	direct, learned := *routeMode == "direct", &routes{}
	if *routeMode != "relay" && !direct {
		return fmt.Errorf("-route %q: relay or direct", *routeMode)
	}
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(*seed, uint64(w)+1000))
			for j := range jobs {
				if requestCtx.Err() != nil {
					exhausted.Add(1)
					return
				}
				inFlight.Add(1)
				body, _ := json.Marshal(map[string]any{
					"item": map[string]any{"request_id": "exp-" + j.k.Collection + "-" + j.k.ID, "collection": j.k.Collection, "location": j.k.Location, "id": j.k.ID, "metrics": []float64{1}},
					"root": "@",
				})
				pick, target := pool.pick(rng)
				var id indexus.ID
				var idErr error
				if direct {
					id, idErr = indexus.ZoneID(j.k.Collection, j.k.Location)
					if idErr == nil {
						if owner, known := learned.owner(id); known {
							if i := pool.index(owner); i >= 0 {
								pick, target = i, owner
							}
						}
					}
				}
				started := time.Now()
				var owner string
				ok := false
				// A refusal during a handoff is retried with the same request
				// id, so a retry never writes twice.
				for attempt := 0; attempt < 5 && !ok && requestCtx.Err() == nil; attempt++ {
					req, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, target+"/item", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+tok)
					attemptStart := time.Now()
					res, err := client.Do(req)
					if err != nil {
						httpAttempt.observe(time.Since(attemptStart))
						fail(0, err)
						pool.down(pick)
						learned.forget(target)
						time.Sleep(200 * time.Millisecond)
						pick, target = pool.pick(rng)
						continue
					}
					_, _ = io.Copy(io.Discard, res.Body)
					res.Body.Close()
					httpAttempt.observe(time.Since(attemptStart))
					if res.StatusCode == http.StatusCreated {
						owner = res.Header.Get("X-Indexus-Owner")
						if owner == "" {
							owner = target
						}
						if direct {
							learnStart := time.Now()
							learned.learn(res.Header.Get("X-Indexus-Owner"), res.Header.Get("X-Indexus-Owner-Cell"))
							if idErr == nil {
								learned.learnZone(id, owner)
							}
							routeLearn.observe(time.Since(learnStart))
						}
						ok = true
						break
					}
					fail(res.StatusCode, nil)
					if res.StatusCode != http.StatusServiceUnavailable && res.StatusCode != http.StatusTooManyRequests {
						break
					}
					time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
				}
				inFlight.Add(-1)
				if !ok {
					exhaustedLatency.observe(time.Since(started))
					exhausted.Add(1)
				}
				if ok {
					took := time.Since(started)
					latency.observe(took)
					intended.observe(time.Since(j.due))
					if took >= 250*time.Millisecond {
						slow[pick].Add(1)
					}
					ackedN.Add(1)
					if j.hot {
						hotAcked.Add(1)
					}
					if readLoad {
						seen.add(acknowledgedKey{key: j.k, owner: owner})
					}
					line, _ := json.Marshal(j.k)
					mu.Lock()
					acked.Write(line)
					acked.WriteByte('\n')
					mu.Unlock()
				}
			}
		}(w)
	}

	readCtx, stopReads := context.WithCancel(ctx)
	var readerGroup sync.WaitGroup
	if *probeEvery > 0 {
		// One probe at a time, in a collection only this loader writes, so
		// the expected total is exact: n acknowledged probe writes.
		probe := fmt.Sprintf("%sp%0*d", *tag, 15-len(*tag), *seed%1_000_000_000)
		readerGroup.Add(1)
		go func() {
			defer readerGroup.Done()
			rng := rand.New(rand.NewPCG(*seed, 9000))
			var n int64
			var k key
			tick := time.NewTicker(*probeEvery)
			defer tick.Stop()
			for {
				select {
				case <-readCtx.Done():
					return
				case <-tick.C:
				}
				// An earlier attempt may have committed and lost its response.
				// Keep the location and request ID together until that probe is acknowledged.
				if k.ID == "" {
					k = key{Collection: probe, Location: randomLocation(rng, "", 16), ID: fmt.Sprint(*idPrefix, "p", n)}
				}
				body, _ := json.Marshal(map[string]any{
					"item": map[string]any{"request_id": "exp-" + k.Collection + "-" + k.ID, "collection": k.Collection, "location": k.Location, "id": k.ID, "metrics": []float64{1}},
					"root": "@",
				})
				req, _ := http.NewRequest(http.MethodPost, pool.url(rng)+"/item", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+tok)
				res, err := client.Do(req)
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != http.StatusCreated {
					continue
				}
				n++
				line, _ := json.Marshal(k)
				mu.Lock()
				acked.Write(line)
				acked.WriteByte('\n')
				mu.Unlock()
				k = key{}
				ackedAt := time.Now()
				for {
					if count, err := collectionTotal(pool.url(rng), tok, probe, false); err == nil && count >= n {
						freshLag.observe(time.Since(ackedAt))
						break
					}
					if time.Since(ackedAt) > 30*time.Second || readCtx.Err() != nil {
						freshTimeouts.Add(1)
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
	}
	if readLoad {
		pace := time.Duration(float64(time.Second) * float64(*readers) / *readRate)
		for r := 0; r < *readers; r++ {
			readerGroup.Add(1)
			go func(r int) {
				defer readerGroup.Done()
				rng := rand.New(rand.NewPCG(*seed, uint64(r)+5000))
				tick := time.NewTicker(pace)
				defer tick.Stop()
				for {
					select {
					case <-readCtx.Done():
						return
					case <-tick.C:
					}
					k, acked, ok := seen.pick(rng)
					if !ok {
						continue
					}
					started := time.Now()
					if rng.Float64() < *totalShare {
						count, err := collectionTotalContext(readCtx, pool.url(rng), tok, k.Collection, false)
						if err != nil {
							readFailed.Add(1)
							continue
						}
						totals.Add(1)
						totalLatency.observe(time.Since(started))
						if count >= acked {
							totalsExact.Add(1)
						} else {
							totalsDeficit.Add(acked - count)
						}
						continue
					}
					found, status, err := readPoint(readCtx, k, tok, func() string { return pool.url(rng) })
					reads.Add(1)
					switch {
					case status == http.StatusRequestEntityTooLarge:
						readOver.Add(1)
					case err != nil:
						readFailed.Add(1)
					case !found:
						readMiss.Add(1)
					default:
						readLatency.observe(time.Since(started))
					}
				}
			}(r)
		}
	}

	// Several load machines share one schedule: each waits for the agreed
	// instant, so their offered rates add up step by step (LOAD-SYNC).
	if *startAt > 0 {
		time.Sleep(time.Until(time.UnixMilli(*startAt)))
	}
	// Open-loop arrivals: the schedule does not wait for answers, so overload
	// shows as shed arrivals and growing latency, not as a slower client.
	start := time.Now()
	if *stacksAt > 0 {
		time.AfterFunc(*stacksAt, func() {
			if f, err := os.Create(filepath.Join(*out, "stacks.txt")); err == nil {
				_ = pprof.Lookup("goroutine").WriteTo(f, 1)
				f.Close()
			}
		})
	}
	rng := rand.New(rand.NewPCG(*seed, 7))
	var i int64
	var due float64
	lastArrivalTick := start
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	nextReport := start.Add(time.Second)
	var last struct {
		offered, shed, acked, hot, exhausted int64
		cpu                                  float64
	}
	current := *rate
	stop := &knee{below: *stopBelow, window: int(*stopFor / time.Second)}
	enc := json.NewEncoder(report)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-tick.C:
			t := now.Sub(start)
			finished := t >= *duration
			if finished {
				t = *duration
				now = start.Add(t)
			}
			current = *rate
			if *ramp > 0 {
				current += *ramp * float64(int(t / *rampEvery))
			}
			if *name == "spike" && t >= *spikeStart && t < *spikeStart+*spikeLength {
				current *= *spikeFactor
			}
			if *name == "steps" {
				current *= stepLevel(t, *stepFactor, *stepRamp, *stepPlateau)
			}
			if *name == "ramp" {
				current *= rampLevel(t, *doubleEvery, *rampHold, *rampShare)
				if *rampMax > 0 {
					current = min(current, *rampMax)
				}
			}
			due += current * now.Sub(lastArrivalTick).Seconds()
			lastArrivalTick = now
			for ; due >= 1; due-- {
				k, hot := gen(rng, i, t)
				k.ID = *idPrefix + k.ID
				i++
				offered.Add(1)
				select {
				case jobs <- job{k, hot, now.Add(-time.Duration((due - 1) / current * float64(time.Second)))}:
				default:
					shed.Add(1)
				}
			}
			if finished {
				break loop
			}
			if now.After(nextReport) {
				nextReport = nextReport.Add(time.Second)
				mu.Lock()
				f, e := failed, errs
				failed, errs = map[int]int64{}, map[string]int{}
				mu.Unlock()
				o, s, a, h, x := offered.Load(), shed.Load(), ackedN.Load(), hotAcked.Load(), exhausted.Load()
				cpu := processCPU()
				line := second{CPU: cpu - last.cpu, Cores: runtime.NumCPU(), T: t.Seconds(), Rate: current, Offered: o - last.offered, Shed: s - last.shed, Acked: a - last.acked, HotAcked: h - last.hot, Failed: f, Errors: e, Latency: latency.drain(), HTTPAttempt: httpAttempt.drain(), RouteLearn: routeLearn.drain(), Intended: intended.drain(), InFlight: inFlight.Load(), Queued: len(jobs), Workers: *workers,
					Exhausted: x - last.exhausted, ExhaustedLatency: exhaustedLatency.drain(),
					Reads: reads.Swap(0), ReadFailed: readFailed.Swap(0), ReadMiss: readMiss.Swap(0), ReadOversized: readOver.Swap(0), ReadLatency: readLatency.drain(),
					Totals: totals.Swap(0), TotalsExact: totalsExact.Swap(0), TotalsDeficit: totalsDeficit.Swap(0), TotalLatency: totalLatency.drain(),
					FreshLag: freshLag.drain(), FreshTimeouts: freshTimeouts.Swap(0), Entries: pool.count()}
				for i := range slow {
					if v := slow[i].Swap(0); v > 0 {
						if line.SlowByNode == nil {
							line.SlowByNode = map[int]int64{}
						}
						line.SlowByNode[i] = v
					}
				}
				_ = enc.Encode(line)
				last.offered, last.shed, last.acked, last.hot, last.exhausted, last.cpu = o, s, a, h, x, cpu
				if stop.add(line.Offered, line.Acked) {
					fmt.Fprintf(os.Stderr, "knee: fewer than %.0f%% of the writes offered over %s acknowledged at %.0f/s, stopping at t=%.0fs\n", 100**stopBelow, *stopFor, current, t.Seconds())
					_ = os.WriteFile(filepath.Join(*out, "knee.json"), fmt.Appendf(nil, `{"t":%.1f,"offered_rate":%.0f,"stop_below":%g,"stop_for_s":%.0f}`+"\n", t.Seconds(), current, *stopBelow, stopFor.Seconds()), 0o644)
					break loop
				}
				fmt.Fprintf(os.Stderr, "t=%4.0fs rate=%6.0f acked/s=%6d shed/s=%5d inflight=%4d cpu=%.1f/%d reads/s=%5d miss=%d totals/s=%d exact=%d\n", t.Seconds(), current, line.Acked, line.Shed, line.InFlight, line.CPU, line.Cores, line.Reads, line.ReadMiss, line.Totals, line.TotalsExact)
			}
		}
	}
	stopReads()
	readerGroup.Wait()
	close(jobs)
	drained := drainWorkers(&wg, cancelRequests, *drainTimeout)
	summary, _ := json.Marshal(map[string]any{"offered": offered.Load(), "acked": ackedN.Load(), "shed": shed.Load(), "exhausted": exhausted.Load(), "queued_unfinished": len(jobs), "drain_completed": drained})
	if err := os.WriteFile(filepath.Join(*out, "load-summary.json"), append(summary, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "offered=%d acked=%d shed=%d in %s\n", offered.Load(), ackedN.Load(), shed.Load(), time.Since(start).Round(time.Second))
	return nil
}

// drainWorkers bounds the harness wait; cancellation affects client requests,
// never reopens a durable fence or changes a record already committed by a node.
func drainWorkers(wg *sync.WaitGroup, cancel context.CancelFunc, limit time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	if limit <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		cancel()
		<-done
		return false
	}
}

// ---------------------------------------------------------------- observe

func observe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	monitors := fs.String("monitors", "", "comma-separated monitoring URLs")
	every := fs.Duration("every", 2*time.Second, "sample period")
	duration := fs.Duration("duration", 10*time.Minute, "observation length")
	out := fs.String("out", "", "output directory")
	monitorsFile := fs.String("monitors-file", "", "file of monitoring URLs, one per line or comma-separated, read again at every sample: machines started during the run are observed too")
	_ = fs.Parse(args)
	nodes := split(*monitors)
	if len(nodes) == 0 && *monitorsFile == "" || *out == "" {
		return fmt.Errorf("observe needs -monitors or -monitors-file, and -out")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(*out, "metrics.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	var mu sync.Mutex
	start := time.Now()
	tick := time.NewTicker(*every)
	defer tick.Stop()
	// Each node is sampled on its own: a node that does not answer is not
	// asked again until it does, and the others go on (2026-11-20: rounds
	// that waited for every node made one stalled node look like 32). took_ms
	// is how long /metrics took; past stallAfter the node's goroutine stacks
	// are saved once per stall (stacks/<node>-<t>.txt), naming what it waits on.
	const stallAfter = 2 * time.Second
	busy := map[string]bool{}
	stacks := filepath.Join(*out, "stacks")
	lastStacks := map[string]time.Time{}
	sample := func(node string) {
		at := time.Now()
		done := make(chan struct{})
		go func() {
			select {
			case <-done:
			case <-time.After(stallAfter):
				saveNodeStacks(stacks, node, at.Sub(start))
			}
		}()
		line := map[string]any{"monitor": node, "wall": time.Now().Unix()}
		res, err := client.Get(node + "/metrics")
		close(done)
		line["t"] = time.Since(start).Seconds()
		line["took_ms"] = time.Since(at).Milliseconds()
		if err != nil {
			line["error"] = err.Error()
		} else {
			var body map[string]any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				line["error"] = err.Error()
			}
			res.Body.Close()
			line["metrics"] = body
			// A transfer can stall while /metrics itself stays fast. Capture
			// stacks for an old operation too, at most once a minute/node.
			handoffs, _ := body["handoffs"].(map[string]any)
			open, _ := handoffs["open"].(map[string]any)
			oldest, _ := open["oldest_seconds"].(float64)
			mu.Lock()
			capture := oldest >= 30 && time.Since(lastStacks[node]) >= time.Minute
			if capture {
				lastStacks[node] = time.Now()
			}
			mu.Unlock()
			if capture {
				go saveNodeStacks(stacks, node, at.Sub(start))
			}
		}
		raw, _ := json.Marshal(line)
		mu.Lock()
		file.Write(append(raw, '\n'))
		busy[node] = false
		mu.Unlock()
	}
	for {
		if *monitorsFile != "" {
			if raw, err := os.ReadFile(*monitorsFile); err == nil {
				if listed := split(strings.ReplaceAll(string(raw), "\n", ",")); len(listed) > 0 {
					nodes = listed
				}
			}
		}
		for _, node := range nodes {
			mu.Lock()
			skip := busy[node]
			busy[node] = true
			mu.Unlock()
			if !skip {
				go sample(node)
			}
		}
		if time.Since(start) >= *duration {
			time.Sleep(stallAfter) // the last samples land
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// saveNodeStacks writes the goroutine stacks of the node behind a
// monitoring URL (INDEXUS_DEBUG_STACKS=1, set by the agent).
func saveNodeStacks(dir, node string, at time.Duration) {
	quick := &http.Client{Timeout: 20 * time.Second, Transport: client.Transport}
	res, err := quick.Get(node + "/debug/stacks")
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	name := strings.NewReplacer("https://", "", "http://", "", ":", "_", "/", "_").Replace(node)
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%04.0f.txt", name, at.Seconds())), body, 0o644)
}

// ---------------------------------------------------------------- audit

type auditReport struct {
	Acked         int    `json:"acked_keys"`
	Checked       int    `json:"checked_keys"`
	Missing       int    `json:"missing"`
	MissingSample []key  `json:"missing_sample,omitempty"`
	ReadErrors    int    `json:"read_errors"`
	Incomplete    bool   `json:"incomplete,omitempty"`
	Canceled      string `json:"canceled,omitempty"`
	// ReadErrorSample: the first errors, to tell a timeout from a refusal.
	ReadErrorSample []string `json:"read_error_sample,omitempty"`
	Oversized       int      `json:"oversized_locations"`
	ItemsMatch      bool     `json:"items_match_acked"`
	// Exact per-collection counts (deep reads) against acknowledged keys.
	Collections      int                 `json:"collections_checked"`
	CountMismatches  map[string][2]int64 `json:"count_mismatches,omitempty"`
	TotalErrors      map[string]string   `json:"total_errors,omitempty"`
	SettlementError  string              `json:"settlement_error,omitempty"`
	FinalRetries     int                 `json:"final_snapshot_retries,omitempty"`
	LastFinalRetry   string              `json:"last_final_retry,omitempty"`
	SettlementPolls  int                 `json:"settlement_polls"`
	SettlementWaitMS int64               `json:"settlement_wait_ms"`
	// For the mismatched collections only: acknowledged keys read back one by
	// one through every node, and those found on none (lost writes).
	MismatchChecked    int      `json:"mismatch_keys_checked,omitempty"`
	MismatchLost       int      `json:"mismatch_keys_lost,omitempty"`
	MismatchLostSample []key    `json:"mismatch_lost_sample,omitempty"`
	Zones              int      `json:"zones"`
	DoubleOwned        int      `json:"double_owned_zones"`
	DoubleSample       []string `json:"double_owned_sample,omitempty"`
	// Ownership asked again while transfers were open, and the open
	// handoffs and frozen transfers at the last ask.
	OwnershipRetries  int            `json:"ownership_retries"`
	MovingAtOwnership int            `json:"moving_at_ownership"`
	ItemsHeld         int64          `json:"items_held"`
	Baseline          int64          `json:"items_before_run"`
	NodesAnswering    int            `json:"nodes_answering"`
	NodesGone         int            `json:"nodes_gone"`
	PerNodeItems      map[string]int `json:"per_node_items"`
	Pass              bool           `json:"pass"`
}

func audit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	targets := fs.String("targets", "", "comma-separated node p2p URLs")
	monitors := fs.String("monitors", "", "comma-separated monitoring URLs")
	monitorFile := fs.String("monitors-file", "", "monitor candidates, one per line or comma-separated, reread throughout final verification")
	issuer := fs.String("issuer", "", "issuer URL")
	sample := fs.Int("sample", 20000, "acknowledged keys to read back; 0 reads all")
	baseline := fs.Int64("baseline", 0, "items the network held before this run's writes")
	out := fs.String("out", "", "directory holding acked.jsonl; audit.json is written there")
	settle := fs.Duration("settle", 5*time.Minute, "deadline for final discovery, ownership, exact totals and their verification; 0 makes one pass")
	_ = fs.Parse(args)
	nodes, mons := split(*targets), split(*monitors)
	if len(nodes) == 0 || (len(mons) == 0 && *monitorFile == "") || *issuer == "" || *out == "" {
		return fmt.Errorf("audit needs -targets, -monitors, -issuer and -out")
	}
	// Keep the whole candidate pool across readback; more slots may start.
	pool := auditMonitorPool{candidates: mons, file: *monitorFile}
	if err := pool.refresh(); err != nil {
		return err
	}
	nodes, pool.seen = live(ctx, nodes, "/join-hint"), live(ctx, pool.candidates, "/health")
	if len(nodes) == 0 {
		return fmt.Errorf("no node answers")
	}
	tok, err := tokenContext(ctx, strings.TrimRight(*issuer, "/"))
	if err != nil {
		return err
	}
	allKeys, ackedBy, err := readAuditLedger(filepath.Join(*out, "acked.jsonl"))
	if err != nil {
		return fmt.Errorf("read ACK ledger: %w", err)
	}
	report := auditReport{Acked: len(allKeys), PerNodeItems: map[string]int{}}
	keys := allKeys
	rng := rand.New(rand.NewPCG(9, 9))
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	if *sample > 0 && len(keys) > *sample {
		keys = keys[:*sample]
	}
	// Checked is incremented only when a dispatched read completes.
	// Every acknowledged key must be readable through any node.
	var mu sync.Mutex
	work := make(chan key)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for k := range work {
				if ctx.Err() != nil {
					return
				}
				// A node that left (a retired vnode exits after its grace) refuses
				// connections: the key is read through the next one.
				start := w + len(k.ID)
				found, err := readBack(ctx, nodes[start%len(nodes)], tok, k)
				for j := 1; j < len(nodes) && err != nil && strings.Contains(err.Error(), "connection refused"); j++ {
					found, err = readBack(ctx, nodes[(start+j)%len(nodes)], tok, k)
				}
				mu.Lock()
				report.Checked++
				switch {
				case errors.Is(err, errOversized):
					// One location above the read budget: not readable key by key,
					// covered by the held-items total instead.
					report.Oversized++
				case err != nil:
					report.ReadErrors++
					if len(report.ReadErrorSample) < 5 {
						report.ReadErrorSample = append(report.ReadErrorSample, err.Error())
					}
				case !found:
					report.Missing++
					if len(report.MissingSample) < 20 {
						report.MissingSample = append(report.MissingSample, k)
					}
				}
				mu.Unlock()
			}
		}(w)
	}
	// Progress every 5 s: a cut audit still says how far it got and how
	// fast the reads were going.
	started, sent := time.Now(), 0
	next := started.Add(5 * time.Second)
sendReads:
	for _, k := range keys {
		select {
		case work <- k:
		case <-ctx.Done():
			break sendReads
		}
		if sent++; time.Now().After(next) {
			mu.Lock()
			fmt.Fprintf(os.Stderr, "audit %s: %d/%d sent, missing %d, read errors %d\n", time.Since(started).Round(time.Second), sent, len(keys), report.Missing, report.ReadErrors)
			mu.Unlock()
			next = next.Add(5 * time.Second)
		}
	}
	close(work)
	wg.Wait()
	if ctx.Err() != nil {
		report.Incomplete, report.Canceled = true, ctx.Err().Error()
		return writeAuditReport(*out, report)
	}
	fmt.Fprintf(os.Stderr, "audit reads done in %s, %d attempts repeated after a 503\n", time.Since(started).Round(time.Second), readsWaited.Load())
	for _, e := range report.ReadErrorSample {
		fmt.Fprintln(os.Stderr, "audit read error:", e)
	}
	// The readback above checks each ACK. Certify ownership and collection
	// totals together across two observations of the candidate monitor slots.
	expected, err := auditFinal(ctx, &report, pool, nodes, tok, ackedBy, *settle)
	report.Baseline = *baseline
	if err != nil {
		report.SettlementError = err.Error()
		if ctx.Err() != nil {
			report.Incomplete, report.Canceled = true, ctx.Err().Error()
		}
		return writeAuditReport(*out, report)
	}
	// A collection whose exact total differs from what was acknowledged has
	// every one of its acknowledged keys read back, through every node in turn:
	// a key nowhere readable is a lost write; all readable means the total read
	// is what is wrong (AUDIT-2).
	// With a full readback every ACK has already been checked. For a sampled
	// audit, diagnose only mismatched collections, with bounded parallelism.
	if report.Checked < report.Acked && len(report.CountMismatches) > 0 {
		checks := make(chan key)
		var rechecks sync.WaitGroup
		for w := 0; w < 32; w++ {
			rechecks.Add(1)
			go func(w int) {
				defer rechecks.Done()
				for k := range checks {
					if ctx.Err() != nil {
						return
					}
					found := false
					for j := range nodes {
						if ok, err := readBack(ctx, nodes[(w+j)%len(nodes)], tok, k); err == nil && ok {
							found = true
							break
						}
						if ctx.Err() != nil {
							return
						}
					}
					mu.Lock()
					if !found {
						report.MismatchLost++
						if len(report.MismatchLostSample) < 20 {
							report.MismatchLostSample = append(report.MismatchLostSample, k)
						}
					}
					report.MismatchChecked++
					mu.Unlock()
				}
			}(w)
		}
	recheckCollections:
		for _, k := range allKeys {
			if _, mismatched := report.CountMismatches[k.Collection]; !mismatched {
				continue
			}
			select {
			case checks <- k:
			case <-ctx.Done():
				break recheckCollections
			}
		}
		close(checks)
		rechecks.Wait()
	}
	if ctx.Err() != nil {
		report.Incomplete, report.Canceled = true, ctx.Err().Error()
		return writeAuditReport(*out, report)
	}
	report.ItemsMatch = len(report.CountMismatches) == 0 && len(report.TotalErrors) == 0
	report.Pass = report.Acked > 0 && report.Missing == 0 && report.DoubleOwned == 0 && report.ReadErrors == 0 && report.NodesAnswering+report.NodesGone == expected && report.ItemsMatch
	return writeAuditReport(*out, report)
}

// readAuditLedger keeps each key once for both readback and mismatch diagnostics.
func readAuditLedger(path string) ([]key, map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	counts := make(map[string]int64)
	var keys []key
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var k key
		if err := json.Unmarshal(scanner.Bytes(), &k); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", len(keys)+1, err)
		}
		if k.Collection == "" || k.ID == "" {
			return nil, nil, fmt.Errorf("line %d lacks collection or id", len(keys)+1)
		}
		keys = append(keys, k)
		counts[k.Collection]++
	}
	return keys, counts, scanner.Err()
}

var errOversized = errors.New("location above the read budget")

// readBack reads one key back. 503 is a node saying "not now" (a fold or a
// handoff holds a zone frozen for a few seconds): a client waits and asks
// again, so the audit does too, for up to readBackPatience, and counts it.
func readBack(ctx context.Context, node, tok string, k key) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, readBackPatience)
	defer cancel()
	last := ""
	deadline := time.Now().Add(readBackPatience)
	for attempt := 0; attempt < 3 || time.Now().Before(deadline) && strings.Contains(last, "status 503"); attempt++ {
		if attempt > 0 && strings.Contains(last, "status 503") {
			readsWaited.Add(1)
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, node+"/records?limit=1000&collection="+k.Collection+"&location="+k.Location, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if strings.Contains(err.Error(), "connection refused") {
				return false, err
			}
			last = err.Error()
			if err := auditBackoff(ctx); err != nil {
				return false, err
			}
			continue
		}
		var body struct {
			Records []struct {
				ID       string `json:"id"`
				Location string `json:"location"`
			}
			Error string `json:"error"`
		}
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if res.StatusCode == http.StatusRequestEntityTooLarge {
			return false, errOversized
		}
		if res.StatusCode != http.StatusOK || err != nil {
			last = fmt.Sprintf("%s: status %d %s %v", node, res.StatusCode, body.Error, err)
			if err := auditBackoff(ctx); err != nil {
				return false, err
			}
			continue
		}
		for _, r := range body.Records {
			if r.ID == k.ID && r.Location == k.Location {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("unreadable: %s", last)
}

const readBackPatience = 15 * time.Second

// readsWaited counts the read attempts repeated after a 503.
var readsWaited atomic.Int64

// live keeps the URLs that answer path at all (any HTTP status).
func live(ctx context.Context, urls []string, path string) []string {
	quick := &http.Client{Timeout: 2 * time.Second}
	var out []string
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+path, nil)
		if err != nil || ctx.Err() != nil {
			break
		}
		if res, err := quick.Do(req); err == nil {
			res.Body.Close()
			out = append(out, u)
		}
	}
	return out
}

// processCPU is this process's user+system CPU seconds so far.
func processCPU() float64 {
	var u syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &u) != nil {
		return 0
	}
	return float64(u.Utime.Sec+u.Stime.Sec) + float64(u.Utime.Usec+u.Stime.Usec)/1e6
}
