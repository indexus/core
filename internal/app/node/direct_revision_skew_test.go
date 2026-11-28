package node

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

// A client can complete an exact read at each owner and still combine a pair
// of document versions that never coexisted. Each write here updates its
// metric and its JSON revision together, and always updates aa before zz.
func TestDirectPayloadReadsCanCrossRevisions(t *testing.T) {
	const collection = "Demo000000000000"
	owner := func(loc string, bootstrap *runningDataNode) *runningDataNode {
		t.Helper()
		key, err := indexus.ZoneID(collection, loc)
		if err != nil {
			t.Fatal(err)
		}
		return startDataNode(t, indexus.BASE64.Encode(key.Bytes()), bootstrap)
	}
	first := owner("aa", nil)
	second := owner("zz", first)
	putPair := func(rev int) error {
		for _, loc := range []string{"aa", "zz"} {
			item := &indexus.Item{Collection: collection, Location: loc, Id: "row",
				RequestID: fmt.Sprintf("rev-%s-%d", loc, rev), Metrics: []float64{float64(rev)},
				Payload: json.RawMessage(fmt.Sprintf(`{"revision":%d}`, rev))}
			if err := first.node.New(item, "@", nil); err != nil {
				return err
			}
		}
		return nil
	}
	read := func(peer *runningDataNode, loc string) int {
		t.Helper()
		url := fmt.Sprintf("http://127.0.0.1:%d/records/direct?collection=%s&location=%s&limit=1&payloads=true", peer.port, collection, loc)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply struct {
			Payloads bool
			Records  []indexus.Item
		}
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !reply.Payloads || len(reply.Records) != 1 {
			t.Fatalf("status %d reply %+v", resp.StatusCode, reply)
		}
		row := reply.Records[0]
		var body struct{ Revision *int }
		if err := json.Unmarshal(row.Payload, &body); err != nil {
			t.Fatal(err)
		}
		if row.Id != "row" || row.Location != loc || len(row.Metrics) != 1 || body.Revision == nil || row.Metrics[0] != float64(*body.Revision) {
			t.Fatalf("incoherent direct row %+v", row)
		}
		return *body.Revision
	}
	if err := putPair(0); err != nil {
		t.Fatal(err)
	}
	early := read(first, "aa")
	const last = 8
	writer := make(chan error, 1)
	go func() {
		for revision := 1; revision <= last; revision++ {
			if err := putPair(revision); err != nil {
				writer <- err
				return
			}
		}
		writer <- nil
	}()
	// Both readers can run while the two owners advance independently.
	for range last {
		read(first, "aa")
		read(second, "zz")
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	late := read(second, "zz")
	if early != 0 || late != last || read(first, "aa") != last {
		t.Fatalf("expected skew across exact reads, got aa=%d zz=%d", early, late)
	}
}
