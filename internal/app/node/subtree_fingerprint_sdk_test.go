package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

// Keep one JS client alive across acknowledged writes to the two real peers.
func TestSDKSelectiveSubtreeRefresh(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	write := func(owner *runningDataNode, location, id, body string, remove bool) error {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		item := &indexus.Item{Collection: directSDKCollection, Location: location, Id: id,
			Payload: payload, Metrics: []float64{2}, RequestID: location + "-" + body}
		if remove {
			return owner.node.Delete(item, "@", nil)
		}
		return owner.node.New(item, "@", nil)
	}
	for _, entry := range []struct {
		owner        *runningDataNode
		location, id string
	}{
		{first, "aa0", "first"}, {first, "aa1", "neighbor"}, {second, "zz0", "remote"},
	} {
		if err := write(entry.owner, entry.location, entry.id, "original", false); err != nil {
			t.Fatal(err)
		}
	}
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var err error
		switch r.URL.Path {
		case "/transient":
			err = write(first, "aa0", "transient", "brief", false)
		case "/undo":
			err = write(first, "aa0", "transient", "undo", true)
		case "/edit":
			err = write(first, "aa0", "first", "modified", false)
		case "/edit-neighbor":
			err = write(first, "aa1", "neighbor", "later", false)
		case "/delete":
			err = write(first, "aa0", "first", "deleted", true)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer control.Close()
	runDirectSDK(t, "subtree_fingerprint_sdk.mjs", sdk, first, second, control.URL)
}
