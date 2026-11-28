package mesh

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Measure the recovery worker through its first install packet. Reject that
// packet before the remote commit so every iteration reads the same durable lot.
func BenchmarkResumeFencedInstall(b *testing.B) {
	net, source, target := streamFixture(b, 16)
	state, err := source.Store.CausalState()
	if err != nil {
		b.Fatal(err)
	}
	keys := make([]tree.ZoneKey, 16)
	payload := append(append([]byte{'"'}, bytes.Repeat([]byte{'x'}, 32<<10)...), '"')
	for i := range keys {
		r := storage.Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: "bulk", Payload: payload}
		keys[i], err = tree.NewZoneKey(r.Collection, r.Location)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := source.Store.Write(fmt.Sprintf("req-%d", i), r); err != nil {
			b.Fatal(err)
		}
	}
	offer := confirmedTransfer(b, source, target, state.Genesis.Right().ID, tree.Region{})
	if _, _, _, err := source.Store.FenceZones(offer.Cession.ID, keys); err != nil {
		b.Fatal(err)
	}
	net.setBefore(func(r Request) error {
		if r.Kind != "install-zone" || len(r.Seals) != len(keys) || len(r.ZoneTransfers) != len(keys) {
			b.Fatalf("unexpected replay %s: %d seals, %d proofs", r.Kind, len(r.Seals), len(r.ZoneTransfers))
		}
		return storage.ErrClosed
	})
	b.ReportAllocs()
	b.SetBytes(int64(len(keys) * len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := source.resumeZoneInstalls(b.Context(), offer); !errors.Is(err, storage.ErrClosed) {
			b.Fatal(err)
		}
	}
}
