package indexus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/prep"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// A batch for the zones one node holds is installed there, counted in its
// summaries, and reaches the parent zone held by the other node in one push
// round: every boundary once, in batched contributions. A batch for zones
// the node does not hold is refused, and the same batch again changes
// nothing.
func TestBulkIngestReachesTheParentOnce(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	const collection = "0000000000000001"
	view := f.nodes[0].mesh.summary.View()
	root, _ := NewZoneKey(collection, "@")
	rootOwner, _ := view.OwnerName(root.ID)
	parent, child := f.nodes[0], f.nodes[1]
	if parent.Name() != rootOwner {
		parent, child = child, parent
	}
	// Records in the depth-1 zones the child holds, three levels deep.
	var mine, theirs []testBulkRow
	for _, a := range ChildCandidates(BASE64, "@") {
		z, _ := NewZoneKey(collection, a)
		owner, _ := view.OwnerName(z.ID)
		for _, b := range ChildCandidates(BASE64, a)[:8] {
			row := testBulkRow{b + "0", []float64{1, 2}}
			if owner == child.Name() {
				mine = append(mine, row)
			} else {
				theirs = append(theirs, row)
			}
		}
	}
	pack := func(rows []testBulkRow) *bytes.Buffer {
		t.Helper()
		return packTestBatch(t, collection, rows)
	}
	var pushMu sync.Mutex
	pushes, contributions := 0, 0
	f.net.before = func(r mesh.Request) error {
		if r.Kind == mesh.KindSummaryContribute {
			pushMu.Lock()
			pushes++
			contributions += len(r.Contributions)
			pushMu.Unlock()
		}
		return nil
	}
	if _, err := child.IngestBulk(ctx, pack(theirs), false); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a batch for zones the other node holds: %v", err)
	}
	stream := pack(mine)
	again := bytes.NewReader(stream.Bytes())
	result, err := child.IngestBulk(ctx, stream, false)
	if err != nil || result.Records != int64(len(mine)) {
		t.Fatalf("%+v %v", result, err)
	}
	child.mesh.summary.FlushPendingEdges()
	if got := child.mesh.summary.Owned(collection, "@"); got != nil && got.Count() != 0 {
		// The child does not hold the root; only the parent sums it.
		t.Fatalf("child counts the root: %v", got)
	}
	got := parent.mesh.summary.Owned(collection, "@")
	if got == nil || got.Count() != len(mine) || got.Metric(1) != float64(2*len(mine)) {
		t.Fatalf("parent root after the bulk: %v, want %d records", got, len(mine))
	}
	boundaries := map[string]bool{}
	for _, r := range mine {
		boundaries[r.location[:1]] = true
	}
	if contributions != len(boundaries) || pushes == 0 || pushes > 1+contributions/8192 {
		t.Fatalf("%d pushes carried %d contributions for %d boundaries", pushes, contributions, len(boundaries))
	}
	if result, err := child.IngestBulk(ctx, again, false); err != nil || !result.Already {
		t.Fatalf("same batch again: %+v %v", result, err)
	}
	t.Logf("%d records in %d zones below %d boundaries: %d push(es)", len(mine), result.Zones, len(boundaries), pushes)
}

// A node joins after a bulk: the handoff moves bulk zones like written ones,
// and the root sums the batch exactly once across both nodes.
func TestJoinAfterBulkMovesItsZones(t *testing.T) {
	t.Parallel()
	testJoinAfterBulkMovesItsZones(t, false)
}

func TestAuthorityCheckpointKeepsRootTotalAfterBulkJoin(t *testing.T) {
	t.Parallel()
	testJoinAfterBulkMovesItsZones(t, true)
}

func testJoinAfterBulkMovesItsZones(t *testing.T, compact bool) {
	t.Helper()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	t.Cleanup(func() {
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	const collection = "0000000000000001"
	var rows []testBulkRow
	for _, a := range ChildCandidates(BASE64, "@") {
		for _, b := range ChildCandidates(BASE64, a)[:4] {
			rows = append(rows, testBulkRow{b, []float64{1, 3}})
		}
	}
	stream := packTestBatch(t, collection, rows)
	if _, err := f.nodes[0].IngestBulk(ctx, stream, false); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	if compact {
		for _, service := range f.services {
			if err := service.Resume(ctx); err != nil {
				t.Fatal(err)
			}
			state, err := service.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			if err := state.Permissions.Validate(service.Self.Owner); err != nil {
				t.Fatalf("invalid local permissions: %v", err)
			}
		}
		for _, service := range f.services {
			if err := service.Sync(ctx, []string{f.services[0].Self.Host, f.services[1].Self.Host}); err != nil {
				t.Fatal(err)
			}
		}
	}
	held := 0
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
		if _, err := n.mesh.summary.Fold(ctx, true); err != nil {
			t.Fatal(err)
		}
		inventory, err := n.mesh.service.Store.Inventory(false)
		if err != nil {
			t.Fatal(err)
		}
		mine := 0
		for _, snap := range inventory {
			if snap.Zone.Collection != collection {
				continue
			}
			mine += len(snap.Records)
		}
		if mine == 0 {
			t.Fatalf("%s holds no record: the join moved nothing", n.Name())
		}
		held += mine
	}
	if held != len(rows) {
		t.Fatalf("the two nodes serve %d records, the batch has %d", held, len(rows))
	}
	for _, n := range f.nodes {
		n.mesh.summary.FlushPendingEdges()
	}
	root, _ := NewZoneKey(collection, "@")
	owner, _ := f.nodes[0].mesh.summary.View().OwnerName(root.ID)
	for _, n := range f.nodes {
		if n.Name() != owner {
			continue
		}
		if err := n.mesh.summary.PollRemoteHosts(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if got := n.mesh.summary.Owned(collection, "@"); got == nil || got.Count() != len(rows) || got.Metric(1) != float64(3*len(rows)) {
			t.Fatalf("root after the join: %v, want %d records", got, len(rows))
		}
	}
}

// SUM-4: the source of a join still holds the rows it handed away until its
// collection deletes them. A zone's total reads its local rows, then the rows
// below each remote child to replace them with that child's contribution. If
// the collection commits between the two reads, the first counts the handed
// rows and the second no longer subtracts them: they were counted twice
// (TestJoinAfterBulkMovesItsZones, about one run in three). Both reads must
// see one committed version.
func TestRootTotalReadsOneVersionWhileCollectionDeletesHandedRows(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	t.Cleanup(func() {
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	const collection = "0000000000000001"
	var rows []testBulkRow
	for _, a := range ChildCandidates(BASE64, "@") {
		for _, b := range ChildCandidates(BASE64, a)[:4] {
			rows = append(rows, testBulkRow{b, []float64{1, 3}})
		}
	}
	if _, err := f.nodes[0].IngestBulk(ctx, packTestBatch(t, collection, rows), false); err != nil {
		t.Fatal(err)
	}
	// The source keeps the rows it hands away until the test collects them.
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
		if _, err := n.mesh.summary.Fold(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range f.nodes {
		n.mesh.summary.FlushPendingEdges()
	}
	root, _ := NewZoneKey(collection, "@")
	owner, _ := f.nodes[0].mesh.summary.View().OwnerName(root.ID)
	if owner != f.nodes[0].Name() {
		t.Fatalf("fixture: the root is owned by %s, not by the source", owner)
	}
	source := f.nodes[0]
	if err := source.mesh.summary.PollRemoteHosts(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if local, err := source.mesh.service.Store.ZoneTotal(collection, "@"); err != nil || local.Count != int64(len(rows)) {
		t.Fatalf("fixture: the source must still hold every row before collection: %d, %v", local.Count, err)
	}
	var collected atomic.Bool
	source.mesh.summary.SetBetweenLocalReadsForTest(func() {
		if collected.CompareAndSwap(false, true) {
			if err := f.services[0].Resume(ctx); err != nil { // collects the handed rows
				t.Error(err)
			}
		}
	})
	got := source.mesh.summary.Owned(collection, "@")
	source.mesh.summary.SetBetweenLocalReadsForTest(nil)
	if !collected.Load() {
		t.Fatal("fixture: the collection did not run between the two reads")
	}
	if local, _ := source.mesh.service.Store.ZoneTotal(collection, "@"); local.Count == int64(len(rows)) {
		t.Fatal("fixture: the collection deleted nothing")
	}
	if got == nil || got.Count() != len(rows) || got.Metric(1) != float64(3*len(rows)) {
		t.Fatalf("root total with a collection between its reads: %v, want %d records", got, len(rows))
	}
}

// testBulkRow is a record of a test batch: id "r" at a location.
type testBulkRow struct {
	location string
	metrics  []float64
}

// packTestBatch writes rows as an IXP1 batch and returns its tar stream.
func packTestBatch(t *testing.T, collection string, rows []testBulkRow) *bytes.Buffer {
	t.Helper()
	heads := make([]prep.Head, len(rows))
	for i, r := range rows {
		var err error
		if heads[i], err = prep.MakeHead(r.location); err != nil {
			t.Fatal(err)
		}
	}
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return bytes.Compare(heads[order[i]][:], heads[order[j]][:]) < 0 })
	dir := t.TempDir()
	w, err := prep.NewBatch(dir, prep.Options{Collection: collection, Metrics: 2, SplitDepth: 1, ZoneDepths: [2]int{0, 1}, Inputs: prep.Inputs{Tool: "indexus test", Sources: map[string]string{dir: ""}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range order {
		if err := w.Add(prep.Row{Head: heads[i], ID: []byte("r"), Weight: 1, Metrics: rows[i].metrics}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := storage.PackBatch(&stream, dir); err != nil {
		t.Fatal(err)
	}
	return &stream
}

// A write of a pre-aggregated record counts its weight in the zone, on
// either engine: the radix by delta, the store by its sums.
func TestWeightedRecordsCountTheirItems(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	ctx := context.Background()
	for i, weight := range []float64{3, 0, 2.0} {
		record := storage.Record{Collection: "Demo000000000000", Location: "ab", ID: fmt.Sprint(i), Metrics: []float64{1}, Weight: weight}
		if _, err := n.MeshData(ctx, mesh.Request{Kind: "write", RequestID: fmt.Sprint(i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	if !n.mesh.summary.Ensure(context.Background()) {
		t.Fatal("not ready")
	}
	if got := n.mesh.summary.Owned("Demo000000000000", "@"); got.Count() != 6 || got.Metric(0) != 3 {
		t.Fatalf("root: %v, want 6 items", got)
	}
}
