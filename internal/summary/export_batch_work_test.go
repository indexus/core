package summary

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
)

func exportBatchFixture(t testing.TB, added int) (*Tree, *exportKeys, storage.ZoneInventory) {
	t.Helper()
	tr := inventoryTree(t)
	write := func(i int) {
		_, err := tr.service.Store.Write(fmt.Sprint("batch-", i), storage.Record{
			Collection: fmt.Sprintf("Batch%011d", i), Location: "1000000000000000", ID: "r",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range 256 {
		write(i)
	}
	tr.exportKeysFor("", false)
	cached := tr.exportKeys
	if cached == nil {
		t.Fatal("fixture must retain its inventory")
	}
	for i := range added {
		write(256 + i)
	}
	inventory, err := tr.service.Store.ZoneInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tr, cached, inventory
}

func TestBoundaryBatchKeepsPublishedPagesImmutable(t *testing.T) {
	tr, cached, inventory := exportBatchFixture(t, 128)
	published := slices.Clone(cached.keys)
	next, ok := tr.extendExportKeys(cached, inventory)
	keys := next.forCollection("")
	if !ok || len(keys) != len(published)+128 {
		t.Fatalf("extension returned %d boundaries, want %d; ok=%v", len(keys), len(published)+128, ok)
	}
	if !reflect.DeepEqual(cached.keys, published) {
		t.Fatal("extending the inventory changed a previously published page")
	}
}

func BenchmarkBoundaryBatchExtension(b *testing.B) {
	for _, added := range []int{16, 128} {
		b.Run(fmt.Sprint(added), func(b *testing.B) {
			tr, cached, inventory := exportBatchFixture(b, added)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, ok := tr.extendExportKeys(cached, inventory); !ok {
					b.Fatal("extension refused added zones")
				}
			}
		})
	}
}
