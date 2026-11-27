package node

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func fetchSets(t *testing.T, n *runningDataNode, query string) ([]byte, string) {
	t.Helper()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/sets?collection=Demo000000000000&%s", n.port, query))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v %s", response.StatusCode, err, body)
	}
	return body, response.Header.Get("Content-Type")
}

// A zone read through a node that does not hold it must be the zone its
// owner serves: same count, every metric, no quantisation. The entry node
// reads the holder's records over the mesh (QueryPrefixMetrics) and projects
// them itself; refresh makes the owner answer from the same scan instead of
// its summary. All three carry the keys of a zone (docs/protocol/query.md).
func TestRelayedBlockReadEqualsTheOwners(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	// Values that v1 cannot carry: metrics 0 and 1, fractions in metric 2,
	// more than six decimals in metrics 3 and 4, a sixth metric.
	for i, location := range []string{"aab", "aab", "aac", "aaZ9"} {
		item := &indexus.Item{
			Collection: "Demo000000000000", Location: location, Id: fmt.Sprintf("record-%d", i), RequestID: fmt.Sprintf("put-%d", i),
			Metrics: []float64{189056.25 + float64(i), 34.5, 5560.470588235294, 135.74761759038387, 183.12291497781908, 1e-9},
		}
		if err := source.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	var owner, relay *runningDataNode
	for _, n := range []*runningDataNode{source, target} {
		_, set, err := n.node.Get("Demo000000000000", "aa", false, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if set != nil {
			owner = n
		} else {
			relay = n
		}
	}
	if owner == nil || relay == nil {
		t.Fatalf("expected one holder of the zone: owner=%v relay=%v", owner != nil, relay != nil)
	}

	query := "location=aa&deep=true&refresh=true&metrics=f64"
	direct, _ := fetchSets(t, owner, query)
	relayed, contentType := fetchSets(t, relay, query)
	if contentType != wire.SetsBlockContentType {
		t.Fatalf("content type %q", contentType)
	}
	if !bytes.Equal(relayed, direct) {
		t.Fatalf("relayed block differs from the owner's:\n got %x\nwant %x", relayed, direct)
	}
	block, err := wire.DecodeSetsBlock(relayed)
	if err != nil {
		t.Fatal(err)
	}
	entries := block.Entries()
	if len(block.Redirects) != 0 || len(entries) != 3 || entries["aaZ9"] == nil {
		t.Fatalf("redirects %v, %d keys", block.Redirects, len(entries))
	}
	pair := entries["aab"]
	if pair == nil || pair.Count() != 2 || pair.Metric(0) != 189056.25+189057.25 || pair.Metric(1) != 69 ||
		pair.Metric(2) != 5560.470588235294+5560.470588235294 || pair.Metric(5) != 2e-9 {
		t.Fatalf("aab = %v", pair)
	}

	// Without refresh the relayed answer has the same shape and values.
	plain, _ := fetchSets(t, relay, "location=aa&deep=true&metrics=f64")
	if !bytes.Equal(plain, relayed) {
		t.Fatal("relayed read differs with and without refresh")
	}

	// The f32 block of the same relayed read is a projection of the same
	// values, so it too equals the owner's.
	narrow, _ := fetchSets(t, relay, "location=aa&deep=true&refresh=true&format=block")
	narrowDirect, _ := fetchSets(t, owner, "location=aa&deep=true&refresh=true&format=block")
	if !bytes.Equal(narrow, narrowDirect) {
		t.Fatal("relayed f32 block differs from the owner's")
	}
}
