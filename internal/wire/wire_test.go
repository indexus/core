package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestPeerWireContract(t *testing.T) {
	var fixture struct {
		Peers  []struct{ Name, IDHex, RoutingKey string }
		Limits struct{ ReadLocations, ContentRecords, ContentParts, ContentBytes, ContentLocationLength int }
		Sets   struct{ Precision, PropertyCount, MetricScale int }
	}
	raw, err := os.ReadFile("testdata/peer-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	limits := fixture.Limits
	projection := fixture.Sets
	if limits.ReadLocations != MaxReadLocations || limits.ContentRecords != MaxContentRecords || limits.ContentParts != MaxContentParts || limits.ContentBytes != MaxContentBytes || limits.ContentLocationLength != ContentLocationLength || projection.Precision != SetsPrecision || projection.PropertyCount != SetsPropertyCount || projection.MetricScale != SetsMetricScale {
		t.Fatal("Go peer contract differs from shared SDK fixture")
	}
	for _, peer := range fixture.Peers {
		expected, _ := hex.DecodeString(peer.IDHex)
		actual, err := tree.BASE64.Decode(peer.Name)
		if err != nil || !bytes.Equal(actual, expected) || tree.BASE64.Encode(expected) != peer.Name {
			t.Fatalf("peer name encoding differs: %+v", peer)
		}
		header, err := base64.RawURLEncoding.DecodeString(peer.RoutingKey)
		if err != nil || !bytes.Equal(header, expected) {
			t.Fatalf("routing header differs: %+v", peer)
		}
	}
}
