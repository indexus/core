package storage

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

func TestGranularPointCoverageIsCanonicalAndIndependentForExactFullQuery(t *testing.T) {
	source, target, g := ticketStores(t)
	parent, _ := tree.NewZoneKey("0000000000000001", "0")
	point, _ := tree.NewZoneKey(parent.Collection, tree.BASE64.Encode(parent.ID[:]))
	if parent.ID != point.ID {
		t.Fatal("fixture needs canonical parent/point ID alias")
	}
	for _, z := range []tree.ZoneKey{parent, point} {
		if _, e := source.Write(z.Location, Record{Collection: z.Collection, Location: z.Location, ID: z.Location}); e != nil {
			t.Fatal(e)
		}
	}
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	snaps, e := source.FreezeZones(op.Cession.ID, []tree.ZoneKey{point})
	if e != nil {
		t.Fatal(e)
	}
	if e = target.StageZones(op.Cession.ID, source.node, snaps); e != nil {
		t.Fatal(e)
	}
	proofs, e := source.EmitZoneTransfers(op.Cession.ID, []tree.ZoneKey{point})
	if e != nil {
		t.Fatal(e)
	}
	if e = target.InstallZones(op.Cession.ID, proofs); e != nil {
		t.Fatal(e)
	}
	if _, e = target.Write("point-new", Record{Collection: point.Collection, Location: point.Location, ID: "point-new"}); e != nil {
		t.Fatal(e)
	}
	if _, e = source.Write("parent-new", Record{Collection: parent.Collection, Location: parent.Location, ID: "parent-new"}); e != nil {
		t.Fatal("child point froze its canonical parent", e)
	}
	rows, page, e := target.CausalInventoryPage(context.Background(), &point, InventoryPageRequest{})
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Referrals) != 0 || !tree.CoversRegions(tree.PrefixRegion(point), page.Coverage) || len(rows) != 1 || len(rows[0].Records) != 2 {
		t.Fatal("exact installed point still depends on source", page, len(rows))
	}
	_, page, e = target.CausalInventoryPage(context.Background(), &parent, InventoryPageRequest{})
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Referrals) == 0 || tree.CoversRegions(tree.PrefixRegion(parent), page.Coverage) {
		t.Fatal("child point falsely covered uninstalled canonical parent", page)
	}
}
