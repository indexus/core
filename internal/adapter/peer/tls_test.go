package peer

import (
	"encoding/json"
	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTLSContactKeepsSchemeAcrossProbeDiscoveryAndRedirect(t *testing.T) {
	const name = "AAAAAAAAAAAAAAAB"
	var port int
	requests := map[string]int{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("peer request lost TLS")
		}
		requests[r.URL.Path]++
		contact := map[string]any{"name": name, "ips": map[string]any{"127.0.0.1": nil}, "port": port, "scheme": "https"}
		switch r.URL.Path {
		case "/ping":
			json.NewEncoder(w).Encode(map[string]any{"contact": contact})
		case "/neighbors":
			json.NewEncoder(w).Encode(map[string]any{"neighbors": []any{contact}})
		case "/random":
			json.NewEncoder(w).Encode(map[string]any{"random": contact})
		case "/sets":
			data, err := wire.EncodeSetsBlock(relayedSet(), []string{"aa"}, wire.SetsPrecision, []wire.SetsRedirect{{Location: "aa", Name: name, IP: "127.0.0.1", Port: port}}, wire.SetsBlockF64)
			if err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", wire.SetsBlockContentType)
			w.Write(data)
		case "/item", "/item/delete":
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_, port = splitHostPort(t, srv.Listener.Addr().String())
	previous := HttpClient
	HttpClient = srv.Client()
	defer func() { HttpClient = previous }()
	c := NewTLSContact(name, map[string]any{"127.0.0.1": nil}, port).(*Contact)
	origin := &fakeOrigin{name: name, ips: map[string]any{"127.0.0.1": nil}, port: 1}
	got, err := c.Ping(origin)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var restored Contact
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Endpoint() != srv.URL {
		t.Fatalf("roundtrip endpoint %s want %s", restored.Endpoint(), srv.URL)
	}
	peers, err := restored.Neighbors(origin)
	if err != nil || len(peers) != 1 {
		t.Fatalf("neighbors %v %v", peers, err)
	}
	if _, err = peers[0].Ping(origin); err != nil {
		t.Fatal("discovered peer lost scheme", err)
	}
	random, err := restored.Random(origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = random.Ping(origin); err != nil {
		t.Fatal("random peer lost scheme", err)
	}
	redirect, set, err := restored.Get("Demo000000000000", "aa", false, nil, false)
	if err != nil || set == nil || redirect == nil {
		t.Fatalf("TLS read: %v %v %v", redirect, set, err)
	}
	if _, err = redirect.Ping(origin); err != nil {
		t.Fatal("redirect lost TLS", err)
	}
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "row", RequestID: "request"}
	if err = restored.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	if err = restored.Delete(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	if requests["/ping"] != 4 || requests["/neighbors"] != 1 || requests["/random"] != 1 {
		t.Fatalf("requests %v", requests)
	}
}
