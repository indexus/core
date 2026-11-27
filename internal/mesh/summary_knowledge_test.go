package mesh

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"testing"
	"time"
)

func TestSummaryRPCDoesNotBuildGlobalKnowledgeWithMatchingRouting(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	sender, receiver := nodes[1], nodes[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	routing, err := sender.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindSummaryGet, KindSummaryContribute} {
		out, err := sender.call(ctx, receiver.Self.Host, Request{Kind: kind, RoutingDigest: routing.RoutingDigest()})
		if err != nil || out.Membership != nil || out.RoutingDigest == (causal.Digest{}) {
			t.Fatalf("summary carried membership: %+v %v", out, err)
		}
	}
}
func TestSummaryRoutingMismatchRefusesUntilAsynchronousRepair(t *testing.T) {
	_, nodes := meshFixture(t, 3)
	sender, receiver := nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sender.Join(ctx); err != nil {
		t.Fatal(err)
	}
	routing, err := sender.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sender.call(ctx, receiver.Self.Host, Request{Kind: KindSummaryContribute, RoutingDigest: routing.RoutingDigest()})
	if !errors.Is(err, causal.ErrDependency) || out.Membership != nil {
		t.Fatalf("mismatch accepted or carried membership: %+v %v", out, err)
	}
	if err = sender.Sync(ctx, []string{receiver.Self.Host}); err != nil {
		t.Fatal(err)
	}
	routing, err = sender.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if out, err = sender.call(ctx, receiver.Self.Host, Request{Kind: KindSummaryContribute, RoutingDigest: routing.RoutingDigest()}); err != nil || out.Membership != nil {
		t.Fatalf("repaired summary: %+v %v", out, err)
	}
}
