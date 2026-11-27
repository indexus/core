package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestZoneInventoryTracksShapeWithoutReadingSumsOrControlCommits(t *testing.T) {
	s := openTestData(t, filepath.Join(t.TempDir(), "catalog"), "D")
	defer s.Close()
	initTestCausal(t, s)
	ctx := context.Background()
	capture := func() ZoneInventory {
		t.Helper()
		v, err := s.ZoneInventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	keys := func(v ZoneInventory) []tree.Key {
		t.Helper()
		var out []tree.Key
		if err := v.Each(ctx, func(z tree.ZoneKey) bool {
			out = append(out, tree.Key{Collection: z.Collection, Location: z.Location})
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	empty := capture()
	if err := s.update(func(tx *Tx) error { return tx.put(meta([]byte("unrelated")), []byte("metadata")) }); err != nil {
		t.Fatal(err)
	}
	if capture().Revision() != empty.Revision() {
		t.Fatal("control commit invalidated canonical shape")
	}
	const collection = "Demo000000000000"
	write := func(request, location string, deleted bool) {
		t.Helper()
		if _, err := s.Write(request, Record{Collection: collection, Location: location, ID: "one", Deleted: deleted}); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "A", false)
	a := capture()
	if a.Revision() == empty.Revision() || len(keys(a)) != 1 || len(keys(empty)) != 0 {
		t.Fatal("creation or immutable old catalog failed")
	}
	write("replace", "A", false)
	if capture().Revision() != a.Revision() {
		t.Fatal("replacement changed zone identities")
	}
	write("delete", "A", true)
	if capture().Revision() != a.Revision() || len(keys(capture())) != 1 {
		t.Fatal("tombstone must remain a conservative candidate")
	}
	write("b", "B", false)
	b := capture()
	if b.Revision() == a.Revision() || len(keys(b)) != 2 || len(keys(a)) != 1 {
		t.Fatal("new zone did not publish a new immutable catalog")
	}
	z, _ := tree.NewZoneKey(collection, "A")
	if err := s.update(func(tx *Tx) error { return tx.dropZone(z) }); err != nil {
		t.Fatal(err)
	}
	after := capture()
	if after.Revision() == b.Revision() || !reflect.DeepEqual(keys(after), []tree.Key{{Collection: collection, Location: "B"}}) {
		t.Fatal("canonical zone removal did not invalidate shape")
	}
	beforeRows := s.spatial.buildRows.Load()
	for range 20 {
		keys(capture())
	}
	if s.spatial.buildRows.Load() != beforeRows {
		t.Fatal("hot inventory rescanned record headers")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := after.Each(cancelled, func(tree.ZoneKey) bool { return true }); err == nil {
		t.Fatal("cancelled inventory ignored its budget")
	}
}
