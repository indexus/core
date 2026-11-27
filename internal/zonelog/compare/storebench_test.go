// Package compare loads the same generated records into internal/storage
// (stores storage and storage-nohistory: OmitHistory off and on) and into
// the zonelog store alone, and prints what it measured as one JSON object
// per run. scripts/research/storebench.sh builds it, runs every case and
// prints the table of docs/storage/zonelog.md, whose bbolt, no-history and
// flat-mock columns were measured while storage still ran on bbolt (the
// flat-mock code, flat_test.go, is at commit 74937c0).
//
//	go test -c -o storebench ./internal/zonelog/compare
//	storebench -store zonelog|storage|storage-nohistory -dir DIR [-scale 1]
//	storebench -store ... -dir DIR -phase open
//
// The load phase ends by killing its own process, so the open phase measures
// recovery after a kill.
package compare

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

// record is one generated record, in neither store's type.
type record struct {
	collection, location, id string
	metrics                  []float64
	payload                  []byte // a JSON string, or nil
}

// rawBytes is the record as IXR1 with a flat key: zone and depth (13 bytes),
// id, 16-byte header, metrics, payload.
func (r record) rawBytes() int { return 13 + len(r.id) + 16 + 8*len(r.metrics) + len(r.payload) }

type dataset struct {
	name, collection string
	count            int
	make             func(i int) record
}

// mix is splitmix64: record i is the same in every run and every store.
func mix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ x>>30) * 0xBF58476D1CE4E5B9
	x = (x ^ x>>27) * 0x94D049BB133111EB
	return x ^ x>>31
}

func chars(seed uint64, n int) string {
	b := make([]byte, n)
	for i := range b {
		if i%10 == 0 {
			seed = mix(seed)
		}
		b[i] = alphabet[seed>>(6*(i%10))&63]
	}
	return string(b)
}

func metrics(seed uint64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		seed = mix(seed)
		out[i] = float64(seed%100000) / 100
	}
	return out
}

func payload(seed uint64, n int) []byte {
	out := append(make([]byte, 0, n+64), '"')
	for chunk := chars(seed, 64); len(out) < n-1; {
		out = append(out, chunk...)
	}
	return append(out[:n-1], '"')
}

func datasets(scale float64) []dataset {
	n := func(count int) int { return max(int(float64(count)*scale), 64) }
	return []dataset{
		// AngVegetation001: 3-D linear space, one record per depth-5 zone,
		// 13 metrics, 200 bytes of payload.
		{"vegetation", "AngVegetation001", n(1_000_000), func(i int) record {
			s := mix(uint64(i))
			return record{"AngVegetation001", chars(s, 5), "v" + strconv.Itoa(i), metrics(s, 13), payload(s, 200)}
		}},
		// The same shape with 20 to 60 KB of payload.
		{"large", "AngLargePayload1", n(20_000), func(i int) record {
			s := mix(uint64(i) + 1<<40)
			return record{"AngLargePayload1", chars(s, 5), "L" + strconv.Itoa(i), metrics(s, 13), payload(s, 20_000+int(s%40_000))}
		}},
		// DVF-like: 5 metrics, no payload, depth 16, clustered: 2000 places
		// of 9 characters, then 7 characters in a small area around each.
		{"dvf", "DvFMV2020idx0001", n(500_000), func(i int) record {
			s := mix(uint64(i) + 2<<40)
			place := chars(mix(s%2000+3<<40), 9)
			near := []byte(chars(s, 7))
			for j := range 3 {
				near[j] = alphabet[strings.IndexByte(alphabet, near[j])&7]
			}
			return record{"DvFMV2020idx0001", place + string(near), "d" + strconv.Itoa(i), metrics(s, 5), nil}
		}},
	}
}

// engine is what the benchmark needs from a store.
type engine interface {
	apply(request string, r record) error
	applyBatch(requests []string, rs []record) error // nil when the store has no batch call
	children(collection, zone string) (count int64, err error)
	zone(collection, zone string, limit int) (int, error)
	settle() error
	diskBytes() (total, data int64)
}

type storageEngine struct {
	store *storage.Store
	path  string
}

func openStorage(dir string, omitHistory bool) (*storageEngine, error) {
	path := filepath.Join(dir, "data.db")
	store, err := storage.OpenWithOptions(path, "D", "bench", 64<<30, storage.Options{
		BatchWrites: true, BatchSize: 256, BatchDelay: 2 * time.Millisecond, OmitHistory: omitHistory,
	})
	if err != nil {
		return nil, err
	}
	if ready, err := store.CausalInitialized(); err != nil {
		return nil, err
	} else if !ready {
		holder, err := store.Endpoint("D")
		if err != nil {
			return nil, err
		}
		genesis := causal.Genesis{Cluster: "bench", Holder: holder}
		if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, genesis); err != nil {
			return nil, err
		}
	}
	return &storageEngine{store, path}, nil
}

func (e *storageEngine) apply(request string, r record) error {
	_, _, err := e.store.Apply(request, storage.Record{
		Collection: r.collection, Location: r.location, ID: r.id, Metrics: r.metrics, Payload: r.payload,
	})
	return err
}

func (e *storageEngine) applyBatch([]string, []record) error { return nil }

// pages reads the records at or below a zone the way a relay or a refresh
// does: paged inventory, metrics only unless payloads are asked for.
func (e *storageEngine) pages(collection, zone string, payloads bool, visit func(storage.Record) bool) error {
	query, err := tree.NewZoneKey(collection, zone)
	if err != nil {
		return err
	}
	request := storage.InventoryPageRequest{MetricsOnly: !payloads}
	for {
		snaps, page, err := e.store.CausalInventoryPage(context.Background(), &query, request)
		if err != nil {
			return err
		}
		for _, snap := range snaps {
			for _, r := range snap.Records {
				if !r.Deleted && !visit(r) {
					return nil
				}
			}
		}
		if page.Done {
			return nil
		}
		request.After = page.Next
	}
}

func (e *storageEngine) children(collection, zone string) (count int64, err error) {
	var sums [65][]float64
	depth := len(strings.TrimPrefix(zone, "@"))
	err = e.pages(collection, zone, false, func(r storage.Record) bool {
		i := 0
		if len(r.Location) > depth && r.Location != "@" {
			i = 1 + strings.IndexByte(alphabet, r.Location[depth])
		}
		for len(sums[i]) < len(r.Metrics) {
			sums[i] = append(sums[i], 0)
		}
		for j, v := range r.Metrics {
			sums[i][j] += v
		}
		count++
		return true
	})
	return count, err
}

func (e *storageEngine) zone(collection, zone string, limit int) (n int, err error) {
	err = e.pages(collection, zone, true, func(storage.Record) bool {
		n++
		return n < limit
	})
	return n, err
}

func (e *storageEngine) settle() error { return nil }

func (e *storageEngine) diskBytes() (int64, int64) {
	var total int64
	filepath.WalkDir(storage.RecordsDir(e.path), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, total
}

type zonelogEngine struct{ store *zonelog.Store }

func (e *zonelogEngine) apply(request string, r record) error {
	return e.applyBatch([]string{request}, []record{r})
}

func (e *zonelogEngine) applyBatch(requests []string, rs []record) error {
	batch := make([]zonelog.Record, len(rs))
	for i, r := range rs {
		batch[i] = zonelog.Record{
			Collection: r.collection, Location: r.location, ID: r.id, Metrics: r.metrics, Payload: r.payload,
		}
	}
	return e.store.Apply(batch)
}

func (e *zonelogEngine) children(collection, zone string) (count int64, err error) {
	sums, err := e.store.Children(collection, zone)
	for _, s := range sums {
		count += s.Count
	}
	return count, err
}

func (e *zonelogEngine) zone(collection, zone string, limit int) (n int, err error) {
	err = e.store.Scan(collection, zone, nil, func(zonelog.Record) bool {
		n++
		return n < limit
	})
	return n, err
}

func (e *zonelogEngine) settle() error { return e.store.Flush() }

func (e *zonelogEngine) diskBytes() (int64, int64) {
	st := e.store.Stats()
	fmt.Fprintf(os.Stderr, "zonelog: %+v\n", st)
	return st.SegmentBytes + st.LogBytes, st.SegmentBytes
}

var memMB = flag.Int("mem", 0, "zonelog memory budget in MB (0: the default, 256)")

func open(kind, dir string) (engine, error) {
	switch kind {
	case "storage":
		return openStorage(dir, false)
	case "storage-nohistory":
		return openStorage(dir, true)
	case "zonelog":
		store, err := zonelog.Open(dir, zonelog.Options{MemoryBytes: *memMB << 20})
		return &zonelogEngine{store}, err
	}
	return nil, fmt.Errorf("unknown store %q", kind)
}

// status reads a kB field of /proc/self/status.
func status(field string) int64 {
	raw, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, field+":") {
			kb, _ := strconv.ParseInt(strings.Fields(line)[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func cpuTime() time.Duration {
	var u syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

func quantile(d []time.Duration, q float64) float64 {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return float64(d[min(len(d)-1, int(q*float64(len(d))))].Microseconds()) / 1000
}

// timed runs read over the sample zones: one pass to warm, then measured
// passes until budget is spent or 300 timings are in.
func timed(zones []string, budget time.Duration, read func(zone string) error) (p50, p99 float64, n int, err error) {
	var d []time.Duration
	start := time.Now()
	for pass := 0; len(d) < 300 && (len(d) == 0 || time.Since(start) < budget); pass++ {
		for _, zone := range zones {
			t := time.Now()
			if err := read(zone); err != nil {
				return 0, 0, 0, err
			}
			if pass > 0 {
				d = append(d, time.Since(t))
			}
			if time.Since(start) > budget && len(d) > 0 {
				break
			}
		}
	}
	return quantile(d, 0.5), quantile(d, 0.99), len(d), nil
}

// reads times the two reads of the node: the sums of the 64 children of a
// zone, and the records of one small zone.
func reads(e engine, sets []dataset, out map[string]any) error {
	// Sums of the 64 children of a zone at depths 0 to 4 (children at depths
	// 1 to 5), on zones that hold records.
	veg, dvf := sets[0], sets[2]
	for depth := range 5 {
		zones := []string{"@"}
		for i := 0; depth > 0 && i < 100; i++ {
			zones = append(zones[:i], veg.make(int(mix(uint64(i)+9<<40) % uint64(veg.count))).location[:depth])
		}
		p50, p99, n, err := timed(zones, 25*time.Second, func(zone string) error {
			_, err := e.children(veg.collection, zone)
			return err
		})
		if err != nil {
			return err
		}
		key := "children_depth" + strconv.Itoa(depth+1)
		out[key+"_p50_ms"], out[key+"_p99_ms"], out[key+"_n"] = p50, p99, n
	}
	// Records of one small zone, at most 64: a DVF place of 9 characters.
	var zones []string
	for i := range 100 {
		zones = append(zones, dvf.make(i).location[:9])
	}
	p50, p99, _, err := timed(zones, 25*time.Second, func(zone string) error {
		n, err := e.zone(dvf.collection, zone, 64)
		if err == nil && n == 0 {
			err = fmt.Errorf("zone %q is empty", zone)
		}
		return err
	})
	if err != nil {
		return err
	}
	out["zone64_p50_ms"], out["zone64_p99_ms"] = p50, p99
	return nil
}

var (
	kind  = flag.String("store", "zonelog", "zonelog, storage or storage-nohistory")
	dir   = flag.String("dir", "", "data directory; without it the test is skipped")
	phase = flag.String("phase", "load", "load, read or open")
	scale = flag.Float64("scale", 1, "fraction of the full record counts")
	idle  = flag.Duration("idle", 60*time.Second, "how long to watch the idle store")
	only  = flag.String("only", "", "load this dataset alone and report its disk use")
)

func TestStoreBench(test *testing.T) {
	if *dir == "" {
		test.Skip("run through scripts/research/storebench.sh")
	}
	out := map[string]any{"store": *kind, "phase": *phase, "scale": *scale}
	fail := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "storebench:", err)
			os.Exit(1)
		}
	}
	sets := datasets(*scale)

	start := time.Now()
	e, err := open(*kind, *dir)
	fail(err)
	if *phase == "read" {
		fail(reads(e, sets, out))
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	if *phase == "open" {
		first := sets[0].make(0)
		n, err := e.zone(first.collection, first.location, 1)
		fail(err)
		out["open_ms"] = float64(time.Since(start).Microseconds()) / 1000
		out["first_read_found"] = n
		total, _ := e.diskBytes()
		out["open_s_per_gb"] = time.Since(start).Seconds() / (float64(total) / (1 << 30))
		out["rss_mb"] = status("VmRSS") >> 20
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}

	// Batched inserts: 256 callers, one record each, grouped by the store.
	var records, raw, asJSON int64
	for _, set := range sets {
		if *only != "" && set.name != *only {
			continue
		}
		var next atomic.Int64
		var wg sync.WaitGroup
		var failed atomic.Value
		t := time.Now()
		for range 256 {
			wg.Go(func() {
				for i := int(next.Add(1)) - 1; i < set.count; i = int(next.Add(1)) - 1 {
					if err := e.apply(set.name+strconv.Itoa(i), set.make(i)); err != nil {
						failed.Store(err)
						return
					}
				}
			})
		}
		wg.Wait()
		if err, _ := failed.Load().(error); err != nil {
			fail(err)
		}
		out[set.name+"_inserts_per_s"] = int(float64(set.count) / time.Since(t).Seconds())
		for i := 0; i < set.count; i += max(set.count/1000, 1) {
			r := set.make(i)
			encoded, _ := json.Marshal(storage.Record{Collection: r.collection, Location: r.location, ID: r.id, Metrics: r.metrics, Payload: r.payload})
			raw += int64(r.rawBytes()) * int64(max(set.count/1000, 1))
			asJSON += int64(len(encoded)) * int64(max(set.count/1000, 1))
		}
		records += int64(set.count)
		fmt.Fprintf(os.Stderr, "%s: %s loaded in %v\n", *kind, set.name, time.Since(t).Round(time.Second))
	}
	out["load_s"] = int(time.Since(start).Seconds())
	out["rss_after_load_mb"], out["rss_peak_mb"] = status("VmRSS")>>20, status("VmHWM")>>20

	t := time.Now()
	fail(e.settle())
	out["settle_s"] = time.Since(t).Seconds()
	total, data := e.diskBytes()
	out["records"], out["raw_bytes_per_record"], out["json_bytes_per_record"] = records, raw/records, asJSON/records
	out["disk_mb"], out["disk_bytes_per_record"] = total>>20, total/records
	out["disk_over_raw"], out["data_over_raw"] = float64(total)/float64(raw), float64(data)/float64(raw)
	if *only != "" {
		out["dataset"] = *only
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}

	// The store's own batch call, where it has one, and one write per fsync.
	// Both write new records of the DVF shape into a collection of their own.
	dvf := sets[2]
	extra := func(i int) (string, record) {
		r := dvf.make(1<<30 + i)
		r.collection = "DvFExtraWrites01"
		return "extra" + strconv.Itoa(i), r
	}
	n := max(int(200_000**scale), 512)
	t = time.Now()
	batched := true
	for i := 0; i < n && batched; i += 256 {
		requests, rs := make([]string, 256), make([]record, 256)
		for j := range rs {
			requests[j], rs[j] = extra(i + j)
		}
		fail(e.applyBatch(requests, rs))
		batched = *kind == "zonelog"
	}
	if batched {
		out["batch_call_inserts_per_s"] = int(float64(n) / time.Since(t).Seconds())
	}
	t = time.Now()
	for i := range 2000 {
		fail(e.apply(extra(n + 1000 + i)))
	}
	out["single_inserts_per_s"] = int(2000 / time.Since(t).Seconds())
	fail(e.settle())

	// Idle: no request for a while; any CPU used is the store's own.
	cpu := cpuTime()
	time.Sleep(*idle)
	out["idle_cpu_ms"] = float64((cpuTime() - cpu).Microseconds()) / 1000
	out["idle_s"] = idle.Seconds()
	out["rss_idle_mb"] = status("VmRSS") >> 20

	fail(reads(e, sets, out))
	out["rss_after_reads_mb"] = status("VmRSS") >> 20
	json.NewEncoder(os.Stdout).Encode(out)
	// Die without closing anything: the open phase measures recovery.
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
}
