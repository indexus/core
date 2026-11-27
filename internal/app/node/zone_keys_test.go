package node

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// Keys of a zone (docs/protocol/query.md): whoever answers a /sets read, the
// owner from its summary, the owner from a scan (refresh) or a relay from the
// holders' records, the IXB2 body is the same byte for byte. A child whose
// records all sit at one location is named by that location, at most four
// characters below the zone; any other child by its one-character key.
func TestZoneKeysAreTheSameFromOwnerAndRelay(t *testing.T) {
	first := startDataNode(t, "0000000000000001", nil)
	second := startDataNode(t, "F000000000000000", first)
	third := startDataNode(t, "U000000000000000", first)
	nodes := []*runningDataNode{first, second, third}

	// Lone deep records, several records at one deep location, records at a
	// zone that also has children, a location deeper than the key depth, and
	// lone records spread over the alphabet so some sit on another node than
	// the parent that names them.
	locations := []string{"aaZ9", "aab", "aab", "aac", "aa", "aa", "aacK7", "Fq", "Fq3", "Uz81", "xy12", "xy12", "pQrStUv"}
	random := rand.New(rand.NewSource(3))
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	// Their first character avoids the zones checked by name below.
	firsts := "0123456789BCDEGHIJKLMNOPQRSTVWXYZbcdefghijklmnoqrstuvwz-_"
	for range 40 {
		location := []byte{firsts[random.Intn(len(firsts))], alphabet[random.Intn(64)], alphabet[random.Intn(64)], alphabet[random.Intn(64)], alphabet[random.Intn(64)]}
		locations = append(locations, string(location))
	}
	for i, location := range locations {
		// Sums of these values are exact, so the order of addition is not seen.
		item := &indexus.Item{
			Collection: "Demo000000000000", Location: location, Id: fmt.Sprintf("record-%d", i),
			RequestID: fmt.Sprintf("put-%d", i), Metrics: []float64{float64(i) + 0.5, 3, 0.25},
		}
		if err := nodes[i%len(nodes)].node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	ownerOf := func(zone string) *runningDataNode {
		t.Helper()
		for _, n := range nodes {
			_, set, err := n.node.Get("Demo000000000000", zone, false, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if set != nil {
				return n
			}
		}
		t.Fatalf("%s has no owner", zone)
		return nil
	}

	zones := []string{"@", "a", "aa", "aaZ", "aaZ9", "aab", "aac", "aacK", "F", "Fq", "U", "Uz", "x", "xy12", "p", "pQ", "pQrStU"}
	for _, location := range locations[13:] {
		zones = append(zones, location[:1], location[:2])
	}
	remoteExact := 0
	for _, zone := range zones {
		query := "location=" + zone + "&metrics=f64"
		owner := ownerOf(zone)
		// A scan reads the durable records: it is the reference. The owner's
		// summary of a parent waits for its children's pushes.
		want, _ := fetchSets(t, owner, query+"&refresh=true")
		deadline := time.Now().Add(15 * time.Second)
		for _, n := range nodes {
			for {
				got, _ := fetchSets(t, n, query)
				if bytes.Equal(got, want) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s from %s (owner %s):\n got %x\nwant %x", zone, n.node.Name(), owner.node.Name(), got, want)
				}
				for _, peer := range nodes {
					if err := peer.node.Update(); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
		}

		block, err := wire.DecodeSetsBlock(want)
		if err != nil {
			t.Fatal(err)
		}
		path := strings.TrimPrefix(zone, "@")
		for key := range block.Entries() {
			if !strings.HasPrefix(key, path) || len(key) > len(path)+4 {
				t.Fatalf("%s answers key %q", zone, key)
			}
			if len(key) > len(path)+1 && ownerOf(key) != owner {
				remoteExact++
			}
		}
		entries := block.Entries()
		switch zone {
		case "aa":
			if len(entries) != 4 || entries["aa"].Count() != 2 || entries["aab"].Count() != 2 ||
				entries["aac"].Count() != 2 || entries["aaZ9"].Count() != 1 {
				t.Fatalf("aa = %v", entries)
			}
		case "x":
			if entries["xy12"] == nil || entries["xy12"].Count() != 2 {
				t.Fatalf("x = %v", entries)
			}
		case "@":
			if entries["pQrS"] == nil || entries["Uz81"] == nil {
				t.Fatalf("@ = %v", entries)
			}
		case "p":
			if entries["pQrSt"] == nil || len(entries) != 1 {
				t.Fatalf("p = %v", entries)
			}
		case "pQrStU":
			if entries["pQrStUv"] == nil || len(entries) != 1 {
				t.Fatalf("pQrStU = %v", entries)
			}
		}
	}
	// The parent learns a remote child's one location from its push.
	if remoteExact == 0 {
		t.Fatal("no deep key named a location held by another node")
	}
	t.Logf("%d deep keys name a location held by another node", remoteExact)
}
