package indexus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// TestLocalLatencyProfile is opt-in measurement, with real fsync and the same
// workload on either protocol revision. Pairwise RPCs cross a JSON boundary;
// the optional RTT is synthetic, not TCP/QUIC performance. Each offered write
// keeps its scheduled arrival, including queue wait and retries.
func TestLocalLatencyProfile(t *testing.T) {
	if os.Getenv("INDEXUS_LATENCY_PROFILE") != "1" {
		t.Skip("opt-in local latency profile")
	}
	out := os.Getenv("INDEXUS_LATENCY_OUT")
	if out == "" {
		t.Fatal("INDEXUS_LATENCY_OUT is required")
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"same-zone", "distinct-zones", "join"} {
		t.Run(scenario, func(t *testing.T) { runLatencyProfile(t, out, scenario) })
	}
}

type latencyObservation struct {
	Operation  string  `json:"operation"`
	Phase      string  `json:"phase"`
	AtMS       float64 `json:"at_ms"`
	LatencyUS  int64   `json:"latency_us"`
	IntendedUS int64   `json:"intended_us"`
	Attempts   int     `json:"attempts"`
	Error      string  `json:"error,omitempty"`
}

type latencyJSONWire struct {
	inner                             *readPhaseTransport
	rtt                               time.Duration
	target                            string
	targetVNode                       string
	measuring                         atomic.Bool
	calls, sent, received             atomic.Int64
	firstPoint, firstNative, firstACK atomic.Int64
	mu                                sync.Mutex
	byKind                            map[string][3]int64
}

func latencyPause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (w *latencyJSONWire) Exchange(ctx context.Context, host string, request mesh.Request) (mesh.Response, error) {
	if err := latencyPause(ctx, w.rtt/2); err != nil {
		return mesh.Response{}, err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return mesh.Response{}, err
	}
	var decoded mesh.Request
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return mesh.Response{}, err
	}
	if w.measuring.Load() {
		w.calls.Add(1)
		w.sent.Add(int64(len(raw)))
		w.mu.Lock()
		if w.byKind == nil {
			w.byKind = map[string][3]int64{}
		}
		item := w.byKind[request.Kind]
		item[0]++
		item[1] += int64(len(raw))
		w.byKind[request.Kind] = item
		w.mu.Unlock()
	}
	reply, err := w.inner.Exchange(ctx, host, decoded)
	if err != nil {
		return reply, err
	}
	raw, err = json.Marshal(reply)
	if err != nil {
		return mesh.Response{}, err
	}
	if w.measuring.Load() {
		w.received.Add(int64(len(raw)))
		w.mu.Lock()
		item := w.byKind[request.Kind]
		item[2] += int64(len(raw))
		w.byKind[request.Kind] = item
		w.mu.Unlock()
		if host == w.target {
			now := time.Now().UnixNano()
			if request.Kind == "install-zone" {
				w.firstPoint.CompareAndSwap(0, now)
			}
			if request.Kind == "install" {
				w.firstNative.CompareAndSwap(0, now)
			}
			if request.Kind == "write" && reply.AppliedBy.VNode == w.targetVNode {
				w.firstACK.CompareAndSwap(0, now)
			}
		}
	}
	var result mesh.Response
	if err = json.Unmarshal(raw, &result); err != nil {
		return mesh.Response{}, err
	}
	if err = latencyPause(ctx, w.rtt/2); err != nil {
		return mesh.Response{}, err
	}
	return result, nil
}

func latencyQuantiles(values []int64) map[string]any {
	if len(values) == 0 {
		return map[string]any{"n": 0}
	}
	values = append([]int64(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	q := func(p float64) float64 { return float64(values[int(math.Ceil(p*float64(len(values))))-1]) / 1000 }
	var sum int64
	for _, v := range values {
		sum += v
	}
	return map[string]any{"n": len(values), "p50_ms": q(.5), "p95_ms": q(.95), "p99_ms": q(.99), "max_ms": float64(values[len(values)-1]) / 1000, "mean_ms": float64(sum) / float64(len(values)) / 1000}
}

func runLatencyProfile(t *testing.T, out, scenario string) {
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(90*time.Second))
	defer cancel()
	descriptors := []VNodeDescriptor{{ID: ID{255}}}
	if scenario == "join" {
		descriptors = append(descriptors, VNodeDescriptor{ID: ID{128}})
	}
	for i := range descriptors {
		descriptors[i].Name = BASE64.Encode(descriptors[i].ID.Bytes())
	}
	f := newReadPhaseFixture(t, descriptors, false, false) // fsync ON, batch delay 1ms in both revisions
	wire := &latencyJSONWire{inner: f.net}
	if scenario == "join" {
		wire.rtt = time.Millisecond
		wire.target = f.services[1].Self.Host
		wire.targetVNode = descriptors[1].Name
	}
	for _, s := range f.services {
		s.Peers = wire // immutable after setup, before any network operation
		if closer, ok := any(s).(interface{ Close() }); ok {
			t.Cleanup(closer.Close)
		}
	}
	const collection = "0000000000000001"
	payload := json.RawMessage(`{"text":"` + strings.Repeat("x", 1024) + `"}`)
	var locations []string
	if scenario == "join" {
		view := tree.NewView(0)
		for _, d := range descriptors {
			if err := view.Add(d.Name, d.ID); err != nil {
				t.Fatal(err)
			}
		}
		cell, err := tree.Cell(view, descriptors[1].Name)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; len(locations) < 1024; i++ {
			hash := sha256.Sum256([]byte(fmt.Sprint("latency-zone-", i)))
			var id tree.ID
			copy(id[:], hash[:])
			loc := tree.EncodeName(id)
			z, err := tree.NewZoneKey(collection, loc)
			if err != nil {
				t.Fatal(err)
			}
			if cell.Contains(z.ID) {
				locations = append(locations, loc)
			}
		}
		// Setup is excluded from latency distributions; seed one durable row per zone.
		for i, loc := range locations {
			row := storage.Record{Collection: collection, Location: loc, ID: fmt.Sprint("seed-", i), Metrics: []float64{1}, Payload: payload}
			if _, _, _, err := f.stores[0].ApplyRecord(row.ID, row); err != nil {
				t.Fatal(err)
			}
		}
	}
	var mu sync.Mutex
	var observations []latencyObservation
	appendObservation := func(x latencyObservation) { mu.Lock(); observations = append(observations, x); mu.Unlock() }
	began := time.Now()
	var joinStart, joinEnd atomic.Int64
	joined := make(chan error, 1)
	if scenario == "join" {
		go func() {
			if err := latencyPause(ctx, 400*time.Millisecond); err != nil {
				joined <- err
				return
			}
			joinStart.Store(time.Now().UnixNano())
			err := f.services[1].Join(ctx)
			joinEnd.Store(time.Now().UnixNano())
			joined <- err
		}()
	}
	type job struct {
		i   int
		due time.Time
	}
	jobs := make(chan job, 64)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := range jobs {
				started := time.Now()
				loc := "aa"
				if scenario == "distinct-zones" {
					hash := sha256.Sum256([]byte(fmt.Sprint("distinct-", j.i)))
					var id tree.ID
					copy(id[:], hash[:])
					loc = tree.EncodeName(id)
				} else if scenario == "join" {
					loc = locations[j.i%len(locations)]
				}
				row := storage.Record{Collection: collection, Location: loc, ID: fmt.Sprint("write-", j.i), Metrics: []float64{1}, Payload: payload}
				requestCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				attempts := 0
				var err error
				for {
					attempts++
					_, err = f.nodes[0].MeshData(requestCtx, mesh.Request{Kind: "write", RequestID: row.ID, Record: &row})
					if err == nil || requestCtx.Err() != nil {
						break
					}
					if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) {
						break
					}
					if e := latencyPause(requestCtx, 2*time.Millisecond); e != nil {
						break
					}
				}
				stop()
				x := latencyObservation{Operation: "write", AtMS: float64(started.Sub(began).Microseconds()) / 1000, LatencyUS: time.Since(started).Microseconds(), IntendedUS: time.Since(j.due).Microseconds(), Attempts: attempts}
				if err != nil {
					x.Error = err.Error()
				}
				appendObservation(x)
			}
		}()
	}
	var closeJobs sync.Once
	finishWrites := func() { closeJobs.Do(func() { close(jobs) }); workers.Wait() }
	defer func() { cancel(); finishWrites() }()
	wire.measuring.Store(true)
	count, rate := 400, 150
	if scenario == "join" {
		count, rate = 600, 100
	}
	offer := func(first, n int) {
		start := time.Now()
		for i := range n {
			due := start.Add(time.Duration(i) * time.Second / time.Duration(rate))
			if err := latencyPause(ctx, time.Until(due)); err != nil {
				t.Fatal(err)
			}
			select {
			case jobs <- job{first + i, due}:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	offer(0, count)
	if scenario == "join" {
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
		offer(count, 150)
		count += 150 // fixed post-join window, same number on both versions
	}
	finishWrites()
	wire.measuring.Store(false)
	if err := f.nodes[0].Update(); err != nil {
		t.Fatal(err)
	}
	expected := count + len(locations)
	for _, fresh := range []bool{false, true} {
		name := "read-ordinary"
		if fresh {
			name = "read-fresh"
		}
		readCount := 1000
		if scenario == "join" {
			readCount = 300
		}
		for i := 0; i < readCount+10; i++ {
			started := time.Now()
			_, set, err := f.nodes[0].GetSetContext(ctx, collection, "@", true, nil, fresh)
			if err == nil && (set == nil || set.Abelian().Count() != expected) {
				err = fmt.Errorf("read count mismatch: expected %d", expected)
			}
			if i < 10 {
				continue
			}
			x := latencyObservation{Operation: name, Phase: "after", AtMS: float64(started.Sub(began).Microseconds()) / 1000, LatencyUS: time.Since(started).Microseconds(), Attempts: 1}
			if err != nil {
				x.Error = err.Error()
			}
			appendObservation(x)
		}
	}
	groups := map[string][]int64{}
	intended := map[string][]int64{}
	errorCounts := map[string]int{}
	retries := 0
	for i := range observations {
		x := &observations[i]
		if x.Operation == "write" {
			phase := "steady"
			if scenario == "join" {
				at := began.Add(time.Duration(x.AtMS * float64(time.Millisecond))).UnixNano()
				switch {
				case at < joinStart.Load():
					phase = "before"
				case at < joinEnd.Load():
					phase = "during"
				default:
					phase = "after"
				}
			}
			x.Phase = phase
			retries += x.Attempts - 1
		}
		key := x.Operation + "/" + x.Phase
		if x.Error != "" {
			errorCounts[key+": "+x.Error]++
			continue
		}
		groups[key] = append(groups[key], x.LatencyUS)
		if x.Operation == "write" {
			intended[key] = append(intended[key], x.IntendedUS)
		}
	}
	summary := map[string]any{"scenario": scenario, "offered": count, "seed_zones": len(locations), "payload_bytes": 1024, "fsync": true, "batch_delay_ms": 1, "workers": 8, "rate": rate, "configured_rtt_ms": float64(wire.rtt) / float64(time.Millisecond), "errors": errorCounts, "retries": retries, "latency": map[string]any{}, "intended": map[string]any{}, "peer_calls": wire.calls.Load(), "peer_request_bytes": wire.sent.Load(), "peer_reply_bytes": wire.received.Load()}
	wire.mu.Lock()
	summary["peer_by_kind_calls_request_bytes_reply_bytes"] = wire.byKind
	wire.mu.Unlock()
	for k, v := range groups {
		summary["latency"].(map[string]any)[k] = latencyQuantiles(v)
	}
	for k, v := range intended {
		summary["intended"].(map[string]any)[k] = latencyQuantiles(v)
	}
	if scenario == "join" {
		since := func(n int64) any {
			if n == 0 {
				return nil
			}
			return float64(time.Unix(0, n).Sub(time.Unix(0, joinStart.Load())).Microseconds()) / 1000
		}
		summary["join_ms"] = float64(joinEnd.Load()-joinStart.Load()) / float64(time.Millisecond)
		summary["first_point_reply_ms"], summary["first_native_reply_ms"], summary["first_receiver_ack_ms"] = since(wire.firstPoint.Load()), since(wire.firstNative.Load()), since(wire.firstACK.Load())
	}
	file, err := os.Create(filepath.Join(out, scenario+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(file)
	for _, x := range observations {
		if err := enc.Encode(x); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	wire.mu.Lock()
	raw, err := json.MarshalIndent(summary, "", "  ")
	wire.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, scenario+".json"), append(raw, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Log(string(raw))
	if len(errorCounts) > 0 {
		t.Errorf("latency profile detected errors: %v", errorCounts)
	}
}
