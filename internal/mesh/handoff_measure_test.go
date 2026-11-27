package mesh_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// pacedLink adds a shared request bandwidth budget and a round-trip delay.
// HTTP, JSON and disk work remain real; this is not a WAN deployment.
type pacedLink struct {
	mu             sync.Mutex
	next           time.Time
	bytesPerSecond float64
	rtt            time.Duration
	base           http.RoundTripper
}

func (p *pacedLink) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	start := time.Now()
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(time.Duration(float64(max(r.ContentLength, 0)) / p.bytesPerSecond * float64(time.Second)))
	until := p.next.Add(p.rtt / 2)
	p.mu.Unlock()
	wait := func(d time.Duration) error {
		timer := time.NewTimer(max(d, 0))
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}
	if err := wait(time.Until(until)); err != nil {
		return nil, err
	}
	response, err := p.base.RoundTrip(r)
	if err == nil {
		if e := wait(p.rtt / 2); e != nil {
			response.Body.Close()
			return nil, e
		}
	}
	return response, err
}

type cutoverObservation struct{ FreezeCallUS, FenceToInstallACKUS int64 }
type cutoverLog struct {
	slog.Handler
	mu       sync.Mutex
	rows     []cutoverObservation
	progress map[string]any
	lastLot  time.Time
}

func (h *cutoverLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "zone transfer progress" {
		var zones int64
		phases := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "zones" {
				zones = a.Value.Int64()
			}
			switch a.Key {
			case "freeze", "mirror", "stage", "emit", "install":
				phases[a.Key+"_ms"] = float64(a.Value.Duration()) / float64(time.Millisecond)
			}
			return true
		})
		if zones > 0 {
			h.mu.Lock()
			h.progress = phases
			h.mu.Unlock()
		}
		return nil
	}
	if r.Message != "zone cutover" {
		return nil
	}
	var row cutoverObservation
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "freeze_call_us":
			row.FreezeCallUS = a.Value.Int64()
		case "fence_to_install_ack_us":
			row.FenceToInstallACKUS = a.Value.Int64()
		}
		return true
	})
	h.mu.Lock()
	h.rows = append(h.rows, row)
	h.lastLot = time.Now()
	h.mu.Unlock()
	return nil
}

// Opt-in paired comparison against main, with identical test code and log
// boundaries. NoSync is false. Percentiles describe completed observed lots;
// first receiver ACK and full join time are separate measurements.
// scripts/perf/handoff.py runs the same fixture in a normal Go executable,
// without the storage's test-only full usage scans. Audits below still run.
func TestMeasuredGranularHandoff(t *testing.T) {
	if os.Getenv("INDEXUS_HANDOFF_MEASURE") != "1" {
		t.Skip("set INDEXUS_HANDOFF_MEASURE=1")
	}
	if os.Getenv("INDEXUS_MEASURE_PRODUCTION") == "1" && testing.Testing() {
		t.Fatal("production measurement requires scripts/perf/handoff.py")
	}
	integer := func(key string, fallback int) int {
		value := os.Getenv(key)
		if value == "" {
			return fallback
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			t.Fatalf("invalid %s", key)
		}
		return n
	}
	count, payloadBytes := integer("INDEXUS_MEASURE_RECORDS", 4096), integer("INDEXUS_MEASURE_PAYLOAD", 8192)
	rtt, mibps := integer("INDEXUS_MEASURE_RTT_MS", 2), integer("INDEXUS_MEASURE_MIBPS", 50)
	if count > 262144 || payloadBytes > 1<<20 {
		t.Fatal("measurement size exceeds local fixture bounds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	oldLogger := slog.Default()
	logs := &cutoverLog{Handler: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})}
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	link := &pacedLink{base: base, rtt: time.Duration(rtt) * time.Millisecond, bytesPerSecond: float64(mibps << 20)}
	peers := &meshhttp.Client{HTTP: &http.Client{Transport: link}}
	var nodes [2]*mesh.Service
	var firstInstall atomic.Bool
	var firstACK atomic.Int64
	firstWriteDone := make(chan error, 1)
	measureCtx := ctx
	epoch := time.Now()
	var joinOffset atomic.Int64
	acked := map[string]bool{}
	var ackMu sync.Mutex
	for i := range 2 {
		index := i
		server := httptest.NewServer(&meshhttp.Handler{Mesh: func(ctx context.Context, request mesh.Request) (mesh.Response, error) {
			out, err := nodes[index].Handle(ctx, request)
			if index == 1 && request.Kind == "install-zone" && err == nil && firstInstall.CompareAndSwap(false, true) {
				var zone tree.ZoneKey
				if len(request.ZoneTransfers) > 0 {
					zone = request.ZoneTransfers[0].Zone
				}
				r := storage.Record{Collection: zone.Collection, Location: zone.Location, ID: "early-receiver-ACK", Metrics: []float64{1}}
				go func() {
					answer, e := nodes[1].Write(measureCtx, r.ID, r)
					if e != nil || !answer.AppliedLocally {
						firstWriteDone <- fmt.Errorf("receiver cannot write its first lot: %v", e)
						return
					}
					firstACK.Store((time.Since(epoch).Nanoseconds() - joinOffset.Load()) / 1000)
					ackMu.Lock()
					acked[r.ID] = true
					ackMu.Unlock()
					firstWriteDone <- nil
				}()
			}
			return out, err
		}})
		t.Cleanup(server.Close)
		name := []string{"source0000000000", "target0000000000"}[i]
		store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data"), name, "measure", 1<<33, storage.Options{BatchWrites: true, BatchSize: 256, BatchDelay: 2 * time.Millisecond, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		holder, err := store.Endpoint(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		id := tree.ID{1}
		if i == 1 {
			id, err = tree.ZoneID("Demo000000000000", "a000")
			if err != nil {
				t.Fatal(err)
			}
		}
		nodes[i] = &mesh.Service{Store: store, Self: holder, VNode: tree.VNodeDescriptor{Name: name, ID: id}, Peers: peers}
	}
	genesis := causal.Genesis{Cluster: "measure", Holder: nodes[0].Self}
	for _, node := range nodes {
		if err := node.Store.InitCausal(nodes[0].VNode, genesis); err != nil {
			t.Fatal(err)
		}
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	location := func(i int) string { return string([]byte{'a', alphabet[i/4096%64], alphabet[i/64%64], alphabet[i%64]}) }
	payload := json.RawMessage(`"` + strings.Repeat("x", payloadBytes) + `"`)
	apply := func(id string, i int) error {
		r := storage.Record{Collection: "Demo000000000000", Location: location(i % count), ID: id, Metrics: []float64{1}, Payload: payload}
		_, err := nodes[0].Write(ctx, id, r)
		if err == nil {
			ackMu.Lock()
			acked[id] = true
			ackMu.Unlock()
		}
		return err
	}
	jobs := make(chan int)
	failures := make(chan error, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for i := range jobs {
				if err := apply(fmt.Sprintf("seed-%06d", i), i); err != nil {
					failures <- err
					return
				}
			}
		})
	}
seed:
	for i := range count {
		select {
		case jobs <- i:
		case err := <-failures:
			close(jobs)
			workers.Wait()
			t.Fatal(err)
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			t.Fatal(ctx.Err())
			break seed
		}
	}
	close(jobs)
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	var liveLatencies []float64
	var liveSamples [][2]float64 // completion since Join start, end-to-end ACK latency (ms)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			case <-ctx.Done():
				done <- ctx.Err()
				return
			default:
			}
			started := time.Now()
			for {
				err := apply(fmt.Sprintf("live-%06d", i), i*7919%count)
				if err == nil {
					break
				}
				if ctx.Err() != nil {
					done <- ctx.Err()
					return
				}
				if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) {
					done <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
			elapsed := float64(time.Since(started).Microseconds()) / 1000
			at := float64(time.Since(epoch).Nanoseconds()-joinOffset.Load()) / 1e6
			liveLatencies = append(liveLatencies, elapsed)
			liveSamples = append(liveSamples, [2]float64{at, elapsed})
			time.Sleep(50 * time.Millisecond)
		}
	}()
	joinedAt := time.Now()
	joinOffset.Store(time.Since(epoch).Nanoseconds())
	joinErr := nodes[1].Join(ctx)
	joinedDone := time.Now()
	joinTime := joinedDone.Sub(joinedAt)
	close(stop)
	writeErr := <-done
	if joinErr != nil {
		t.Fatal(joinErr)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	select {
	case err := <-firstWriteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	rows, err := nodes[1].Store.Inventory(false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range rows {
		for _, r := range snap.Records {
			seen[r.ID]++
			if r.ID != "early-receiver-ACK" && string(r.Payload) != string(payload) {
				t.Fatal("payload changed")
			}
		}
	}
	for id := range acked {
		if seen[id] != 1 {
			t.Fatalf("ACK %s observed %d times", id, seen[id])
		}
	}
	for i := range 32 {
		r := storage.Record{Collection: "Demo000000000000", Location: location(i * count / 32), ID: fmt.Sprintf("seed-%06d", i*count/32), Metrics: []float64{1}, Payload: payload}
		if _, _, err := nodes[0].Store.Apply(r.ID, r); err == nil {
			t.Fatal("donor still ACKs transferred data")
		}
		if _, applied, err := nodes[1].Store.Apply(r.ID, r); err != nil || applied {
			t.Fatalf("receiver lost retry receipt: %v applied=%v", err, applied)
		}
	}
	logs.mu.Lock()
	samples := append([]cutoverObservation(nil), logs.rows...)
	progress := logs.progress
	lastLot := logs.lastLot
	logs.mu.Unlock()
	if len(samples) == 0 || firstACK.Load() == 0 {
		t.Fatal("cutover measurement was not exercised")
	}
	stats := func(values []float64) map[string]float64 {
		sort.Float64s(values)
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return map[string]float64{
			"mean_ms": sum / float64(len(values)),
			"p95_ms":  values[(95*len(values)+99)/100-1], "max_ms": values[len(values)-1],
		}
	}
	var fence, whole []float64
	for _, sample := range samples {
		fence = append(fence, float64(sample.FenceToInstallACKUS)/1000)
		whole = append(whole, float64(sample.FreezeCallUS+sample.FenceToInstallACKUS)/1000)
	}
	result := map[string]any{
		"records": count, "payload_bytes": payloadBytes,
		"payload_MiB": float64(count*payloadBytes) / (1 << 20),
		"rtt_ms":      rtt, "link_MiBps": mibps, "fsync": true,
		"full_usage_audit": testing.Testing(), "samples": len(samples),
		"ACKs": len(acked), "observed_rows": len(seen),
		"first_receiver_ACK_ms":      float64(firstACK.Load()) / 1000,
		"join_ms":                    float64(joinTime.Microseconds()) / 1000,
		"after_last_lot_ms":          float64(joinedDone.Sub(lastLot).Microseconds()) / 1000,
		"fence_to_install_ACK":       stats(fence),
		"freeze_call_to_install_ACK": stats(whole),
		"raw_cutovers":               samples, "phase_totals_ms": progress,
	}
	if len(liveLatencies) == 0 {
		t.Fatal("background client did not receive an ACK")
	}
	result["live_write_latency"] = stats(liveLatencies)
	result["raw_live_ACKs_ms"] = liveSamples
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HANDOFF_MEASUREMENT %s", raw)
}
