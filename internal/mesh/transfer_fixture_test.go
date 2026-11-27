package mesh

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

// confirmedTransfer prepares real durable participants. Tests may then stop at
// a precise fence/stage/install boundary without inventing an admission proof.
func confirmedTransfer(t testing.TB, source, target *Service, input causal.Digest, region tree.Region) storage.CausalTransfer {
	t.Helper()
	offer, err := source.Store.BeginCausal(context.Background(), input, target.Self, region)
	if err != nil {
		t.Fatal(err)
	}
	c, err := target.Store.ReserveCausal(offer.Cession)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Store.BindCausal(c.ID, c.Ticket); err != nil {
		t.Fatal(err)
	}
	offer.Cession = c
	if _, err = target.Store.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	if err = source.Store.ConfirmCausal(c.ID); err != nil {
		t.Fatal(err)
	}
	offer.Phase = causal.TransferReady
	return offer
}

type transportFunc func(context.Context, string, Request) (Response, error)

func (f transportFunc) Exchange(ctx context.Context, host string, r Request) (Response, error) {
	return f(ctx, host, r)
}
func inventoryRPC(kind string) bool { return kind == "inventory" || kind == KindInventoryPrefix }
