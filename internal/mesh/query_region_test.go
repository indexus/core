package mesh

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

const queryAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

func rowSet(t *testing.T, snaps []storage.Snapshot, collection, prefix string) []string {
	t.Helper()
	var out []string
	for _, snap := range snaps {
		for _, r := range snap.Records {
			if !r.Deleted && r.Collection == collection && (prefix == "@" || len(r.Location) >= len(prefix) && r.Location[:len(prefix)] == prefix) {
				out = append(out, r.Location+"/"+r.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// PREFIX-1 in the read path: a prefix read scoped to the prefix's XOR subtree
// returns exactly what asking every holder returns, and a full-length location
// costs one host.
func TestPREFIX1_ScopedPrefixReadsMatchAskingEveryone(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 6)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(3, 3))
	word := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = queryAlphabet[rng.IntN(len(queryAlphabet))]
		}
		return string(b)
	}
	collection := "qcoll00000000001"
	var locations []string
	// Shared short prefixes, so several depths hold rows.
	stems := []string{word(1), word(2), word(3)}
	for i := 0; i < 300; i++ {
		loc := stems[i%len(stems)] + word(16-len(stems[i%len(stems)]))
		if i%5 == 0 {
			loc = stems[i%len(stems)] + word(rng.IntN(4))
		}
		locations = append(locations, loc)
		record := storage.Record{Collection: collection, Location: loc, ID: fmt.Sprint("r", i)}
		if _, err := nodes[0].Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprint(i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range nodes[1:] {
		if err := node.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		for _, node := range nodes {
			var hosts []string
			for _, other := range nodes {
				hosts = append(hosts, other.Self.Host)
			}
			if err := node.Sync(ctx, hosts); err != nil {
				t.Fatal(err)
			}
			if err := node.Resume(ctx); err != nil {
				t.Fatal(err)
			}
			// Membership is a placement hint and joins have no global finish
			// barrier. Reconcile each member after gossip before checking the
			// stable one-host prefix cost.
			if err := node.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	reader := nodes[3]
	everyone := func(prefix string) []string {
		var holders []causal.Endpoint
		for _, node := range nodes {
			holders = append(holders, node.Self)
		}
		zone, err := storageZone(collection, prefix)
		if err != nil {
			t.Fatal(err)
		}
		snaps, err := reader.queryHolders(ctx, holders, Request{Kind: KindInventoryPrefix, Zone: zone})
		if err != nil {
			t.Fatal(err)
		}
		return rowSet(t, snaps, collection, prefix)
	}
	prefixes := []string{"@"}
	prefixes = append(prefixes, stems...)
	for _, loc := range locations[:40] {
		prefixes = append(prefixes, loc, loc[:min(len(loc), 1+rng.IntN(len(loc)))])
	}
	for _, prefix := range prefixes {
		want := everyone(prefix)
		net.mu.Lock()
		net.hosts = nil
		net.mu.Unlock()
		got, err := reader.QueryPrefix(ctx, collection, prefix)
		if err != nil {
			t.Fatal(err)
		}
		net.mu.Lock()
		asked := len(net.hosts)
		net.mu.Unlock()
		if g := rowSet(t, got, collection, prefix); fmt.Sprint(g) != fmt.Sprint(want) {
			t.Fatalf("prefix %q: scoped read %d rows, everyone %d", prefix, len(g), len(want))
		}
		if len(prefix) == 16 && asked > 1 {
			t.Fatalf("full-length location %q asked %d hosts", prefix, asked)
		}
	}
}

func storageZone(collection, location string) (tree.ZoneKey, error) {
	return tree.NewZoneKey(collection, location)
}
