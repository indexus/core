package zonelog

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// bulkRecords is a sorted batch for zone "L": records the base workload
// also writes, and new ones.
func bulkRecords(rng *rand.Rand, n int) []Record {
	seen := map[string]Record{}
	for len(seen) < n {
		loc := "L" + strings.TrimPrefix(randomLocation(rng), "@")
		r := Record{Collection: testCollection, Location: loc, ID: fmt.Sprint(rng.Intn(400)), Metrics: []float64{float64(rng.Intn(9)), 2}, Payload: bytes.Repeat([]byte{'b'}, rng.Intn(600))}
		seen[r.Location+"\x00"+r.ID] = r
	}
	out := make([]Record, 0, n)
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := makeKey(0, out[i].Location, out[i].ID)
		b, _ := makeKey(0, out[j].Location, out[j].ID)
		return bytes.Compare(a, b) < 0
	})
	return out
}

// Add is AddHead for a record given by its location and id.
func (b *Bulk) Add(r Record) error {
	if r.Deleted {
		return fmt.Errorf("zonelog: bulk record %s/%s is a delete", r.Location, r.ID)
	}
	key, err := makeKey(b.col, r.Location, r.ID)
	if err != nil {
		return err
	}
	return b.add(key, r.Weight, r.Metrics, r.Payload, r.Binary)
}

func writeBulk(s *Store, records []Record, replace bool, receipt string) error {
	b, err := s.BeginBulk(testCollection, "L", replace, int64(len(records)))
	if err != nil {
		return err
	}
	if err := b.Also(Record{Collection: Control, Location: "@", ID: receipt, Payload: []byte("done")}); err != nil {
		b.Abort()
		return err
	}
	for _, r := range records {
		if err := b.Add(r); err != nil {
			b.Abort()
			return err
		}
	}
	return b.Commit()
}

// A bulk replace of a zone is exactly: every live record of the zone gone,
// the batch in. Sums, scans and gets agree with the model after the cut,
// after later writes over bulk records, after merges and after a reopen.
func TestBulkReplaceMatchesModel(t *testing.T) {
	dir := t.TempDir()
	opt := Options{MemoryBytes: 128 << 10, BlobBytes: 300, NoSync: true}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(3))
	o := oracle{}
	zones := []string{"@", "L", "L0", "LL", "g", "LgL"}
	write := func(steps int) {
		for step := range steps {
			r := Record{Collection: testCollection, Location: randomLocation(rng), ID: fmt.Sprint(rng.Intn(400)), Metrics: []float64{float64(step)}}
			if rng.Intn(6) == 0 {
				r = Record{Collection: testCollection, Location: r.Location, ID: r.ID, Deleted: true}
			}
			if err := s.Apply([]Record{r}); err != nil {
				t.Fatal(err)
			}
			o.apply(r)
		}
	}
	write(3000)
	if err := writeBulk(s, bulkRecords(rng, 10), false, "first"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("bulk into a zone with records: %v", err)
	}
	for round := range 3 {
		batch := bulkRecords(rng, 300)
		if err := writeBulk(s, batch, true, fmt.Sprint("replace", round)); err != nil {
			t.Fatal(err)
		}
		for key, r := range o {
			if strings.HasPrefix(r.Location, "L") {
				delete(o, key)
			}
		}
		for _, r := range batch {
			o.apply(r)
		}
		checkStore(t, s, o, zones)
		if _, ok, _ := s.Get(Control, "@", fmt.Sprint("replace", round)); !ok {
			t.Fatal("the receipt of the bulk is not in the cut")
		}
		write(1500)
		checkStore(t, s, o, zones)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(dir, opt); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkStore(t, s, o, zones)
}

// AUTH-4 for a bulk: the power goes off at any disk operation of a bulk
// replace. The store reopens to the records before it, or to the records
// after it with its receipt; never to a part of it.
func TestPowerLossDuringBulkIsAllOrNothing(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	base, batch := oracle{}, bulkRecords(rng, 400)
	prepare := func() *memFS {
		fs := newMemFS()
		opt := crashOptions
		opt.FS = fs
		s, err := Open("", opt)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 60 {
			for _, m := range crashBatch(i) {
				base.apply(m)
			}
			if err := s.Apply(crashBatch(i)); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
		return fs
	}
	// A dry run counts the disk operations of the bulk.
	dry := prepare()
	after := oracle{}
	for key, r := range base {
		if !strings.HasPrefix(r.Location, "L") {
			after[key] = r
		}
	}
	for _, r := range batch {
		after.apply(r)
	}
	start := dry.Ops
	opt := crashOptions
	opt.FS = dry
	s, err := Open("", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBulk(s, batch, true, "bulk"); err != nil {
		t.Fatal(err)
	}
	s.work.Wait()
	total := dry.Ops - start
	rounds := 40
	if testing.Short() {
		rounds = 10
	}
	outcomes := map[bool]int{}
	for round := range rounds {
		fs := prepare()
		// The cut is anywhere in the bulk, or, every other round, around its
		// last operation, the commit: just before it, or once it is done.
		fs.FailAt = fs.Ops + 1 + rng.Intn(total)
		if round%2 == 1 {
			fs.FailAt = fs.Ops + total - 4 + rng.Intn(8)
		}
		opt.FS = fs
		committed := false
		if s, err := Open("", opt); err == nil {
			committed = writeBulk(s, batch, true, "bulk") == nil
			s.work.Wait()
		}
		opt.FS = fs.crash(rng, round%2 == 1)
		s, err := Open("", opt)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		_, done, _ := s.Get(Control, "@", "bulk")
		if committed && !done {
			t.Fatalf("round %d: an acknowledged bulk is lost", round)
		}
		want := base
		if done {
			want = after
		}
		checkStore(t, s, want, []string{"@", "L", "LL"})
		s.Close()
		outcomes[done]++
	}
	if outcomes[true] == 0 || outcomes[false] == 0 {
		t.Fatalf("the cuts never fell on both sides of the commit: %v", outcomes)
	}
	t.Logf("installed after %d cuts, absent after %d", outcomes[true], outcomes[false])
}

// Uncounted rows are read like records and add nothing to the sums, through
// replacements both ways, flushes, merges and a reopen.
func TestUncountedRowsAreReadAndNotSummed(t *testing.T) {
	dir := t.TempDir()
	opt := Options{MemoryBytes: 128 << 10, NoSync: true}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(9))
	counted, all := oracle{}, map[string]bool{}
	for step := range 4000 {
		r := Record{Collection: testCollection, Location: randomLocation(rng), ID: fmt.Sprint(rng.Intn(300)), Metrics: []float64{float64(step % 7)}}
		key := r.Location + "\x00" + r.ID
		switch rng.Intn(5) {
		case 0:
			r.Uncounted, r.Metrics = true, nil
			delete(counted, key)
			all[key] = true
		case 1:
			r = Record{Collection: testCollection, Location: r.Location, ID: r.ID, Deleted: true}
			delete(counted, key)
			delete(all, key)
		default:
			counted.apply(r)
			all[key] = true
		}
		if err := s.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		for _, zone := range []string{"@", "0", "L", "LgL"} {
			got, err := s.Children(testCollection, zone)
			if err != nil {
				t.Fatal(err)
			}
			want := counted.children(zone)
			for i := range want {
				if got[i].Count != want[i].Count || got[i].Only != want[i].Only {
					t.Fatalf("zone %q child %d: got %+v, want %+v", zone, i, got[i], want[i])
				}
			}
		}
		seen := 0
		s.Scan(testCollection, "@", nil, func(r Record) bool {
			if !all[r.Location+"\x00"+r.ID] {
				t.Fatalf("scan: %+v", r)
			}
			seen++
			return true
		})
		if seen != len(all) {
			t.Fatalf("scan saw %d of %d", seen, len(all))
		}
	}
	check()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	check()
	s.Close()
	if s, err = Open(dir, opt); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check()
}
