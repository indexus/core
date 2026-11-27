package indexus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/algebra"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// typedSchema has a column of every kind.
var typedSchema = algebra.Schema{algebra.Sum, algebra.Min, algebra.Max, algebra.First, algebra.Last, algebra.AffineA, algebra.AffineB}

func typedDescription(t *testing.T) (string, []byte, *algebra.Descriptor) {
	t.Helper()
	d := &algebra.Descriptor{BitsPerChar: 6, Width: 16, Nonce: [16]byte{'t'}, RecordDepths: [2]int{1, 4}, Schema: typedSchema}
	b, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := d.Name()
	return name, b, d
}

// expectedRoot folds the live records in cell order, one by one: the value
// every node must report for the root, whoever holds which zone.
func expectedRoot(d *algebra.Descriptor, name string, live map[string]storage.Record) algebra.Value {
	type rec struct {
		key []byte
		id  string
		v   algebra.Value
	}
	var all []rec
	for _, r := range live {
		all = append(all, rec{algebra.CellOrder(d.BitsPerChar, name, r.Location), r.ID, d.Schema.Lift(r.Metrics)})
	}
	sort.Slice(all, func(i, j int) bool {
		if c := bytes.Compare(all[i].key, all[j].key); c != 0 {
			return c < 0
		}
		return all[i].id < all[j].id
	})
	acc := d.Schema.Zero()
	for _, r := range all {
		acc = d.Schema.Combine(acc, r.v)
	}
	return acc
}

func sameTyped(got *Abelian, want algebra.Value) error {
	if int64(got.Count()) != want.Count {
		return fmt.Errorf("count %d, want %d", got.Count(), want.Count)
	}
	for i, k := range typedSchema {
		g, w := got.Metric(i), want.Cols[i]
		exact := k == algebra.Min || k == algebra.Max || k == algebra.First || k == algebra.Last
		if exact && g != w || !exact && math.Abs(g-w) > 1e-9*math.Max(1, math.Abs(w)) {
			return fmt.Errorf("column %d (%s) = %v, want %v", i, k, g, w)
		}
	}
	return nil
}

func TestTypedCollectionAcrossVnodesMatchesASequentialFold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	descriptors := []VNodeDescriptor{{ID: ID{255}}, {ID: ID{128}}, {ID: ID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}}}
	for i := range descriptors {
		descriptors[i].Name = BASE64.Encode(descriptors[i].ID.Bytes())
	}
	f := newReadPhaseFixture(t, descriptors, false, true)
	hosts := []string{}
	for _, service := range f.services {
		hosts = append(hosts, service.Self.Host)
	}
	for _, i := range []int{1, 2} {
		if err := f.services[i].Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, service := range f.services {
		if err := service.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
	name, b, d := typedDescription(t)
	if held, err := f.nodes[0].AdmitCollection(ctx, name, b); err != nil || held != 3 {
		t.Fatalf("admission held by %d: %v", held, err)
	}
	if got, err := f.nodes[2].CollectionDescription(name); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("description on another node: %v", err)
	}

	rng := rand.New(rand.NewSource(7))
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	live := map[string]storage.Record{}
	write := func(r storage.Record) {
		t.Helper()
		for {
			_, err := f.nodes[rng.Intn(3)].MeshData(ctx, mesh.Request{Kind: "write", RequestID: fmt.Sprintf("%s-%d", r.ID, rng.Int63()), Record: &r})
			if err == nil {
				break
			}
			if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Millisecond)
		}
		if r.Deleted {
			delete(live, r.ID)
		} else {
			live[r.ID] = r
		}
	}
	for i := 0; i < 600; i++ {
		depth := 1 + rng.Intn(4)
		loc := make([]byte, depth)
		for k := range loc {
			loc[k] = alphabet[rng.Intn(8)] // few characters: deep shared prefixes
		}
		write(storage.Record{Collection: name, Location: string(loc), ID: fmt.Sprintf("r%03d", i),
			Metrics: []float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64(), float64(i), float64(i), 0.9 + 0.1*rng.Float64(), rng.NormFloat64()}})
	}
	// Remove the records that hold the maximum and the minimum, and move one.
	var hi, lo string
	for id, r := range live {
		if hi == "" || r.Metrics[2] > live[hi].Metrics[2] {
			hi = id
		}
		if lo == "" || r.Metrics[1] < live[lo].Metrics[1] {
			lo = id
		}
	}
	for _, id := range []string{hi, lo} {
		r := live[id]
		write(storage.Record{Collection: name, Location: r.Location, ID: id, Deleted: true})
	}
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "bad-width", Record: &storage.Record{Collection: name, Location: "0", ID: "bad", Metrics: []float64{1}}}); !errors.Is(err, storage.ErrDescription) && (err == nil || !InvalidDescription(err)) {
		t.Fatalf("a record of the wrong width was accepted: %v", err)
	}
	want := expectedRoot(d, name, live)

	deadline := time.Now().Add(15 * time.Second)
	for {
		var problem error
		for i, node := range f.nodes {
			if err := node.Update(); err != nil {
				t.Fatal(err)
			}
			for _, refresh := range []bool{false, true} {
				_, set, err := node.Get(name, "@", true, nil, refresh)
				if err != nil || set == nil {
					problem = fmt.Errorf("node %d refresh=%v: %v", i, refresh, err)
					continue
				}
				if err := sameTyped(set.Abelian(), want); err != nil {
					problem = fmt.Errorf("node %d refresh=%v: %w", i, refresh, err)
				}
			}
		}
		if problem == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(problem)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Every child key of the root folds its own records in cell order.
	_, set, err := f.nodes[1].Get(name, "@", true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range set.List() {
		sub := map[string]storage.Record{}
		for id, r := range live {
			if len(r.Location) >= len(key) && r.Location[:len(key)] == key {
				sub[id] = r
			}
		}
		if err := sameTyped(value, expectedRoot(d, name, sub)); err != nil {
			t.Fatalf("key %s: %v", key, err)
		}
	}
	t.Logf("%d live records, root %v", len(live), want)
}
