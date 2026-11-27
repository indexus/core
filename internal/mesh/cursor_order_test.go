package mesh

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// A paged read moves through zones in the store's key order: the routing
// alphabet puts '-' and '_' after the letters, unlike Go's string order. A
// read of a collection whose zones cross from "v..." to "-..." was refused as
// "inventory changed" (the weather's clouds, 503 on every load).
func TestInventoryCursorAdvancesInKeyOrder(t *testing.T) {
	t.Parallel()
	at := func(location, id string) storage.InventoryCursor {
		return storage.InventoryCursor{Collection: "CloudAROME0128v2", Location: location, RecordID: id, Watermark: 1}
	}
	for _, c := range []struct {
		prev, next storage.InventoryCursor
		want       bool
	}{
		{at("v4HdAwn7VUKej0rf", "m128-64-48"), at("-Ptg6-LmpAwp_SJI", "m128-99-67"), true},
		{at("-Ptg6-LmpAwp_SJI", "a"), at("v4HdAwn7VUKej0rf", "a"), false},
		{at("4nB8SUBIr2wHPcHS", "m"), at("v4HdAwn7VUKej0rf", "m"), true},
		{at("ab", "z"), at("ab0", "a"), true},
		{at("ab", "a"), at("ab", "b"), true},
		{at("ab", "b"), at("ab", "b"), false},
	} {
		if got := inventoryCursorAdvances(c.prev, c.next); got != c.want {
			t.Errorf("%s/%s -> %s/%s: advances = %v, want %v", c.prev.Location, c.prev.RecordID, c.next.Location, c.next.RecordID, got, c.want)
		}
	}
}
