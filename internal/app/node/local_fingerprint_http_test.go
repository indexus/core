package node

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestLocalFingerprintHTTPDistributedPrefix(t *testing.T) {
	const collection = "Demo000000000000"
	start := func(location string, bootstrap *runningDataNode) *runningDataNode {
		t.Helper()
		address, err := indexus.ZoneID(collection, location)
		if err != nil {
			t.Fatal(err)
		}
		return startDataNode(t, indexus.BASE64.Encode(address.Bytes()), bootstrap)
	}
	first := start("aa", nil)
	second := start("zz", first)
	probe := func(peer *runningDataNode, location string, wantCount string) string {
		t.Helper()
		url := fmt.Sprintf("http://127.0.0.1:%d/zones/fingerprint/local?collection=%s&location=%s", peer.port, collection, location)
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body struct {
			Collection, Location string
			Fingerprint, Guard   *string
			Complete             *bool
			Monoid               struct{ Algorithm, Sum, Count string }
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 || body.Complete == nil || *body.Complete || body.Collection != collection || body.Location != location ||
			body.Fingerprint == nil || *body.Fingerprint == "" || body.Guard == nil || *body.Guard == "" {
			t.Fatalf("%s: %d %+v", url, res.StatusCode, body)
		}
		if body.Monoid.Algorithm != "sha256-add256-v1" || body.Monoid.Count != wantCount || len(body.Monoid.Sum) != 64 {
			t.Fatalf("%s: invalid monoid %+v", url, body.Monoid)
		}
		return *body.Fingerprint
	}
	// Both disks serve an empty local observation at a distributed root. The
	// second disk may be probed at aa even though it cannot serve its rows.
	before := probe(first, "@", "0")
	probe(second, "@", "0")
	probe(second, "aa", "0")
	addr := fmt.Sprintf("http://127.0.0.1:%d/records/direct?collection=%s&location=aa&limit=1", second.port, collection)
	res, err := http.Get(addr)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected non-owner at aa, got %d", res.StatusCode)
	}
	if err := first.node.New(&indexus.Item{Collection: collection, Location: "aa", Id: "local", RequestID: "insert-fingerprint-test"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	if after := probe(first, "@", "1"); after == before {
		t.Fatal("descendant insertion retained the root fingerprint")
	}
	// A remote write does not appear on this disk, and a remote outage cannot prevent this local read.
	first.stop()
	probe(second, "@", "0")
}
