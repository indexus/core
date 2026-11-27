package storage

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/prep"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

const bulkCollection = "0000000000000001"

func bulkStore(t *testing.T, path string) (*Store, causal.Genesis) {
	t.Helper()
	s, err := OpenWithOptions(path, "D", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	g := causal.Genesis{Cluster: "test", Holder: h}
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, g); err != nil {
		t.Fatal(err)
	}
	return s, g
}

// bulkRow is a record of a test batch.
type bulkRow struct {
	Location, ID string
	Metrics      []float64
	Payload      []byte
}

// bulkRows are n sorted records at or below zone, each in its own zone.
func bulkRows(zone string, n, seed int) []bulkRow {
	rng := rand.New(rand.NewSource(int64(seed)))
	prefix := strings.TrimPrefix(zone, "@")
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	seen := map[string]bool{}
	var rows []bulkRow
	for len(rows) < n {
		loc := prefix
		for len(loc) < len(prefix)+3 {
			loc += string(chars[rng.Intn(64)])
		}
		if seen[loc] {
			continue
		}
		seen[loc] = true
		r := bulkRow{Location: loc, ID: fmt.Sprint("r", rng.Intn(9)), Metrics: []float64{float64(rng.Intn(100)), rng.Float64()}}
		if rng.Intn(3) == 0 {
			r.Payload = []byte(fmt.Sprintf(`{"seed":%d,"tag":"<b>"}`, seed))
		}
		rows = append(rows, r)
	}
	sortBulkRows(rows)
	return rows
}

// sortBulkRows puts rows in the store's key order: location bits, depth, id.
func sortBulkRows(rows []bulkRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, _ := prep.MakeHead(rows[i].Location)
		b, _ := prep.MakeHead(rows[j].Location)
		if c := bytes.Compare(a[:], b[:]); c != 0 {
			return c < 0
		}
		return rows[i].ID < rows[j].ID
	})
}

// writeBatchDir writes rows as an IXP1 batch, with zones.ixp from the root.
func writeBatchDir(t testing.TB, dir string, rows []bulkRow) {
	t.Helper()
	writeBatchDirOf(t, dir, bulkCollection, rows)
}

func writeBatchDirOf(t testing.TB, dir, collection string, rows []bulkRow) {
	t.Helper()
	w, err := prep.NewBatch(dir, prep.Options{Collection: collection, Metrics: 2, Payloads: true, SplitDepth: 1, PartBytes: 64 << 10, ZoneDepths: [2]int{0, 2},
		Inputs: prep.Inputs{Tool: "storage test", Sources: map[string]string{dir: fmt.Sprint(len(rows))}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		head, err := prep.MakeHead(r.Location)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Add(prep.Row{Head: head, ID: []byte(r.ID), Weight: 1, Metrics: r.Metrics, Payload: r.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func ingestDir(s *Store, dir string, replace bool) (BulkResult, error) {
	return s.IngestBulk(context.Background(), dir, replace)
}

// inventoryOf is the records of the collection the store serves, by key.
func inventoryOf(t *testing.T, s *Store) map[string]Record {
	t.Helper()
	snaps, err := s.Inventory(false)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Record{}
	for _, snap := range snaps {
		if snap.Zone.Collection != bulkCollection {
			continue
		}
		for _, r := range snap.Records {
			out[r.Location+"/"+r.ID] = r
		}
	}
	return out
}

func sameRecords(t *testing.T, got map[string]Record, rows []bulkRow) {
	t.Helper()
	if len(got) != len(rows) {
		t.Fatalf("%d records, want %d", len(got), len(rows))
	}
	for _, row := range rows {
		r, ok := got[row.Location+"/"+row.ID]
		want := Record{Collection: bulkCollection, Location: row.Location, ID: row.ID, Metrics: row.Metrics}
		if len(row.Payload) > 0 {
			raw, _ := json.Marshal(json.RawMessage(row.Payload))
			want.Payload = raw
		}
		a, _ := json.Marshal(r)
		b, _ := json.Marshal(want)
		if !ok || string(a) != string(b) {
			t.Fatalf("record %s/%s: %s, want %s", row.Location, row.ID, a, b)
		}
	}
}

// A batch is installed whole, served like written records, counted in the
// capacity usage exactly as writes count theirs, and the same batch again
// changes nothing. A later write over a bulk record replaces it.
func TestBulkIngestInstallsOnceAndCountsLikeWrites(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	rows := bulkRows("A", 3000, 1)
	dir := t.TempDir()
	writeBatchDir(t, dir, rows)
	result, err := ingestDir(s, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Already || result.Records != 3000 || result.Zones != 3000 || result.Bytes <= 0 {
		t.Fatalf("result %+v", result)
	}
	sameRecords(t, inventoryOf(t, s), rows)
	assertStoredUsage(t, s)
	stats, err := s.PlanningTotals()
	if err != nil || stats.Items != 3000 || stats.Zones != 3000 {
		t.Fatalf("planning totals %+v %v", stats, err)
	}
	revision, _ := s.Revision()
	again, err := ingestDir(s, dir, false)
	if err != nil || !again.Already || again.Batch != result.Batch {
		t.Fatalf("same batch again: %+v %v", again, err)
	}
	if after, _ := s.Revision(); after != revision {
		t.Fatal("a known batch committed something")
	}
	first := rows[0]
	_, previous, applied, err := s.ApplyRecord("over-bulk", Record{Collection: bulkCollection, Location: first.Location, ID: first.ID, Metrics: []float64{7, 7}})
	if err != nil || !applied || previous == nil || previous.Metrics[0] != first.Metrics[0] {
		t.Fatalf("write over a bulk record: previous %+v applied %v err %v", previous, applied, err)
	}
	rows[0].Metrics = []float64{7, 7}
	rows[0].Payload = nil
	sameRecords(t, inventoryOf(t, s), rows)
	assertStoredUsage(t, s)
	receipts, err := s.BulkReceipts()
	if err != nil || len(receipts) != 1 || receipts[0].Batch != result.Batch {
		t.Fatalf("receipts %+v %v", receipts, err)
	}
}

// A batch never lands over records it does not replace; with replace, the
// zone holds exactly the batch afterwards, and records outside it stay.
func TestBulkOverlapIsRefusedUnlessReplace(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	outside := Record{Collection: bulkCollection, Location: "B00", ID: "keep", Metrics: []float64{1, 1}}
	inside := Record{Collection: bulkCollection, Location: "A00", ID: "old", Metrics: []float64{1, 1}}
	for i, r := range []Record{outside, inside} {
		if _, err := s.Write(fmt.Sprint("w", i), r); err != nil {
			t.Fatal(err)
		}
	}
	rows := bulkRows("A", 500, 2)
	dir := t.TempDir()
	writeBatchDir(t, dir, rows)
	if _, err := ingestDir(s, dir, false); !errors.Is(err, ErrBulkOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	if got := inventoryOf(t, s); len(got) != 2 {
		t.Fatalf("a refused batch changed the store: %d records", len(got))
	}
	if _, err := ingestDir(s, dir, true); err != nil {
		t.Fatal(err)
	}
	got := inventoryOf(t, s)
	if _, ok := got["A00/old"]; ok {
		t.Fatal("replace kept a record the batch does not hold")
	}
	delete(got, "B00/keep")
	sameRecords(t, got, rows)
	assertStoredUsage(t, s)
}

// The node refuses a batch for zones another disk holds: after a handoff
// of its whole region, and on a disk that never held it. It also refuses any
// batch while a handoff is in progress. Nothing is installed.
func TestBulkIntoAZoneOwnedElsewhereIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeBatchDir(t, dir, bulkRows("A", 100, 3))
	refused := func(s *Store, want ...error) {
		t.Helper()
		_, err := ingestDir(s, dir, false)
		for _, w := range want {
			if errors.Is(err, w) {
				if receipts, _ := s.BulkReceipts(); len(receipts) != 0 || len(inventoryOf(t, s)) != 0 {
					t.Fatal("a refused batch left something")
				}
				return
			}
		}
		t.Fatalf("batch not refused as %v: %v", want, err)
	}

	source, g := bulkStore(t, filepath.Join(t.TempDir(), "d.db"))
	defer source.Close()
	target, err := OpenWithOptions(filepath.Join(t.TempDir(), "j.db"), "J", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	routing, _ := source.CausalRouting()
	root, _ := routing.Identity()
	if err := target.InitCausal(root, g); err != nil {
		t.Fatal(err)
	}
	refused(target, ErrNotOwner, ErrNotReady) // J is not active yet: it holds nothing
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	if _, err := source.ForceFreezeRegion(op.Cession.ID); err != nil {
		t.Fatal(err)
	}
	refused(source, ErrNotReady, ErrFrozen)
	ticketFinish(t, source, target, op.Cession.ID)
	refused(source, ErrNotOwner)
}

// A corrupt or reordered batch is refused after reading it, and leaves
// nothing: no record, no receipt, no usage.
func TestBulkCorruptBatchLeavesNothing(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	dir := t.TempDir()
	writeBatchDir(t, dir, bulkRows("A", 5000, 4))
	part := filepath.Join(dir, "part-00000.ixp")
	raw, _ := os.ReadFile(part)
	raw[len(raw)/2] ^= 1
	os.WriteFile(part, raw, 0o600)
	if _, err := ingestDir(s, dir, false); err == nil {
		t.Fatal("corrupt batch installed")
	}
	if got := inventoryOf(t, s); len(got) != 0 {
		t.Fatalf("%d records of a refused batch", len(got))
	}
	assertStoredUsage(t, s)
}

// The window between the zonelog cut and the control receipt: Open finds the
// receipt row and finishes the batch's accounting.
func TestBulkCutWithoutControlReceiptIsReconciledAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bulk.db")
	s, _ := bulkStore(t, path)
	dir := t.TempDir()
	rows := bulkRows("A", 800, 5)
	writeBatchDir(t, dir, rows)
	result, err := ingestDir(s, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Undo the control half, as a crash right after the zonelog cut would.
	err = s.update(func(tx *Tx) error {
		if err := tx.drop(key(metaBucket, bulkBucket)); err != nil {
			return err
		}
		usage, _ := s.storedDataUsage(tx)
		usage.Owned -= result.Bytes
		if err := tx.putJSON(meta(usageKey), usage); err != nil {
			return err
		}
		return tx.put(meta(rowBytesKey), number(uint64(storedRowBytes(tx)-result.Bytes)))
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenWithOptions(path, "D", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertStoredUsage(t, s)
	if receipts, _ := s.BulkReceipts(); len(receipts) != 1 || receipts[0].Batch != result.Batch {
		t.Fatalf("receipt not reconciled: %+v", receipts)
	}
	sameRecords(t, inventoryOf(t, s), rows)
	if again, err := ingestDir(s, dir, false); err != nil || !again.Already {
		t.Fatalf("after reconcile the batch is not known: %+v %v", again, err)
	}
}

// AUTH-4 for a bulk with real fsync: a process ingesting a batch is killed
// at random moments. Each reopen holds either none of the batch and no
// receipt, or all of it with its receipt, and usage equals what is stored;
// a batch the process reported done is there.
func TestKillDuringBulkIsAllOrNothing(t *testing.T) {
	if os.Getenv("BULK_CHILD") != "" {
		bulkChild(os.Getenv("BULK_CHILD"), os.Getenv("BULK_BATCH"))
		return
	}
	batch := t.TempDir()
	rows := bulkRows("A", 40000, 6)
	writeBatchDir(t, batch, rows)
	rng := rand.New(rand.NewSource(7))
	kills := 8
	if testing.Short() {
		kills = 3
	}
	outcomes := map[bool]int{}
	for round := range kills {
		path := filepath.Join(t.TempDir(), "bulk.db")
		s, _ := bulkStore(t, path)
		s.Close()
		cmd := exec.Command(os.Args[0], "-test.run", "^TestKillDuringBulkIsAllOrNothing$")
		cmd.Env = append(os.Environ(), "BULK_CHILD="+path, "BULK_BATCH="+batch)
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(out)
		lines.Scan() // "open": the store is open, the ingest starts
		done := make(chan bool, 1)
		go func() {
			reported := false
			for lines.Scan() {
				reported = reported || lines.Text() == "done"
			}
			done <- reported
		}()
		delay := time.Duration(rng.Intn(150)) * time.Millisecond
		if round == kills-1 {
			delay = 30 * time.Second // the last round runs to the end
		}
		finished := false
		select {
		case finished = <-done:
		case <-time.After(delay):
			cmd.Process.Signal(syscall.SIGKILL)
			finished = <-done
		}
		cmd.Wait()
		s, err := OpenWithOptions(path, "D", "test", 1<<30, Options{NoSync: true})
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		receipts, _ := s.BulkReceipts()
		got := inventoryOf(t, s)
		switch {
		case len(receipts) == 1:
			sameRecords(t, got, rows)
		case finished:
			t.Fatalf("round %d: the process reported done, the batch is not there", round)
		case len(got) != 0:
			t.Fatalf("round %d: %d records of a batch without receipt", round, len(got))
		}
		assertStoredUsage(t, s)
		outcomes[len(receipts) == 1]++
		s.Close()
	}
	if outcomes[true] == 0 {
		t.Fatalf("no round installed the batch: %v", outcomes)
	}
	t.Logf("installed in %d rounds, absent in %d", outcomes[true], outcomes[false])
}

func bulkChild(path, batch string) {
	s, err := OpenWithOptions(path, "D", "test", 1<<30, Options{})
	if err != nil {
		fmt.Println("open failed:", err)
		os.Exit(1)
	}
	fmt.Println("open")
	_, err = s.IngestBulk(context.Background(), batch, false)
	if err != nil {
		fmt.Println("failed:", err)
		os.Exit(1)
	}
	fmt.Println("done")
	s.Close()
	os.Exit(0)
}

// A batch sent as a stream is checked and installed as it comes; sent again
// it is known from its manifest alone; a stream cut short installs nothing.
func TestBulkStreamAnswersAKnownBatchEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bulk.db")
	s, _ := bulkStore(t, path)
	defer s.Close()
	dir := t.TempDir()
	rows := bulkRows("@", 2000, 9)
	writeBatchDir(t, dir, rows)
	var stream bytes.Buffer
	if err := PackBatch(&stream, dir); err != nil {
		t.Fatal(err)
	}
	whole := stream.Bytes()
	result, err := s.IngestBulkStream(context.Background(), bytes.NewReader(whole), false)
	if err != nil || result.Records != 2000 {
		t.Fatalf("%+v %v", result, err)
	}
	sameRecords(t, inventoryOf(t, s), rows)
	again, err := s.IngestBulkStream(context.Background(), bytes.NewReader(whole[:2048]), false)
	if err != nil || !again.Already || again.Batch != result.Batch {
		t.Fatalf("known batch from its manifest: %+v %v", again, err)
	}
	other := t.TempDir()
	writeBatchDir(t, other, bulkRows("@", 2000, 10))
	stream.Reset()
	if err := PackBatch(&stream, other); err != nil {
		t.Fatal(err)
	}
	cut := stream.Bytes()[:stream.Len()/2]
	if _, err := s.IngestBulkStream(context.Background(), bytes.NewReader(cut), true); !errors.Is(err, ErrBulkInvalid) {
		t.Fatalf("a stream cut short: %v", err)
	}
	sameRecords(t, inventoryOf(t, s), rows)
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.ixp")); len(left) != 0 {
		t.Fatalf("files left beside the store: %v", left)
	}
}

// HND-2 for bulk zones: a batch has no sequence and no history, so a
// handoff carries its records in the frozen snapshot alone. After the whole
// region moves, the receiver serves every record of the batch and of a
// later write, and the donor has collected them.
func TestHandoffCarriesBulkZones(t *testing.T) {
	source, g := bulkStore(t, filepath.Join(t.TempDir(), "d.db"))
	defer source.Close()
	rows := bulkRows("A", 300, 11)
	dir := t.TempDir()
	writeBatchDir(t, dir, rows)
	if _, err := ingestDir(source, dir, false); err != nil {
		t.Fatal(err)
	}
	written := Record{Collection: bulkCollection, Location: rows[5].Location, ID: rows[5].ID, Metrics: []float64{9, 9}}
	if _, err := source.Write("after-bulk", written); err != nil {
		t.Fatal(err)
	}
	rows[5].Metrics, rows[5].Payload = written.Metrics, nil
	target, err := OpenWithOptions(filepath.Join(t.TempDir(), "j.db"), "J", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	routing, _ := source.CausalRouting()
	root, _ := routing.Identity()
	if err := target.InitCausal(root, g); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	states, err := source.TransferZones(op.Cession.ID)
	if err != nil || len(states) != len(rows) {
		t.Fatalf("discovered %d zones, batch has %d: %v", len(states), len(rows), err)
	}
	ticketFinish(t, source, target, op.Cession.ID)
	if err := source.CollectTransferred(op.Cession.ID); err != nil {
		t.Fatal(err)
	}
	// The receiver serves installed zones before membership publication.
	// Check their contents directly, including the post-bulk update.
	got := map[string]Record{}
	for _, row := range rows {
		zone, _ := tree.NewZoneKey(bulkCollection, row.Location)
		snap, err := target.Snapshot(zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range snap.Records {
			got[r.Location+"/"+r.ID] = r
		}
	}
	sameRecords(t, got, rows)
	assertStoredUsage(t, target)
	assertStoredUsage(t, source)
	left := 0
	source.db.Scan(bulkCollection, "@", nil, func(zonelog.Record) bool { left++; return true })
	if left != 0 {
		t.Fatalf("the donor kept %d rows after collecting", left)
	}
}

// A producer whose zones.ixp does not sum its records is refused before the
// cut, as is a stream that names a file outside its batch.
func TestBulkChecksZoneSumsAndFileNames(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	dir := t.TempDir()
	writeBatchDir(t, dir, bulkRows("A", 400, 12))
	m, err := prep.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The root row of zones.ixp claims one record more.
	pr, f, err := prep.OpenFile(filepath.Join(dir, m.Zones.File))
	if err != nil {
		t.Fatal(err)
	}
	var zones []prep.Row
	pr.Each(func(r prep.Row) error {
		r.Metrics = append([]float64(nil), r.Metrics...)
		if r.Head.Depth() == 0 {
			r.Metrics[0]++
		}
		zones = append(zones, r)
		return nil
	})
	f.Close()
	info, err := prep.WriteZones(filepath.Join(dir, m.Zones.File), m.Metrics, zones)
	if err != nil {
		t.Fatal(err)
	}
	m.Zones.SHA256 = info.SHA256
	if err := prep.WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestDir(s, dir, false); !errors.Is(err, ErrBulkInvalid) || !strings.Contains(err.Error(), "does not sum") {
		t.Fatalf("wrong zone sums: %v", err)
	}
	if len(inventoryOf(t, s)) != 0 {
		t.Fatal("a refused batch left records")
	}
	m.Parts[0].File = "../escape.ixp"
	prep.WriteManifest(dir, m)
	var stream bytes.Buffer
	tw := tar.NewWriter(&stream)
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(raw))})
	tw.Write(raw)
	tw.Close()
	if _, err := s.IngestBulkStream(context.Background(), &stream, false); !errors.Is(err, ErrBulkInvalid) {
		t.Fatalf("a part outside the batch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.path), "escape.ixp")); err == nil {
		t.Fatal("a file was written outside the spool")
	}
}

// A pre-aggregated record counts as its weight: the zone sums add weights,
// a read gives the weight back, a weight of 1 is stored as none.
func TestBulkWeightsCountAsItems(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	rows := bulkRows("A", 200, 13)
	dir := t.TempDir()
	w, err := prep.NewBatch(dir, prep.Options{Collection: bulkCollection, Metrics: 2, SplitDepth: 1, ZoneDepths: [2]int{0, 1}, Inputs: prep.Inputs{Tool: "weights"}})
	if err != nil {
		t.Fatal(err)
	}
	items := 0.0
	for i, r := range rows {
		head, _ := prep.MakeHead(r.Location)
		weight := float64(1 + i%5)
		items += weight
		if err := w.Add(prep.Row{Head: head, ID: []byte(r.ID), Weight: weight, Metrics: r.Metrics}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestDir(s, dir, false); err != nil {
		t.Fatal(err)
	}
	total, err := s.ZoneTotal(bulkCollection, "@")
	if err != nil || total.Count != 200 || total.Weight != items {
		t.Fatalf("total %+v, want 200 records of %v items: %v", total, items, err)
	}
	got := inventoryOf(t, s)
	if r := got[rows[1].Location+"/"+rows[1].ID]; r.Weight != 2 {
		t.Fatalf("weight read back: %+v", r)
	}
	if r := got[rows[0].Location+"/"+rows[0].ID]; r.Weight != 0 {
		t.Fatalf("a weight of 1 is stored as none: %+v", r)
	}
	if _, err := s.Write("w", Record{Collection: bulkCollection, Location: "B", ID: "x", Weight: -1}); err == nil {
		t.Fatal("a negative weight was written")
	}
}

// A binary batch keeps its payloads as bytes: a read gives them as the
// base64 JSON string encoding/json makes of []byte, with Binary set. A write
// of such a record reads back the same, and a JSON check never runs on them.
func TestBulkBinaryPayloads(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	rows := bulkRows("A", 50, 2)
	dir := t.TempDir()
	w, err := prep.NewBatch(dir, prep.Options{Collection: bulkCollection, Metrics: 2, Payloads: true, PayloadEncoding: "binary", SplitDepth: 1, ZoneDepths: [2]int{0, 1}, Inputs: prep.Inputs{Tool: "binary"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		head, _ := prep.MakeHead(r.Location)
		if err := w.Add(prep.Row{Head: head, ID: []byte(r.ID), Metrics: r.Metrics, Payload: []byte{0xff, byte(i), '{'}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestDir(s, dir, false); err != nil {
		t.Fatal(err)
	}
	got := inventoryOf(t, s)
	r := got[rows[3].Location+"/"+rows[3].ID]
	if want, _ := json.Marshal([]byte{0xff, 3, '{'}); !r.Binary || string(r.Payload) != string(want) {
		t.Fatalf("binary payload read back as %+v", r)
	}
	write := Record{Collection: bulkCollection, Location: "B", ID: "w", Binary: true, Payload: json.RawMessage(`"AAEC"`)}
	if _, err := s.Write("bin", write); err != nil {
		t.Fatal(err)
	}
	if r := inventoryOf(t, s)["B/w"]; !r.Binary || string(r.Payload) != `"AAEC"` {
		t.Fatalf("binary write read back as %+v", r)
	}
	if _, err := s.Write("bad", Record{Collection: bulkCollection, Location: "B", ID: "x", Binary: true, Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a binary payload that is not base64 was written")
	}
	odd := t.TempDir()
	w, err = prep.NewBatch(odd, prep.Options{Collection: bulkCollection, Metrics: 2, Payloads: true, PayloadEncoding: "utf-16", SplitDepth: 1, ZoneDepths: [2]int{0, 1}, Inputs: prep.Inputs{Tool: "odd"}})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := prep.MakeHead("C")
	if err := w.Add(prep.Row{Head: head, ID: []byte("x"), Metrics: []float64{1, 2}, Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestDir(s, odd, false); !errors.Is(err, ErrBulkInvalid) {
		t.Fatalf("an unknown encoding: %v", err)
	}
}

// A record keeps up to MaxMetrics metrics, every one in the sums; a wider
// write or batch is refused, never stored without its metrics.
func TestMetricsUpToMaxAndNoWider(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "wide.db"))
	defer s.Close()
	wide := make([]float64, MaxMetrics)
	for i := range wide {
		wide[i] = float64(i)
	}
	if _, err := s.Write("wide", Record{Collection: bulkCollection, Location: "B", ID: "w", Metrics: wide}); err != nil {
		t.Fatal(err)
	}
	total, err := s.ZoneTotal(bulkCollection, "@")
	if err != nil || len(total.Metrics) != MaxMetrics || total.Metrics[MaxMetrics-1] != MaxMetrics-1 {
		t.Fatalf("a %d-metric record: %+v %v", MaxMetrics, total, err)
	}
	if _, err := s.Write("wider", Record{Collection: bulkCollection, Location: "B", ID: "x", Metrics: make([]float64, MaxMetrics+1)}); !errors.Is(err, ErrTooManyMetrics) {
		t.Fatalf("a wider write: %v", err)
	}
	dir := t.TempDir()
	w, err := prep.NewBatch(dir, prep.Options{Collection: bulkCollection, Metrics: MaxMetrics + 1, SplitDepth: 1, ZoneDepths: [2]int{0, -1}, Inputs: prep.Inputs{Tool: "wide"}})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := prep.MakeHead("C")
	if err := w.Add(prep.Row{Head: head, ID: []byte("x"), Weight: 1, Metrics: make([]float64, MaxMetrics+1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestDir(s, dir, false); !errors.Is(err, ErrBulkInvalid) {
		t.Fatalf("a wider batch: %v", err)
	}
}
