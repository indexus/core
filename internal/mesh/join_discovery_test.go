package mesh

import (
	"context"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync/atomic"
	"testing"
	"time"
)

func TestJoinInstallsFirstPieceBeforeProbingUnrelatedMembers(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hosts []string
	for _, node := range nodes[:3] {
		hosts = append(hosts, node.Self.Host)
	}
	for _, node := range nodes[1:3] {
		if err := node.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range nodes[:3] {
		if err := node.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
		if err := node.Rebalance(ctx); err != nil {
			t.Fatal(err)
		}
	}
	joiner := nodes[3]
	if err := joiner.Sync(ctx, []string{nodes[0].Self.Host}); err != nil {
		t.Fatal(err)
	}
	var probes, beforeInstall atomic.Int64
	var installed atomic.Bool
	net.setBefore(func(r Request) error {
		if r.Kind == KindRights {
			probes.Add(1)
		}
		if (r.Kind == "install" || r.Kind == "install-zone") && installed.CompareAndSwap(false, true) {
			beforeInstall.Store(probes.Load())
		}
		return nil
	})
	if err := joiner.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !installed.Load() || beforeInstall.Load() != 1 {
		t.Fatalf("first usable piece needed %d rights probes", beforeInstall.Load())
	}
}

func TestSpawnedJoinKeepsInstalledResponsibilityWhenDemandFalls(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	target.VNode.ID = tree.ID{128, 1}
	view := tree.NewView(0)
	_ = view.Add(source.VNode.Name, source.VNode.ID)
	_ = view.Add(target.VNode.Name, target.VNode.ID)
	desired, err := tree.Cell(view, target.VNode.Name)
	if err != nil {
		t.Fatal(err)
	}
	// Spread the points through the desired cell: this requires multiple
	// populated native grains, rather than scores of empty prefix complements.
	for candidate := range 600 {
		digest := causal.Hash(fmt.Sprint("wanted-zone-", candidate))
		address := tree.FromSlice(digest[:])
		for i := range address {
			address[i] = (address[i] &^ desired.Mask[i]) | desired.Value[i]
		}
		row := storage.Record{Collection: "wanted-000000000", Location: tree.BASE64.Encode(address[:]), ID: "seed"}
		if _, e := source.Write(context.Background(), fmt.Sprint(candidate), row); e != nil {
			t.Fatal(e)
		}
	}
	// The deadline only bounds a hung join; under -race, 600 zones in lots
	// sized for a 40 ms hold (FLIP-HOLD-1) take more than 10 s.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wanted atomic.Bool
	wanted.Store(true)
	source.Wanted = wanted.Load
	target.Spawned = true
	net.setAfter(func(r Request) error {
		if r.Kind == "install-zone" {
			wanted.Store(false)
		}
		return nil
	})
	if err := target.Join(ctx); err != nil {
		t.Fatalf("spawn abandoned an installed writer after demand fell: %v", err)
	}
	if wanted.Load() {
		t.Fatal("did not exercise falling demand")
	}
	m, err := target.Membership()
	if err != nil {
		t.Fatal(err)
	}
	cell, err := tree.Cell(m.Current(), target.VNode.Name)
	if err != nil {
		t.Fatal(err)
	}
	rights, err := target.Store.Rights(cell)
	if err != nil {
		t.Fatal(err)
	}
	var coverage []tree.Region
	for _, right := range rights {
		coverage = append(coverage, right.Region)
	}
	if !tree.CoversRegions(cell, coverage) {
		t.Fatal("spawn stopped before acquiring its remaining desired address space")
	}
}
