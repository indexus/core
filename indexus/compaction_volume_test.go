package indexus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type coldVolumePhase struct {
	Name                                              string
	Seconds, P50MS, P95MS, P99MS, MaxMS, ACKPerSecond float64
	Operations, Active, Prefixes, Rows, Receipts      int
	DiskBytes                                         int64
}
type coldVolumeRetirement struct {
	Source, Target string
	Bytes          int64
	Pieces         int
}
type coldVolumeReport struct {
	StoreCapacityBytes                               int64
	Retirements                                      []coldVolumeRetirement
	ConcurrentReadsExact, ConcurrentReadsUnavailable int
	CachedReadsExact                                 int
	Profile, Status, Transport, Storage              string
	InitialRows, Workers                             int
	Phases                                           []coldVolumePhase
	Trace                                            []int
	PeakHeapBytes                                    uint64
	Errors                                           []string
}
type coldVolumeFixture struct {
	capacity    int64
	net         *readPhaseTransport
	nodes       []*Node
	stores      []*storage.Store
	services    []*mesh.Service
	paths       []string
	descriptors []VNodeDescriptor
}

func (f *coldVolumeFixture) open(t *testing.T) {
	t.Helper()
	f.net = &readPhaseTransport{nodes: map[string]*Node{}}
	f.nodes = nil
	f.stores = nil
	f.services = nil
	for i, d := range f.descriptors {
		settings, err := NewSettings(d.Name, 19000+i, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		node, err := NewNode(settings, testNewContact, nil)
		if err != nil {
			t.Fatal(err)
		}
		store, err := storage.OpenWithOptions(f.paths[i], d.Name, "cold-volume", f.capacity, storage.Options{BatchWrites: true, BatchDelay: 2 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		holder, err := store.Endpoint(fmt.Sprintf("http://127.0.0.1:%d", 19000+i))
		if err != nil {
			t.Fatal(err)
		}
		service := &mesh.Service{Store: store, Self: holder, VNode: d, Peers: f.net}
		f.nodes = append(f.nodes, node)
		f.stores = append(f.stores, store)
		f.services = append(f.services, service)
		f.net.nodes[holder.Host] = node
	}
	genesis := causal.Genesis{Cluster: "cold-volume", Holder: f.services[0].Self}
	for i, store := range f.stores {
		if err := store.InitCausal(f.descriptors[0], genesis); err != nil {
			t.Fatal(err)
		}
		if err := f.nodes[i].EnableCausalCluster(f.services[i]); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *coldVolumeFixture) sync(t *testing.T, ctx context.Context, active int) {
	t.Helper()
	var hosts []string
	for _, s := range f.services[:active] {
		hosts = append(hosts, s.Self.Host)
	}
	for _, s := range f.services[:active] {
		if err := s.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *coldVolumeFixture) diskBytes(t *testing.T) int64 {
	t.Helper()
	var n int64
	for _, path := range f.paths {
		err := filepath.Walk(storage.RecordsDir(path), func(_ string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() {
				n += info.Size()
			}
			return err
		})
		if err != nil {
			t.Fatalf("measure store %s: %v", path, err)
		}
	}
	return n
}

// Opt-in campaign: the ordinary suite keeps its small deterministic fixtures.
// INDEXUS_VOLUME_ROWS=100000 INDEXUS_VOLUME_PROFILE=balanced|adversarial
// INDEXUS_VOLUME_OUT=/absolute/result.json go test ./indexus -run TestLargeDurableColdCampaign -v -timeout=30m
func TestLargeDurableColdCampaign(t *testing.T) {
	t.Parallel()
	raw := os.Getenv("INDEXUS_VOLUME_ROWS")
	if raw == "" {
		t.Skip("set INDEXUS_VOLUME_ROWS for durable volume campaign")
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count < 1000 {
		t.Fatal("INDEXUS_VOLUME_ROWS must be >=1000")
	}
	profile := os.Getenv("INDEXUS_VOLUME_PROFILE")
	if profile == "" {
		profile = "balanced"
	}
	if profile != "balanced" && profile != "adversarial" {
		t.Fatal("unknown profile")
	}
	capacity := int64(1 << 30)
	if raw := os.Getenv("INDEXUS_VOLUME_CAPACITY_BYTES"); raw != "" {
		capacity, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || capacity <= 0 {
			t.Fatal("INDEXUS_VOLUME_CAPACITY_BYTES must be positive")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	report := coldVolumeReport{StoreCapacityBytes: capacity, Profile: profile, InitialRows: count, Workers: 32, Transport: "in-memory real mesh/Node handlers; no simulated WAN", Storage: "zonelog fsync, grouped writes, 2ms max group delay", Trace: []int{1}}
	output := os.Getenv("INDEXUS_VOLUME_OUT")
	save := func() {
		if output != "" {
			b, e := json.MarshalIndent(report, "", "  ")
			if e == nil {
				e = os.WriteFile(output, b, 0600)
			}
			if e != nil {
				t.Errorf("save report: %v", e)
			}
		}
	}
	t.Cleanup(func() {
		report.Status = "PASS"
		if t.Failed() {
			report.Status = "FAIL"
		}
		save()
	})
	f := &coldVolumeFixture{capacity: capacity}
	for i, first := range []byte{16, 48, 80, 112, 144, 176, 208, 240} {
		id := ID{first, 1}
		f.descriptors = append(f.descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
		f.paths = append(f.paths, filepath.Join(t.TempDir(), fmt.Sprintf("node-%d.db", i)))
	}
	f.open(t)
	phase := func(p coldVolumePhase, begin time.Time) {
		p.Seconds = time.Since(begin).Seconds()
		p.DiskBytes = f.diskBytes(t)
		report.Phases = append(report.Phases, p)
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		report.PeakHeapBytes = max(report.PeakHeapBytes, m.HeapAlloc)
		t.Logf("phase=%s seconds=%.2f ops=%d active=%d rows=%d prefixes=%d receipts=%d p95=%.2fms disk=%.1fMiB", p.Name, p.Seconds, p.Operations, p.Active, p.Rows, p.Prefixes, p.Receipts, p.P95MS, float64(p.DiskBytes)/(1<<20))
		save()
	}
	rows := make([]storage.Record, count)
	for i := range rows {
		id := tree.ID{byte(i % 256), byte((i / 256) % 2)}
		location := tree.EncodeName(id)
		if profile == "adversarial" && i%10 != 0 {
			hot := tree.EncodeName(tree.ID{240})
			location = hot[:1+i%3]
		}
		rows[i] = storage.Record{Collection: "0000000000000001", Location: location, ID: fmt.Sprintf("row-%08d-", i) + strings.Repeat("x", 64), Metrics: []float64{1, float64(i%97 - 48), float64(i%11) / 8}}
	}
	type write struct {
		id  string
		row storage.Record
	}
	receipts := map[string]storage.Mutation{}
	var receiptsMu sync.Mutex
	writeBatch := func(name string, jobs []write, active int) {
		t.Helper()
		begin := time.Now()
		latencies := make([]float64, len(jobs))
		queue := make(chan int, 64)
		var wg sync.WaitGroup
		var firstErr error
		var completed atomic.Int64
		var errorMu sync.Mutex
		for w := 0; w < report.Workers; w++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for j := range queue {
					start := time.Now()
					reply, e := f.nodes[worker%active].MeshData(ctx, mesh.Request{Kind: "write", RequestID: jobs[j].id, Record: &jobs[j].row})
					latencies[j] = float64(time.Since(start)) / float64(time.Millisecond)
					if e != nil {
						errorMu.Lock()
						if firstErr == nil {
							firstErr = e
						}
						errorMu.Unlock()
						continue
					}
					receiptsMu.Lock()
					receipts[jobs[j].id] = reply.Mutation
					receiptsMu.Unlock()
					if n := completed.Add(1); n%10000 == 0 {
						t.Logf("phase=%s progress=%d/%d", name, n, len(jobs))
					}
				}
			}(w)
		}
		for j := range jobs {
			queue <- j
		}
		close(queue)
		wg.Wait()
		if firstErr != nil {
			t.Fatal(firstErr)
		}
		sort.Float64s(latencies)
		q := func(p float64) float64 { return latencies[min(len(latencies)-1, int(float64(len(latencies))*p))] }
		phase(coldVolumePhase{Name: name, Operations: len(jobs), Active: active, ACKPerSecond: float64(len(jobs)) / time.Since(begin).Seconds(), P50MS: q(.5), P95MS: q(.95), P99MS: q(.99), MaxMS: q(1)}, begin)
	}
	insert := func(name string, lo, hi, active int) {
		jobs := make([]write, 0, hi-lo)
		for i := lo; i < hi; i++ {
			jobs = append(jobs, write{fmt.Sprintf("seed:%d", i), rows[i]})
		}
		writeBatch(name, jobs, active)
	}
	grow := func(lo, hi int) {
		begin := time.Now()
		for i := lo; i < hi; i++ {
			if e := f.services[i].Join(ctx); e != nil {
				t.Fatal(e)
			}
			f.sync(t, ctx, i+1)
			t.Logf("joined vnode %d/%d", i+1, hi)
		}
		report.Trace = append(report.Trace, hi)
		phase(coldVolumePhase{Name: fmt.Sprintf("grow-%d", hi), Active: hi}, begin)
	}
	insert("insert-at-1", 0, count/4, 1)
	grow(1, 4)
	insert("insert-at-4", count/4, count*3/4, 4)
	grow(4, 8)
	insert("insert-at-8", count*3/4, count, 8)
	var deletes, restores []write
	for i := range rows {
		if i%10 == 0 {
			rows[i].Deleted = true
			deletes = append(deletes, write{fmt.Sprintf("delete:%d", i), rows[i]})
		}
	}
	writeBatch("delete-10pct", deletes, 8)
	for i := range rows {
		if i%20 == 0 {
			rows[i].Deleted = false
			rows[i].Metrics = []float64{2, float64(-i % 97), .5}
			restores = append(restores, write{fmt.Sprintf("restore:%d", i), rows[i]})
		}
	}
	writeBatch("restore-5pct", restores, 8)
	verify := func(name string, active int, allReceipts bool) {
		t.Helper()
		begin := time.Now()
		f.sync(t, ctx, 8)
		type aggregate struct {
			count   int
			metrics [3]float64
		}
		expected := map[Key]*aggregate{}
		wanted := map[string]storage.Record{}
		for _, row := range rows {
			wanted[row.Location+":"+row.ID] = row
			prefixes := []string{"@"}
			for i := 1; i <= len(row.Location); i++ {
				prefixes = append(prefixes, row.Location[:i])
			}
			for _, prefix := range prefixes {
				key := Key{Collection: row.Collection, Location: prefix}
				if expected[key] == nil {
					expected[key] = &aggregate{}
				}
				if !row.Deleted {
					a := expected[key]
					a.count++
					for k, x := range row.Metrics {
						a.metrics[k] += x
					}
				}
			}
		}
		// Query through two independent ingress endpoints, including a retired one.
		for _, ingress := range []int{0, 7} {
			snaps, e := f.services[ingress].Query(ctx)
			if e != nil {
				t.Fatal(e)
			}
			actual := map[string]storage.Record{}
			for _, snap := range snaps {
				for _, r := range snap.Records {
					key := r.Location + ":" + r.ID
					if _, duplicate := actual[key]; duplicate {
						t.Fatal("duplicate authoritative row")
					}
					actual[key] = r
				}
			}
			if !reflect.DeepEqual(actual, wanted) {
				t.Fatalf("%s payload mismatch: got %d want %d", name, len(actual), len(wanted))
			}
		}
		var locations []string
		for key := range expected {
			locations = append(locations, key.Location)
		}
		sort.Strings(locations)
		deadline := time.Now().Add(45 * time.Second)
		mismatches := 0
		for {
			for _, node := range f.nodes {
				if e := node.Update(); e != nil {
					report.Errors = append(report.Errors, e.Error())
				}
			}
			answers := map[string]*Abelian{}
			owners := map[string]int{}
			for _, node := range f.nodes {
				values, e := node.GetAggregates("0000000000000001", locations)
				if e != nil {
					t.Fatal(e)
				}
				for location, a := range values {
					owners[location]++
					answers[location] = a
				}
			}
			mismatches = 0
			for key, want := range expected {
				got := answers[key.Location]
				if owners[key.Location] != 1 {
					mismatches++
					continue
				}
				if got == nil {
					if want.count != 0 {
						mismatches++
					}
					continue
				}
				if got.Count() != want.count {
					mismatches++
					continue
				}
				for k, x := range want.metrics {
					if got.Metric(k) != x {
						mismatches++
						break
					}
				}
			}
			if mismatches == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d incorrect prefix aggregates", name, mismatches)
			}
			time.Sleep(100 * time.Millisecond)
		}
		checkedReceipts := 0
		if allReceipts {
			snapshots, e := f.stores[0].Inventory(true)
			if e != nil {
				t.Fatal(e)
			}
			got := map[string]storage.RequestReceipt{}
			var seq uint64
			for _, snap := range snapshots {
				seq += snap.Seq
				for _, op := range snap.Requests {
					if _, exists := got[op.RequestID]; exists {
						t.Fatal("duplicate receipt")
					}
					got[op.RequestID] = op
				}
			}
			wantReceipts := map[string]storage.RequestReceipt{}
			for id, m := range receipts {
				raw, e := json.Marshal(m.Record)
				if e != nil {
					t.Fatal(e)
				}
				wantReceipts[id] = storage.RequestReceipt{Digest: sha256.Sum256(raw), RequestID: id, Seq: m.Seq, Collection: m.Record.Collection, Location: m.Record.Location}
			}
			if !reflect.DeepEqual(got, wantReceipts) {
				t.Fatalf("%s receipts changed: got %d want %d", name, len(got), len(receipts))
			}
			if seq != uint64(len(receipts)) {
				t.Fatalf("sequence coverage got %d want %d", seq, len(receipts))
			}
			checkedReceipts = len(got)
		}
		live := 0
		for _, r := range rows {
			if !r.Deleted {
				live++
			}
		}
		phase(coldVolumePhase{Name: name, Active: active, Rows: live, Prefixes: len(expected), Receipts: checkedReceipts}, begin)
	}
	verify("verify-before-cold", 8, false)
	expectedDuringCold := map[string]storage.Record{}
	for _, row := range rows {
		expectedDuringCold[row.Location+":"+row.ID] = row
	}
	// Warm a full result before cold work. Cache guarantees are checked
	// separately from strict raw inventories; neither replaces the other oracle.
	var liveCount int
	var totals [3]float64
	for _, row := range rows {
		if !row.Deleted {
			liveCount++
			for k, x := range row.Metrics {
				totals[k] += x
			}
		}
	}
	checkCached := func() {
		out, e := f.nodes[7].ReadContext(ctx, "0000000000000001", "@", ReadOptions{Mode: ReadCached, MaxAge: 30 * time.Minute})
		if e != nil || out.Set == nil || out.Set.Count() != liveCount {
			t.Fatalf("cold cached read: %+v %v", out, e)
		}
		for k, x := range totals {
			if out.Set.Abelian().Metric(k) != x {
				t.Fatalf("cold cached metric %d differs", k)
			}
		}
		report.CachedReadsExact++
	}
	if _, e := f.nodes[7].ReadContext(ctx, "0000000000000001", "@", ReadOptions{Mode: ReadFresh}); e != nil {
		t.Fatal(e)
	}
	checkCached()
	auditRead := func() {
		checkCached()
		readCtx, stopRead := context.WithTimeout(ctx, 10*time.Second)
		defer stopRead()
		snapshots, e := f.services[7].Query(readCtx)
		if errors.Is(e, storage.ErrFrozen) || errors.Is(e, storage.ErrNotReady) || errors.Is(e, storage.ErrNotOwner) {
			report.ConcurrentReadsUnavailable++
			return
		}
		if e != nil {
			t.Fatalf("concurrent cold read: %v", e)
		}
		actual := map[string]storage.Record{}
		for _, snap := range snapshots {
			for _, row := range snap.Records {
				key := row.Location + ":" + row.ID
				if _, duplicate := actual[key]; duplicate {
					t.Fatal("concurrent cold read duplicated a row")
				}
				actual[key] = row
			}
		}
		if !reflect.DeepEqual(actual, expectedDuringCold) {
			t.Fatalf("concurrent cold read falsely complete: got %d want %d", len(actual), len(expectedDuringCold))
		}
		report.ConcurrentReadsExact++
	}

	begin := time.Now()
	for _, node := range f.nodes {
		if e := node.EnableCausalCompaction(CausalCompactionConfig{CoolFor: 100 * time.Millisecond, Interval: 250 * time.Millisecond, Timeout: 3 * time.Minute}); e != nil {
			t.Fatal(e)
		}
	}
	stop := func() {
		for _, node := range f.nodes {
			node.DisableCausalCompaction()
		}
		deadline := time.Now().Add(4 * time.Minute)
		for {
			busy := false
			for _, node := range f.nodes {
				busy = busy || node.mesh.compactionRunning.Load()
			}
			if !busy {
				return
			}
			if time.Now().After(deadline) {
				t.Error("cold workers failed to stop")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	defer stop()
	deadline := time.Now().Add(12 * time.Minute)
	lastLog := time.Now()
	lastRead := time.Now()
	for {
		if time.Since(lastRead) > 2*time.Second {
			auditRead()
			lastRead = time.Now()
		}
		all := true
		for _, node := range f.nodes {
			_ = node.Refresh()
			_ = node.Update()
		}
		for i, store := range f.stores {
			snapshot, e := store.CausalRouting()
			if e != nil {
				t.Fatal(e)
			}
			if snapshot.Current().Len() != 1 {
				all = false
			}
			if i == 0 && snapshot.Current().Len() != report.Trace[len(report.Trace)-1] {
				report.Trace = append(report.Trace, snapshot.Current().Len())
				t.Logf("cold scale trace=%v elapsed=%.1fs", report.Trace, time.Since(begin).Seconds())
				save()
			}
		}
		if all {
			stop()
			break
		}
		if time.Since(lastLog) > 10*time.Second {
			t.Logf("cold in progress %.1fs, trace=%v", time.Since(begin).Seconds(), report.Trace)
			lastLog = time.Now()
		}
		if time.Now().After(deadline) {
			for i, node := range f.nodes {
				status, _ := f.stores[i].ColdStatus()
				t.Logf("node %d cold=%+v last=%+v", i, status, node.mesh.lastColdResult.Load())
			}
			t.Fatal("compaction did not converge")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, store := range f.stores {
		status, e := store.ColdStatus()
		if e != nil {
			t.Fatal(e)
		}
		if status.Drain != nil {
			w := status.Drain
			report.Retirements = append(report.Retirements, coldVolumeRetirement{w.Source.VNode, w.Target.VNode, w.Bytes, len(w.Pieces)})
		}
	}
	phase(coldVolumePhase{Name: "automatic-cold", Active: 1}, begin)
	verify("verify-after-cold", 1, true)
	begin = time.Now()
	for _, store := range f.stores {
		if e := store.Close(); e != nil {
			t.Fatal(e)
		}
	}
	runtime.GC()
	f.open(t)
	for _, service := range f.services {
		if e := service.Resume(ctx); e != nil && !errors.Is(e, protocol.ErrColdPlanPreempted) {
			t.Fatal(e)
		}
	}
	phase(coldVolumePhase{Name: "reopen-all-disks", Active: 1}, begin)
	verify("verify-after-reopen", 1, true)
	for i := 0; i < count; i += max(1, count/512) {
		original := rows[i]
		original.Deleted = false
		original.Metrics = []float64{1, float64(i%97 - 48), float64(i%11) / 8}
		reply, e := f.nodes[7].MeshData(ctx, mesh.Request{Kind: "write", RequestID: fmt.Sprintf("seed:%d", i), Record: &original})
		if e != nil || reply.Applied || !reflect.DeepEqual(reply.Mutation, receipts[fmt.Sprintf("seed:%d", i)]) {
			t.Fatalf("stale ingress replay changed receipt: %v", e)
		}
	}
	t.Logf("PASS profile=%s rows=%d acknowledged=%d trace=%v", profile, count, len(receipts), report.Trace)
}
