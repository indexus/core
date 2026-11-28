package node

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestCachedSetEndpointDoesNotWarmColdZone(t *testing.T) {
	n := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	const collection = "Demo000000000000"
	row := &indexus.Item{Collection: collection, Location: "aa", Id: "existing", Metrics: []float64{7}}
	if err := n.node.New(row, "@", nil); err != nil {
		t.Fatal(err)
	}
	fetch := func(path string, status int) []byte {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", n.port, path))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("%s: %d %q %v", path, resp.StatusCode, data, err)
		}
		return data
	}
	const query = "?collection=" + collection + "&location=aa"
	const lookup = "/set/cached" + query + "&max_age=5m"
	fetch("/set/cached"+query, 400)
	fetch(lookup, 204)
	fetch(lookup, 204)
	fetch("/set"+query+"&deep=true&refresh=true", 200)
	raw := fetch(lookup, 200)
	var body struct {
		Source     string
		ObservedAt time.Time `json:"observed_at"`
		Set        map[string]json.RawMessage
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Source != "cache" || body.ObservedAt.IsZero() || len(body.Set) == 0 {
		t.Fatalf("retained result: %s (%v)", raw, err)
	}
}
