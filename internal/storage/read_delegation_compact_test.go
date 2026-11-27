package storage

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestDirectoryCompactionPreservesPendingAdmissionThenForgetsVersions(t *testing.T) {
	a, b, genesis := ticketStores(t)
	left := tree.Region{Mask: tree.ID{128}}
	right := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	first := ticketAdmission(t, a, b, genesis.Right().ID, left)
	ticketFinish(t, a, b, first.Cession.ID)
	leftRights, err := b.Rights(left)
	if err != nil || len(leftRights) != 1 {
		t.Fatal("missing first installed piece", err)
	}
	ep, err := a.Endpoint(a.node)
	if err != nil {
		t.Fatal(err)
	}
	back, err := b.BeginCausal(context.Background(), leftRights[0].ID, ep, left)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := a.ReserveCausal(back.Cession)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.BindCausal(reserved.ID, reserved.Ticket); err != nil {
		t.Fatal(err)
	}
	back.Cession = reserved
	rights, err := a.Rights(right)
	if err != nil || len(rights) != 1 {
		t.Fatal("missing source complement", err)
	}
	second := ticketAdmission(t, a, b, rights[0].ID, right)
	ticketFinish(t, a, b, second.Cession.ID)
	if err := a.CompactReadDirectory(); err != nil {
		t.Fatal(err)
	}
	var directory []readDelegation
	load := func() {
		t.Helper()
		if err := a.view(func(tx *Tx) error {
			var err error
			directory, err = readDelegations(tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	load()
	if len(directory) != 2 || directory[0].Stamp != 0 || directory[1].Stamp != reserved.Ticket {
		t.Fatalf("forgot the distinction needed by a delayed first preload: %+v", directory)
	}
	if _, err := a.PreloadCausal(back); err != nil {
		t.Fatal("delayed legitimate preload was suppressed", err)
	}
	if err := b.ConfirmCausal(reserved.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.AbortCausal(reserved.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.CancelCausal(reserved); err != nil {
		t.Fatal(err)
	}
	if err := b.FinishAbortCausal(reserved.ID); err != nil {
		t.Fatal(err)
	}
	before, err := a.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CompactReadDirectory(); err != nil {
		t.Fatal(err)
	}
	load()
	targetEndpoint, err := b.Endpoint(b.node)
	if err != nil {
		t.Fatal(err)
	}
	if len(directory) != 1 || directory[0].Region != (tree.Region{}) || directory[0].Stamp != 0 || directory[0].Holder.Owner != targetEndpoint.Owner {
		t.Fatalf("completed transfers left historical versions in the frontier: %+v", directory)
	}
	var raw []byte
	_ = a.view(func(tx *Tx) error { raw = bytes.Clone(tx.value(meta(readDelegationsKey))); return nil })
	if _, err := a.EmitCausal(first.Cession.ID); err != nil {
		t.Fatal(err)
	}
	_ = a.view(func(tx *Tx) error {
		if !bytes.Equal(raw, tx.value(meta(readDelegationsKey))) {
			t.Error("replayed issued operation rewrote the current directory")
		}
		return nil
	})
	after, err := a.CausalState()
	if err != nil || after.Permissions.Sequence != before.Permissions.Sequence || len(after.Permissions.Native) != 0 {
		t.Fatal("routing compaction changed installed permissions", err)
	}
	if _, err := a.Write("not-authorized", Record{Collection: "Demo000000000000", Location: "aa", ID: "no"}); !errors.Is(err, ErrNotOwner) {
		t.Fatal("a compact routing pointer granted a write", err)
	}
}

func FuzzDirectoryVersionRankPreservesPossibleUpdates(f *testing.F) {
	f.Add(uint64(6), uint64(4), uint64(8), uint64(12))
	f.Add(uint64(0), uint64(0), uint64(0), uint64(0))
	f.Fuzz(func(t *testing.T, stamp, a, b, c uint64) {
		possible := []uint64{a, b, c}
		slices.Sort(possible)
		possible = slices.Compact(possible)
		rank := directoryVersionRank(stamp, possible)
		for _, update := range possible {
			if (update > stamp) != (update > rank) {
				t.Fatal("compaction changed whether a pending update supersedes a pointer")
			}
		}
		upper := max(stamp, a, b, c)
		if upper != math.MaxUint64 && upper+1 <= rank {
			t.Fatal("compaction suppressed a future fresh operation")
		}
	})
}
