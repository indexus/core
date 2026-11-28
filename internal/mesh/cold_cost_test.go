package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestColdCostOrientation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		a, b               ColdUsage
		aDonates, bDonates bool
	}{
		{"both fit", ColdUsage{10, 100}, ColdUsage{30, 100}, true, false},
		{"only smaller disk's owner can survive", ColdUsage{10, 100}, ColdUsage{30, 60}, false, true},
		{"neither fits", ColdUsage{10, 60}, ColdUsage{30, 60}, false, false},
		{"id tie", ColdUsage{20, 100}, ColdUsage{20, 100}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.a.shouldDonate(tc.b, .45, false) != tc.aDonates || tc.b.shouldDonate(tc.a, .45, true) != tc.bDonates {
				t.Fatal("unexpected pair orientation")
			}
		})
	}
}

func coldNonRootPair(t *testing.T, ctx context.Context) (*network, *Service, *Service) {
	t.Helper()
	net, nodes := meshFixture(t, 4)
	for _, n := range nodes[1:] {
		if err := n.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes {
		for _, p := range nodes {
			if n != p {
				if err := n.Sync(ctx, []string{p.Self.Host}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return net, nodes[2], nodes[3]
}

func TestAutomaticColdMovesSmallerSiblingAndResumesWithoutProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	net, light, heavy := coldNonRootPair(t, ctx)
	r := storage.Record{Collection: "0000000000000001", Location: tree.EncodeName(heavy.VNode.ID), ID: "heavy-row"}
	if _, _, err := heavy.Store.Apply("ACK", r); err != nil {
		t.Fatal(err)
	}
	config := ColdConfig{Automatic: true}
	if err := heavy.Compact(ctx, config); !errors.Is(err, protocol.ErrNoBetterConfiguration) {
		t.Fatalf("heavy sibling attempted to donate: %v", err)
	}
	var drop string
	net.setBefore(func(r Request) error {
		if r.Kind == drop {
			return storage.ErrClosed
		}
		return nil
	})
	drop = KindColdUsage
	if err := light.Compact(ctx, config); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("probe: %v", err)
	}
	status, err := light.Store.ColdStatus()
	if err != nil || status.Drain != nil {
		t.Fatalf("failed probe left durable work: %+v %v", status, err)
	}
	drop = KindColdPin
	if err := light.Compact(ctx, config); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("pin: %v", err)
	}
	drop = KindColdUsage
	if err := light.ResumeCold(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = light.Store.ColdStatus()
	if err != nil || status.Drain == nil || !status.Drain.Witnessed || status.Drain.Target != heavy.Self {
		t.Fatalf("incomplete recovery: %+v %v", status, err)
	}
	if _, applied, err := heavy.Store.Apply("ACK", r); err != nil || applied {
		t.Fatalf("repeat write: applied=%v error=%v", applied, err)
	}
}

func TestColdCrossedProposalsReleaseAndRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, a, b := coldNonRootPair(t, ctx)
	for _, pair := range [][2]*Service{{a, b}, {b, a}} {
		if _, err := pair[0].Store.BeginColdDrain(pair[0].VNode.Name, pair[1].Self, storage.ColdLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ResumeCold(ctx); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("crossed pin: %v", err)
	}
	state, err := a.Store.ColdStatus()
	if err != nil || state.Pin != nil || state.Drain != nil {
		t.Fatalf("rejected source: %+v %v", state, err)
	}
	if err := b.ResumeCold(ctx); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*Service{a, b} {
		state, err := node.Store.ColdStatus()
		if err != nil || state.Pin != nil || (node == b && (state.Drain == nil || !state.Drain.Witnessed)) {
			t.Fatalf("incomplete retry: %+v %v", state, err)
		}
	}
}

func TestPinnedSurvivorSkipsAutomaticDonorProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	net, receiver, source := coldNonRootPair(t, ctx)
	if _, err := receiver.Store.PinColdSurvivor("incoming", source.Self, storage.ColdOffer{}); err != nil {
		t.Fatal(err)
	}
	probes := 0
	net.setBefore(func(r Request) error {
		if r.Kind == KindColdUsage {
			probes++
		}
		return nil
	})
	if err := receiver.Compact(ctx, ColdConfig{Automatic: true}); !errors.Is(err, ErrColdDeferred) {
		t.Fatalf("wanted idle tick while receiving: %v", err)
	}
	if probes != 0 {
		t.Fatalf("pinned receiver issued %d donor probes", probes)
	}
	if err := receiver.Store.ReleaseColdPin("incoming", source.Self); err != nil {
		t.Fatal(err)
	}
	err := receiver.Compact(ctx, ColdConfig{Automatic: true})
	if probes != 1 || (err != nil && !errors.Is(err, protocol.ErrNoBetterConfiguration)) {
		t.Fatalf("after release: probes=%d, err=%v", probes, err)
	}
}
