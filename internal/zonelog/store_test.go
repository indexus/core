package zonelog

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const testCollection = "0AABCDEFGH123456"

// oracle is the store's expected content: live records by location and id.
type oracle map[string]Record

func (o oracle) apply(r Record) {
	key := r.Location + "\x00" + r.ID
	if r.Deleted {
		delete(o, key)
		return
	}
	if r.Weight == 0 {
		r.Weight = 1
	}
	o[key] = r
}

// children computes what Children must return, by brute force.
func (o oracle) children(zone string) (out [65]Sum) {
	prefix := strings.TrimPrefix(zone, "@")
	keys := make([]string, 0, len(o))
	for key := range o {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	several := [65]bool{}
	for _, key := range keys {
		r := o[key]
		if !strings.HasPrefix(r.Location, prefix) && r.Location != "@" || r.Location == "@" && prefix != "" {
			continue
		}
		i := 0
		if rest := strings.TrimPrefix(r.Location, "@")[len(prefix):]; rest != "" {
			i = 1 + strings.IndexByte(alphabet, rest[0])
		}
		switch {
		case out[i].Count == 0:
			out[i].Only = r.Location
		case out[i].Only != r.Location:
			several[i] = true
		}
		out[i].Count++
		out[i].Weight += r.Weight
		for len(out[i].Metrics) < len(r.Metrics) {
			out[i].Metrics = append(out[i].Metrics, 0)
		}
		for j, v := range r.Metrics {
			out[i].Metrics[j] += v
		}
	}
	for i := range out {
		if several[i] {
			out[i].Only = ""
		}
	}
	return out
}

func checkStore(t *testing.T, s *Store, o oracle, zones []string) {
	t.Helper()
	for _, zone := range zones {
		got, err := s.Children(testCollection, zone)
		if err != nil {
			t.Fatal(err)
		}
		want := o.children(zone)
		for i := range want {
			if got[i].Count != want[i].Count || math.Abs(got[i].Weight-want[i].Weight) > 1e-6 || got[i].Only != want[i].Only {
				t.Fatalf("zone %q child %d: got %+v, want %+v", zone, i, got[i], want[i])
			}
			for j, v := range want[i].Metrics {
				if math.Abs(got[i].Metrics[j]-v) > 1e-6 {
					t.Fatalf("zone %q child %d metric %d: got %v, want %v", zone, i, j, got[i].Metrics[j], v)
				}
			}
		}
		var keys, wantKeys []string
		err = s.Scan(testCollection, zone, nil, func(r Record) bool {
			want, ok := o[r.Location+"\x00"+r.ID]
			if !ok || !bytes.Equal(want.Payload, r.Payload) || fmt.Sprint(want.Metrics) != fmt.Sprint(r.Metrics) {
				t.Fatalf("scan %q: unexpected record %+v (want %+v)", zone, r, want)
			}
			keys = append(keys, r.Location+"\x00"+r.ID)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		prefix := strings.TrimPrefix(zone, "@")
		for key, r := range o {
			if prefix == "" || r.Location != "@" && strings.HasPrefix(r.Location, prefix) {
				wantKeys = append(wantKeys, key)
			}
		}
		sort.Strings(keys)
		sort.Strings(wantKeys)
		if fmt.Sprint(keys) != fmt.Sprint(wantKeys) {
			t.Fatalf("scan %q: %d records, want %d", zone, len(keys), len(wantKeys))
		}
	}
	for key, want := range o {
		if rand.Intn(20) != 0 {
			continue
		}
		got, ok, err := s.Get(testCollection, want.Location, want.ID)
		if err != nil || !ok || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("get %q: %v %v", key, ok, err)
		}
	}
}

func randomLocation(rng *rand.Rand) string {
	depth := rng.Intn(4)
	if depth == 0 {
		return "@"
	}
	b := make([]byte, depth)
	for i := range b {
		b[i] = alphabet[rng.Intn(4)*21] // few distinct characters: zones fill up
	}
	return string(b)
}

// Inserts, replaces, deletes and re-inserts across many flushes and merges:
// reads and zone sums equal a brute-force model, before and after reopening.
func TestRandomMutationsMatchModel(t *testing.T) {
	dir := t.TempDir()
	opt := Options{MemoryBytes: 128 << 10, LogRetain: 64 << 10, BlobBytes: 400, NoSync: true}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	o := oracle{}
	zones := []string{"@", "0", "L", "_", "0L", "g0", "__", "LgL"}
	for step := range 6000 {
		var batch []Record
		for range 1 + rng.Intn(4) {
			r := Record{Collection: testCollection, Location: randomLocation(rng), ID: fmt.Sprint(rng.Intn(400))}
			switch rng.Intn(10) {
			case 0, 1:
				r.Deleted = true
			default:
				r.Metrics = []float64{float64(rng.Intn(100)), rng.Float64()}[:rng.Intn(3)]
				r.Payload = bytes.Repeat([]byte{byte(step)}, rng.Intn(3)*rng.Intn(300))
				if rng.Intn(8) == 0 {
					r.Weight = float64(1 + rng.Intn(5))
				}
			}
			batch = append(batch, r)
		}
		if err := s.Apply(batch); err != nil {
			t.Fatal(err)
		}
		for _, r := range batch {
			o.apply(r)
		}
		if step%1500 == 1499 {
			checkStore(t, s, o, zones)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if s, err = Open(dir, opt); err != nil {
				t.Fatal(err)
			}
			checkStore(t, s, o, zones)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	checkStore(t, s, o, zones)
	st := s.Stats()
	if st.Segments == 0 || st.Segments > 12 {
		t.Fatalf("segments after merges: %d", st.Segments)
	}
	t.Logf("%d live records, %+v", len(o), st)
	s.Close()
}

// A write that changes nothing is not applied: the same bytes take no seq,
// before and after a restart. A control record commits with the data of its
// batch and stays out of the zone sums.
func TestUnchangedWritesAndControlRecords(t *testing.T) {
	dir := t.TempDir()
	opt := Options{NoSync: true}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Collection: testCollection, Location: "ab", ID: "1", Metrics: []float64{3}}
	fence := Record{Collection: Control, Location: "@", ID: "fence/ab", Payload: []byte("frozen")}
	if err := s.Apply([]Record{record, fence}); err != nil || s.Seq() != 2 {
		t.Fatalf("first: seq %d, %v", s.Seq(), err)
	}
	for round := range 2 {
		if err := s.Apply([]Record{record}); err != nil || s.Seq() != 2 {
			t.Fatalf("round %d: unchanged write: seq %d, %v", round, s.Seq(), err)
		}
		s.Close()
		if s, err = Open(dir, opt); err != nil {
			t.Fatal(err)
		}
	}
	defer s.Close()
	if r, ok, _ := s.Get(Control, "@", "fence/ab"); !ok || string(r.Payload) != "frozen" {
		t.Fatalf("control record: %+v %v", r, ok)
	}
	if sums, _ := s.Children(testCollection, "a"); sums[1+strings.IndexByte(alphabet, 'b')].Count != 1 {
		t.Fatal("control records leak into zone sums")
	}
}

// Large payloads are written once: merges keep pointing at the payload file
// of the flush. Once enough of a payload file is dead it is copied out of
// and removed.
func TestMergesPointAtLargePayloads(t *testing.T) {
	fs := newMemFS()
	s, err := Open("", Options{FS: fs, MemoryBytes: 256 << 10, BlobBytes: 1000, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	o := oracle{}
	write := func(i, id int) {
		// Random bytes: flate cannot shrink them, so bytes written count payloads.
		payload := make([]byte, 3000)
		rand.New(rand.NewSource(int64(i))).Read(payload)
		r := Record{Collection: testCollection, Location: "ab", ID: fmt.Sprint(id), Payload: payload}
		if err := s.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
		o.apply(r)
	}
	// How the memtable splits into segments depends on timing, so write
	// until a merge has rewritten a segment.
	n := 0
	for n < 400 || fs.written(".seg") == fs.size(".seg") {
		if n == 4000 {
			t.Fatalf("no merge happened: %+v", s.Stats())
		}
		for range 100 {
			write(n, n)
			n++
		}
		s.Flush()
	}
	payloads := int64(n * 3000)
	if written := fs.written(".val"); written < payloads || written > payloads+payloads/100 {
		t.Fatalf("payload bytes written: %d for %d of payloads", written, payloads)
	}
	for i := range 2000 { // replace the same 100 records: old payload files die
		write(1000+i, i%100)
	}
	s.Flush()
	checkStore(t, s, o, []string{"a"})
	if st := s.Stats(); st.SegmentBytes > 3*payloads/2+payloads/4 {
		t.Fatalf("dead payloads are kept: %d bytes on disk for %d live", st.SegmentBytes, payloads)
	}
}

// A write is in no read before its fsync returns.
func TestWriteIsInvisibleUntilSynced(t *testing.T) {
	fs := newMemFS()
	s, err := Open("", Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(v float64) {
		r := Record{Collection: testCollection, Location: "ab", ID: "1", Metrics: []float64{v}}
		if err := s.Apply([]Record{r}); err != nil {
			t.Error(err)
		}
	}
	put(1)
	held := fs.holdSync() // the next fsync waits
	done := make(chan struct{})
	go func() { put(2); close(done) }()
	<-held
	if r, _, _ := s.Get(testCollection, "ab", "1"); r.Metrics[0] != 1 {
		t.Fatalf("read saw an unsynced write: %+v", r)
	}
	if sums, _ := s.Children(testCollection, "a"); sums[1+strings.IndexByte(alphabet, 'b')].Metrics[0] != 1 {
		t.Fatalf("sums saw an unsynced write: %+v", sums)
	}
	n := 0
	s.Scan(testCollection, "a", nil, func(r Record) bool { n += int(r.Metrics[0]); return true })
	if n != 1 {
		t.Fatalf("scan saw an unsynced write")
	}
	close(fs.release)
	<-done
	if r, _, _ := s.Get(testCollection, "ab", "1"); r.Metrics[0] != 2 {
		t.Fatalf("synced write is not visible: %+v", r)
	}
}

// REC-1. A store keeps its identity, and a directory in another format is
// refused.
func TestIdentityAndFormat(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.id
	s.Close()
	if s, err = Open(dir, Options{}); err != nil || s.id != id || len(id) != 32 {
		t.Fatalf("identity changed: %q then %q, %v", id, s.id, err)
	}
	s.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "MANIFEST"))
	os.WriteFile(filepath.Join(dir, "MANIFEST"), bytes.Replace(raw, []byte(format), []byte("zonelog-0"), 1), 0o600)
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("another format was opened")
	}
}

func TestZoneRangeBounds(t *testing.T) {
	for _, c := range []struct{ zone, in, out string }{
		{"a", "a", "b"}, {"a", "a__", "b"}, {"a_", "a_0", "b"}, {"__", "___", "_-"}, {"@", "_", ""}, {"a0", "a0", "a"},
	} {
		lo, hi, err := zoneRange(3, c.zone)
		if err != nil {
			t.Fatal(err)
		}
		in, _ := makeKey(3, c.in, "x")
		if bytes.Compare(in, lo) < 0 || bytes.Compare(in, hi) >= 0 {
			t.Errorf("zone %q should hold %q", c.zone, c.in)
		}
		if c.out != "" {
			out, _ := makeKey(3, c.out, "x")
			if bytes.Compare(out, lo) >= 0 && bytes.Compare(out, hi) < 0 {
				t.Errorf("zone %q should not hold %q", c.zone, c.out)
			}
		}
	}
	bits, depth, _ := locBits("pQrStUv-_0")
	if got := locString(bits[:], depth); got != "pQrStUv-_0" {
		t.Errorf("location round trip: %q", got)
	}
}

// Sums are compensated (Neumaier pairs) in the memtable, in pages, in the
// page index and across segments: 1e16 + 1 - 1e16 is 1 wherever the three
// rows are, a replace cancels the old value to the bit, and so after a
// reopen.
func TestSumsAreCompensatedAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	write := func(id string, v float64) {
		t.Helper()
		r := Record{Collection: testCollection, Location: "ab", ID: id, Metrics: []float64{v}}
		if err := s.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string, want float64, count int64) {
		t.Helper()
		sums, err := s.Children(testCollection, "@")
		if err != nil {
			t.Fatal(err)
		}
		got := sums[1+strings.IndexByte(alphabet, 'a')]
		if got.Count != count || len(got.Metrics) != 1 || got.Metrics[0] != want {
			t.Fatalf("%s: %+v, want %v over %d", when, got, want, count)
		}
	}
	write("0", 1e16)
	write("1", 1)
	check("memtable", 1e16+1, 2) // the value itself is not representable: rounded once
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	write("2", -1e16)
	check("memtable over a segment", 1, 3)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	check("two segments", 1, 3)
	write("0", 0) // a replace: the retraction cancels 1e16
	write("2", 0)
	check("replaced", 1, 3)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir, Options{NoSync: true}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check("reopened", 1, 3)
}

// Payload files are flate frames: a compressible payload takes less disk
// than its bytes, every payload reads back exact, in any order, across
// frames, after merges and after a reopen.
func TestPayloadsAreCompressedOnDisk(t *testing.T) {
	dir := t.TempDir()
	opt := Options{MemoryBytes: 1 << 20, BlobBytes: 1000, NoSync: true}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(3))
	want := map[string][]byte{}
	raw := 0
	for i := range 600 {
		// Text-like payloads of 100 B to 90 kB: some span several frames.
		n := 100 + rng.Intn(90_000)*(i%7/6) + rng.Intn(3000)
		p := make([]byte, n)
		for j := range p {
			p[j] = "abcdefgh {}\":,0123456789"[rng.Intn(24)]
		}
		id := fmt.Sprint(i)
		want[id], raw = p, raw+n
		r := Record{Collection: testCollection, Location: "ab", ID: id, Payload: p}
		if err := s.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string) {
		t.Helper()
		for _, i := range rng.Perm(len(want)) {
			id := fmt.Sprint(i)
			got, ok, err := s.Get(testCollection, "ab", id)
			if err != nil || !ok || !bytes.Equal(got.Payload, want[id]) {
				t.Fatalf("%s: payload %s: %d bytes, want %d (%v)", when, id, len(got.Payload), len(want[id]), err)
			}
		}
	}
	check("memtable")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	check("segment")
	var disk int64
	for _, e := range must(os.ReadDir(dir)) {
		if strings.HasSuffix(e.Name(), ".val") {
			disk += must(e.Info()).Size()
		}
	}
	if disk == 0 || disk > int64(raw)*7/10 {
		t.Fatalf("payload files take %d bytes for %d of payloads", disk, raw)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir, opt); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check("reopened")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
