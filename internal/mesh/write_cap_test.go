package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// CAP-2: a capped vnode applies at most its rate per second and refuses the
// rest as overload; a write it only relays costs it nothing.
func TestCAP2_WriteRateCapsAppliedWrites(t *testing.T) {
	// Not parallel: it measures a rate against the wall clock.
	_, source, target := streamFixture(t, 64)
	ctx := context.Background()
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	write := func(node *Service, i int) (Response, error) {
		record := storage.Record{Collection: fmt.Sprintf("collection-%05d", i%64), Location: "aa", ID: fmt.Sprintf("capped-%d", i)}
		return node.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("capped-%s-%d", node.VNode.Name, i), Record: &record})
	}
	// The source holds nothing after the join: everything it gets is relayed.
	source.WriteRate = 10
	for i := range 64 {
		if out, err := write(source, i); err != nil || out.AppliedBy == source.Self {
			t.Fatalf("relayed write %d: applied by %v, %v", i, out.AppliedBy, err)
		}
	}
	source.WriteRate = 0
	target.WriteRate = 10
	applied, refused := 0, 0
	for i := range 64 {
		_, err := write(target, 100+i)
		switch {
		case errors.Is(err, storage.ErrWriteOverload):
			refused++
		case err != nil:
			t.Fatal(err)
		default:
			applied++
		}
	}
	if applied < 10 || applied > 11 || refused == 0 {
		t.Fatalf("applied=%d refused=%d, want about 10 within the burst", applied, refused)
	}
	if !target.writeCap.take(10, time.Now().Add(200*time.Millisecond)) {
		t.Fatal("the cap does not refill")
	}
}
