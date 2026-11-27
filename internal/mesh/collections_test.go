package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/algebra"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func testDescription(t *testing.T, nonce byte) (string, []byte) {
	t.Helper()
	d := &algebra.Descriptor{
		BitsPerChar:  6,
		Width:        16,
		Nonce:        [16]byte{nonce},
		Space:        algebra.Space{Axes: []algebra.Axis{{Min: 0, Max: 1 << 20}, {Min: 0, Max: 1 << 20}}, Schedule: algebra.RoundRobin(2, 96)},
		RecordDepths: [2]int{1, 16},
		Schema:       algebra.Schema{algebra.Sum, algebra.Max, algebra.Last},
	}
	b, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := d.Name()
	return name, b
}

// joinAll makes every node of a fixture active, so that each one holds a grant.
func joinAll(t *testing.T, nodes []*Service) {
	t.Helper()
	ctx := context.Background()
	var hosts []string
	for _, node := range nodes {
		hosts = append(hosts, node.Self.Host)
	}
	for _, node := range nodes[1:] {
		if err := node.Join(ctx); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			for _, n := range nodes {
				if err := n.Sync(ctx, hosts); err != nil {
					t.Fatal(err)
				}
				if err := n.Resume(ctx); err != nil {
					t.Fatal(err)
				}
				if err := n.Rebalance(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, node := range nodes {
		others, err := node.viewHosts()
		if err != nil || len(others) != len(nodes)-1 {
			t.Fatalf("%s sees %v (%v)", node.Self.Host, others, err)
		}
	}
}

func TestDescriptionIsAdmittedOnEveryHostBeforeItIsAcknowledged(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	joinAll(t, nodes)
	ctx := context.Background()
	name, b := testDescription(t, 1)

	held, err := nodes[0].AdmitEverywhere(ctx, name, b)
	if err != nil || held != 3 {
		t.Fatalf("held by %d: %v", held, err)
	}
	for _, node := range nodes {
		if c, err := node.Store.CollectionOf(name); err != nil || c == nil || c.Digest != algebra.Digest(b) {
			t.Fatalf("%s does not hold the description: %v", node.Self.Host, err)
		}
	}

	// A host out of reach: the admission is not acknowledged, and a retry
	// once it is back completes it.
	other, ob := testDescription(t, 2)
	net.setBlocked(nodes[2].Self.Host, true)
	if held, err := nodes[1].AdmitEverywhere(ctx, other, ob); err == nil || held != 2 {
		t.Fatalf("acknowledged without every host: %d %v", held, err)
	}
	net.setBlocked(nodes[2].Self.Host, false)
	if held, err := nodes[1].AdmitEverywhere(ctx, other, ob); err != nil || held != 3 {
		t.Fatalf("retry: %d %v", held, err)
	}

	// Bytes that are not the name's hash are refused by the first disk.
	if _, err := nodes[0].AdmitEverywhere(ctx, name, ob); !errors.Is(err, algebra.ErrName) {
		t.Fatalf("wrong bytes for a name: %v", err)
	}
}

func TestADiskFetchesAnUnknownDescriptionFromAPeer(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	ctx := context.Background()
	name, b := testDescription(t, 3)
	if _, err := nodes[0].Store.AdmitCollection(name, b); err != nil {
		t.Fatal(err)
	}
	c, err := nodes[1].FetchCollection(ctx, nodes[0].Self.Host, name)
	if err != nil || c == nil || c.Digest != algebra.Digest(b) {
		t.Fatalf("fetch: %v", err)
	}
	if held, _ := nodes[1].Store.CollectionOf(name); held == nil {
		t.Fatalf("fetched description not admitted")
	}
	missing, _ := testDescription(t, 4)
	if _, err := nodes[0].FetchCollection(ctx, nodes[1].Self.Host, missing); err == nil {
		t.Fatalf("fetched a description nobody holds")
	}
}

func TestAJoiningDiskReceivesTheCatalogWithItsZones(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	ctx := context.Background()
	name, b := testDescription(t, 5)
	// Only the first disk holds the description, as after an admission that
	// happened before the second disk existed.
	if _, err := nodes[0].Store.AdmitCollection(name, b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		r := storage.Record{Collection: name, Location: alphabet64[i : i+1], ID: fmt.Sprint(i), Metrics: []float64{1, float64(i), float64(i)}}
		if _, err := nodes[0].Write(ctx, fmt.Sprint("w", i), r); err != nil {
			t.Fatal(err)
		}
	}
	joinAll(t, nodes)
	c, err := nodes[1].Store.CollectionOf(name)
	if err != nil || c == nil || c.Digest != algebra.Digest(b) {
		t.Fatalf("the joined disk holds zones of %s without its description (%v)", name, err)
	}
}

const alphabet64 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
