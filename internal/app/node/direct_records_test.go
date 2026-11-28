package node

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestDirectRecordsColdHTTP(t *testing.T) {
	n := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	const col = "Demo000000000000"
	for _, item := range []indexus.Item{
		{Collection: col, Location: "@", Id: "root"},
		{Collection: col, Location: "a0", Id: "child"},
		{Collection: col, Location: "a", Id: "deleted"},
		{Collection: col, Location: "a", Id: "alpha"},
	} {
		if err := n.node.New(&item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Seed the weighted pre-aggregate through the durable write interface.
	if _, err := n.boot.store.Write("weighted", storage.Record{Collection: col, Location: "a", ID: "z", Weight: 3, Metrics: []float64{42}, Payload: json.RawMessage(`{"private":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := n.node.Delete(&indexus.Item{Collection: col, Location: "a", Id: "deleted"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	get := func(location string, limit int, status int) []indexus.Item {
		t.Helper()
		url := fmt.Sprintf("http://127.0.0.1:%d/records/direct?collection=%s&location=%s&limit=%d", n.port, col, location, limit)
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var reply struct {
			Records []indexus.Item
			Error   string
		}
		if err := json.NewDecoder(res.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != status || (status == 200) != (reply.Records != nil) {
			t.Fatalf("%s: status %d, reply %+v", url, res.StatusCode, reply)
		}
		return reply.Records
	}
	rows := get("a", 2, 200)
	if len(rows) != 2 || rows[0].Id != "alpha" || rows[0].Metrics == nil || rows[1].Id != "z" || rows[1].Weight != 3 || len(rows[1].Metrics) != 1 || rows[1].Metrics[0] != 42 || rows[1].Payload != nil {
		t.Fatal(rows)
	}
	root := get("@", 1, 200)
	if len(root) != 1 || root[0].Id != "root" {
		t.Fatal(root)
	}
	if rows := get("b", 1, 200); len(rows) != 0 {
		t.Fatal(rows)
	}
	get("a", 1, 413)
	get("a", 0, 400)
	get("!", 1, 400)
}
