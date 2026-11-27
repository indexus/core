//go:build linux

package zonelog

import (
	"container/list"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestScale is the memory and latency benchmark of docs/storage/zonelog.md
// (section "At 2, 10 and 15 M records"). It runs only when asked, one phase
// per process, so that each RSS is that phase's own:
//
//	ZONELOG_SCALE=2000000 ZONELOG_SCALE_DIR=/tmp/scale ZONELOG_SCALE_PHASE=load \
//	  go test -run TestScale -count=1 -timeout 0 -v ./internal/zonelog
//	... ZONELOG_SCALE_PHASE=read ...
//
// load writes the records as one bulk segment per collection (as an import
// of prepared batches does), then 50 000 single writes; read reopens the
// store and times reads cold (the store's cache emptied and the files
// dropped from the kernel's page cache before each one) and warm. Each phase
// prints one JSON line. ZONELOG_SCALE_MEM sets MemoryBytes in MB.
//
// The records are those of the Anglet collections, in proportion: 95 %
// vegetation (one record per depth-5 voxel of the 3-D space, clustered, 13
// metrics, a 100-byte JSON payload: a third of the real one, to fit the
// benchmark's disk; payload bytes go to payload files, not to pages, indexes
// or filters), 5 % DVF-like (depth 16, clustered around places, 5 metrics, no
// payload), and one in 5 000 an orthophoto tile (depth 9, 4 metrics, 20 to
// 60 KB of incompressible payload).
func TestScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ZONELOG_SCALE"))
	dir := os.Getenv("ZONELOG_SCALE_DIR")
	if n == 0 || dir == "" {
		t.Skip("set ZONELOG_SCALE and ZONELOG_SCALE_DIR")
	}
	mem, _ := strconv.Atoi(os.Getenv("ZONELOG_SCALE_MEM"))
	d := scaleData(n)
	out := map[string]any{"records": n, "phase": os.Getenv("ZONELOG_SCALE_PHASE"), "memory_mb": mem}
	start := time.Now()
	s, err := Open(dir, Options{MemoryBytes: mem << 20})
	if err != nil {
		t.Fatal(err)
	}
	out["open_ms"] = ms(time.Since(start))
	if os.Getenv("ZONELOG_SCALE_PHASE") == "read" {
		scaleReads(t, s, dir, d, out)
	} else {
		scaleLoad(t, s, d, out)
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type scaleSet struct {
	veg, dvf, ortho int
	gap             uint64 // vegetation: mean gap between voxels, in depth-5 cells
}

func scaleData(n int) scaleSet {
	d := scaleSet{veg: n * 95 / 100, ortho: max(n/5000, 50)}
	d.dvf = n - d.veg - d.ortho
	d.gap = (1 << 30) * 9 / 10 / uint64(d.veg)
	return d
}

// vegetation calls visit with the location of each voxel, in key order:
// runs of near cells (gaps of 1 to 4) between larger jumps, as vegetation
// clusters in a scene.
func (d scaleSet) vegetation(visit func(i int, loc uint64)) {
	big := max(5*(d.gap-2), 1) // 80 % gaps of mean 2.5, 20 % of mean big
	loc := uint64(0)
	for i := range d.veg {
		r := mix64(uint64(i))
		if r%10 < 8 {
			loc += 1 + r>>8%4
		} else {
			loc += 1 + r>>8%(2*big)
		}
		visit(i, loc)
	}
}

func (d scaleSet) vegRecord(i int, loc uint64) Record {
	s := mix64(uint64(i) + 7<<40)
	return Record{Collection: "AngVegetation001", Location: locOf(loc, 5), ID: "v" + strconv.Itoa(i),
		Metrics: scaleMetrics(s, 13), Payload: []byte(`"` + scaleChars(s, 98) + `"`)}
}

// sales calls visit with each DVF-like record, in key order: places of 9
// characters, each with its sales spread over a small area.
func (d scaleSet) sales(visit func(r Record)) {
	places := max(d.dvf/250, 1)
	at, step := uint64(0), (uint64(1)<<54)/uint64(places)*9/10
	type sale struct {
		near uint64
		id   string
	}
	i := 0
	for p := range places {
		at += 1 + mix64(uint64(p)+3<<40)%(2*step-2)
		var sales []sale
		for k := 0; k < d.dvf/places+boolInt(p < d.dvf%places); k++ {
			s := mix64(uint64(i) + 2<<40)
			sales = append(sales, sale{s >> 22 &^ (0b111000<<36 | 0b111000<<30 | 0b111000<<24), "d" + strconv.Itoa(i)})
			i++
		}
		sort.Slice(sales, func(a, b int) bool {
			if sales[a].near != sales[b].near {
				return sales[a].near < sales[b].near
			}
			return sales[a].id < sales[b].id
		})
		for _, x := range sales {
			s := mix64(uint64(len(x.id)) + x.near)
			visit(Record{Collection: "DvFMV2020idx0001", Location: locOf(at, 9) + locOf(x.near, 7), ID: x.id, Metrics: scaleMetrics(s, 5)})
		}
	}
}

func (d scaleSet) orthoLocs() []uint64 {
	locs := make([]uint64, d.ortho)
	for i := range locs {
		locs[i] = mix64(uint64(i)+5<<40) >> 10 // 54 bits
	}
	slices.Sort(locs)
	return slices.Compact(locs)
}

func (d scaleSet) orthoRecord(i int, loc uint64) Record {
	s := mix64(uint64(i) + 6<<40)
	payload := make([]byte, 20_000+s%40_000)
	for j := 0; j < len(payload); j += 8 {
		s = mix64(s)
		for k := 0; k < 8 && j+k < len(payload); k++ {
			payload[j+k] = byte(s >> (8 * k))
		}
	}
	return Record{Collection: "OrthoBDORT2024v1", Location: locOf(loc, 9), ID: "t" + strconv.Itoa(i), Metrics: scaleMetrics(s, 4), Payload: payload, Binary: true}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ x>>30) * 0xBF58476D1CE4E5B9
	x = (x ^ x>>27) * 0x94D049BB133111EB
	return x ^ x>>31
}

// locOf is the location of depth chars whose bits are the low 6×depth bits
// of v.
func locOf(v uint64, depth int) string {
	b := make([]byte, depth)
	for j := range b {
		b[j] = alphabet[v>>(6*(depth-1-j))&63]
	}
	return string(b)
}

func scaleChars(seed uint64, n int) string {
	b := make([]byte, n)
	for i := range b {
		if i%10 == 0 {
			seed = mix64(seed)
		}
		b[i] = alphabet[seed>>(6*(i%10))&63]
	}
	return string(b)
}

func scaleMetrics(seed uint64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		seed = mix64(seed)
		out[i] = float64(seed%100000) / 100
	}
	return out
}

// procKB reads a kB field of /proc/self/status, in MB.
func procMB(field string) float64 {
	raw, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, field+":") {
			kb, _ := strconv.ParseFloat(strings.Fields(line)[1], 64)
			return float64(int(kb/1024*10)) / 10
		}
	}
	return 0
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// memory records the process's resident memory and the Go heap after a
// collection, under a name.
func memory(out map[string]any, name string, s *Store) {
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	st := s.Stats()
	s.cache.mu.Lock()
	cached := s.cache.used
	s.cache.mu.Unlock()
	out[name] = map[string]float64{"rss_mb": procMB("VmRSS"), "heap_inuse_mb": float64(m.HeapInuse>>16) / 16,
		"index_mb": float64(st.IndexBytes>>10) / 1024, "cache_mb": float64(cached>>16) / 16}
}

func scaleLoad(t *testing.T, s *Store, d scaleSet, out map[string]any) {
	start := time.Now()
	bulk := func(collection string, expect int, each func(add func(Record) error) error) {
		b, err := s.BeginBulk(collection, "@", false, int64(expect))
		if err == nil {
			err = each(b.Add)
		}
		if err == nil {
			err = b.Commit()
		} else {
			b.Abort()
		}
		if err != nil {
			t.Fatal(collection, err)
		}
	}
	bulk("AngVegetation001", d.veg, func(add func(Record) error) (err error) {
		d.vegetation(func(i int, loc uint64) {
			if err == nil {
				err = add(d.vegRecord(i, loc))
			}
		})
		return err
	})
	bulk("DvFMV2020idx0001", d.dvf, func(add func(Record) error) (err error) {
		d.sales(func(r Record) {
			if err == nil {
				err = add(r)
			}
		})
		return err
	})
	bulk("OrthoBDORT2024v1", d.ortho, func(add func(Record) error) error {
		for i, loc := range d.orthoLocs() {
			if err := add(d.orthoRecord(i, loc)); err != nil {
				return err
			}
		}
		return nil
	})
	out["bulk_s"] = time.Since(start).Seconds()
	out["bulk_rss_peak_mb"] = procMB("VmHWM")
	memory(out, "after_bulk", s)

	// Single writes over the imported records: each looks its key up in
	// every segment (filter, index, page), as a write does.
	start = time.Now()
	const writes = 50_000
	for i := 0; i < writes; i += 256 {
		batch := make([]Record, 256)
		for j := range batch {
			r := d.vegRecord(d.veg+i+j, mix64(uint64(i+j)+8<<40)%(1<<30))
			batch[j] = r
		}
		if err := s.Apply(batch); err != nil {
			t.Fatal(err)
		}
	}
	out["writes_per_s"] = int(writes / time.Since(start).Seconds())
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	out["rss_peak_mb"] = procMB("VmHWM")
	memory(out, "after_load", s)
	st := s.Stats()
	out["segments"], out["disk_mb"] = st.Segments, (st.SegmentBytes+st.LogBytes)>>20
}

// dropCaches empties the store's cache and asks the kernel to drop the
// store's files from its page cache: the next read goes to the disk.
func dropCaches(s *Store, dir string) {
	s.cache.mu.Lock()
	s.cache.byKey = map[[2]uint64]*list.Element{}
	s.cache.order.Init()
	s.cache.used = 0
	s.cache.mu.Unlock()
	names, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, name := range names {
		if f, err := os.Open(name); err == nil {
			syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, 4 /* POSIX_FADV_DONTNEED */, 0, 0)
			f.Close()
		}
	}
}

func scaleReads(t *testing.T, s *Store, dir string, d scaleSet, out map[string]any) {
	memory(out, "after_open", s)
	// Samples: 100 voxels spread over the vegetation, 100 DVF places, 50
	// tiles.
	type vox struct {
		loc string
		id  string
	}
	var voxels []vox
	every := max(d.veg/100, 1)
	d.vegetation(func(i int, loc uint64) {
		if i%every == every/2 {
			voxels = append(voxels, vox{locOf(loc, 5), "v" + strconv.Itoa(i)})
		}
	})
	var places []string
	k := 0
	d.sales(func(r Record) {
		if k++; k%max(d.dvf/100, 1) == 0 {
			places = append(places, r.Location[:9])
		}
	})
	places = slices.Compact(places)
	tiles := d.orthoLocs()
	if len(tiles) > 50 {
		tiles = tiles[:50]
	}
	type op struct {
		name string
		n    int
		run  func(i int) error
	}
	children := func(depth int) op {
		return op{"veg_children_depth" + strconv.Itoa(depth), 100, func(i int) error {
			zone := "@"
			if depth > 0 {
				zone = voxels[i%len(voxels)].loc[:depth]
			}
			sums, err := s.Children("AngVegetation001", zone)
			total := int64(0)
			for _, x := range sums {
				total += x.Count
			}
			if err == nil && total == 0 {
				err = fmt.Errorf("zone %s is empty", zone)
			}
			return err
		}}
	}
	ops := []op{children(0), children(1), children(2), children(3), children(4),
		{"dvf_children_place", 100, func(i int) error {
			_, err := s.Children("DvFMV2020idx0001", places[i%len(places)])
			return err
		}},
		{"veg_zone_records_depth3", 100, func(i int) error {
			n := 0
			err := s.Scan("AngVegetation001", voxels[i%len(voxels)].loc[:3], nil, func(Record) bool { n++; return n < 64 })
			if err == nil && n == 0 {
				err = fmt.Errorf("empty zone")
			}
			return err
		}},
		{"dvf_zone_records_place", 100, func(i int) error {
			n := 0
			err := s.Scan("DvFMV2020idx0001", places[i%len(places)], nil, func(Record) bool { n++; return n < 64 })
			if err == nil && n == 0 {
				err = fmt.Errorf("empty place %s", places[i%len(places)])
			}
			return err
		}},
		{"veg_get", 100, func(i int) error {
			v := voxels[i%len(voxels)]
			_, ok, err := s.Get("AngVegetation001", v.loc, v.id)
			if err == nil && !ok {
				err = fmt.Errorf("no voxel %v", v)
			}
			return err
		}},
		{"ortho_get", len(tiles), func(i int) error {
			_, ok, err := s.Get("OrthoBDORT2024v1", locOf(tiles[i], 9), "t"+strconv.Itoa(i))
			if err == nil && !ok {
				err = fmt.Errorf("no tile %d", i)
			}
			return err
		}},
	}
	quantiles := func(ds []time.Duration) []float64 {
		slices.Sort(ds)
		return []float64{ms(ds[len(ds)/2]), ms(ds[min(len(ds)-1, len(ds)*99/100)])}
	}
	cold, warm, timed := map[string][]float64{}, map[string][]float64{}, map[string][]time.Duration{}
	for _, o := range ops {
		var ds []time.Duration
		for i := range o.n {
			dropCaches(s, dir)
			start := time.Now()
			if err := o.run(i); err != nil {
				t.Fatal(o.name, err)
			}
			ds = append(ds, time.Since(start))
		}
		cold[o.name] = quantiles(ds)
	}
	memory(out, "after_cold", s)
	// Warm: one pass over every sample of every read, then three timed.
	for pass := range 4 {
		for _, o := range ops {
			var ds []time.Duration
			for i := range o.n {
				start := time.Now()
				if err := o.run(i); err != nil {
					t.Fatal(o.name, err)
				}
				ds = append(ds, time.Since(start))
			}
			if pass > 0 {
				timed[o.name] = append(timed[o.name], ds...)
			}
		}
	}
	for name, ds := range timed {
		warm[name] = quantiles(ds)
	}
	out["cold_p50_p99_ms"], out["warm_p50_p99_ms"] = cold, warm
	memory(out, "after_warm", s)
	out["rss_peak_mb"] = procMB("VmHWM")
}
