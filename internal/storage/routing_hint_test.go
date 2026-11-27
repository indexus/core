package storage

import (
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

func TestWriteHintExcludesPendingOutgoingRegion(t *testing.T) {
	source, target, g := ticketStores(t)
	region := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	op := ticketAdmission(t, source, target, g.Right().ID, region)
	_, err := source.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	outside := tree.ID{1}
	hint := source.WriteRegionHint(outside)
	if !hint.Contains(outside) {
		t.Fatal("hint lost ACKed point")
	}
	if _, overlap := hint.Intersect(region); overlap {
		t.Fatal("source advertised space it is handing out")
	}
	inside := tree.ID{200}
	if hint := source.WriteRegionHint(inside); hint != tree.Point(inside) {
		t.Fatal("partial source advertised neighboring points", hint)
	}
	ticketFinish(t, source, target, op.Cession.ID)
	_, err = target.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if got := target.WriteRegionHint(inside); got != region {
		t.Fatal("receiver did not advertise installed native grant", got)
	}
}
