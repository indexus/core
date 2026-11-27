package mesh

import (
	"context"
	"testing"
)

// A join large enough for the donor's memtable to flush between a zone's
// freeze and the region's: every zone keeps one seal, so the region
// certificate is accepted (it was refused from about 3 000 zones on).
func TestJoinOfManyZonesKeepsOneSealPerZone(t *testing.T) {
	t.Parallel()
	_, _, target := streamFixture(t, 3000)
	if err := target.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
}
