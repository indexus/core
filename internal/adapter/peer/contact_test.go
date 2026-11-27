package peer

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type fakeOrigin struct {
	name string
	ips  map[string]any
	port int
}

func (f *fakeOrigin) ID() []byte                                        { return nil }
func (f *fakeOrigin) Name() string                                      { return f.name }
func (f *fakeOrigin) IPs() map[string]any                               { return f.ips }
func (f *fakeOrigin) Port() int                                         { return f.port }
func (f *fakeOrigin) IP() string                                        { return "" }
func (f *fakeOrigin) Host() string                                      { return "" }
func (f *fakeOrigin) Ping(indexus.Contact) (indexus.Contact, error)     { return nil, nil }
func (f *fakeOrigin) Neighbors(indexus.Peer) ([]indexus.Contact, error) { return nil, nil }
func (f *fakeOrigin) Random(indexus.Peer) (indexus.Contact, error)      { return nil, nil }
func (f *fakeOrigin) Transfer(indexus.Peer, indexus.Key, []*indexus.Item) (string, error) {
	return f.name, nil
}
func (f *fakeOrigin) Copy(origin indexus.Peer, key indexus.Key, items []*indexus.Item) (string, error) {
	return f.Transfer(origin, key, items)
}
func (f *fakeOrigin) Get(string, string, bool, indexus.Visited, bool) (indexus.Contact, *indexus.Set, error) {
	return nil, nil, nil
}
func (f *fakeOrigin) New(*indexus.Item, string, indexus.Visited) error    { return nil }
func (f *fakeOrigin) Delete(*indexus.Item, string, indexus.Visited) error { return nil }

func TestNewContactSetsPrimaryIP(t *testing.T) {
	c := NewContact("PeerAAAAAAAAAAAA", map[string]any{"127.0.0.1": nil}, 21010).(*Contact)
	if c.IP() != "127.0.0.1" {
		t.Fatalf("IP=%q want 127.0.0.1", c.IP())
	}
	host := c.Host()
	if host != "PeerAAAAAAAAAAAA@127.0.0.1|21010" {
		t.Fatalf("Host=%q", host)
	}
}

func newServerContact(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Contact) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u := srv.Listener.Addr().(interface{ String() string }).String()
	host, port := splitHostPort(t, u)

	c := &Contact{
		name: "PeerAAAAAAAAAAAA",
		ips:  map[string]any{host: nil},
		ip:   host,
		port: port,
	}
	return srv, c
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := splitAddr(addr)
	if err != nil {
		t.Fatalf("split addr %q: %v", addr, err)
	}
	port := 0
	for _, r := range portStr {
		port = port*10 + int(r-'0')
	}
	return host, port
}

func splitAddr(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", nil
}

func TestPingPropagatesNon2xxAsError(t *testing.T) {
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	origin := &fakeOrigin{name: "self", ips: map[string]any{"127.0.0.1": nil}, port: 1}
	got, err := c.Ping(origin)
	if err == nil {
		t.Fatalf("Ping must return an error on non-2xx response")
	}
	if got != nil {
		t.Fatalf("Ping must not return a contact on non-2xx response, got %#v", got)
	}
}

func TestPingHappyPathReturnsContact(t *testing.T) {
	var serverPort int
	srv, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			Contact *Contact `json:"contact"`
		}{
			Contact: &Contact{name: "PeerAAAAAAAAAAAA", ips: map[string]any{}, port: serverPort},
		})
	})
	_ = srv
	serverPort = c.port

	origin := &fakeOrigin{name: "self", ips: map[string]any{"127.0.0.1": nil}, port: 1}
	got, err := c.Ping(origin)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got == nil || got.Name() != "PeerAAAAAAAAAAAA" {
		t.Fatalf("Ping returned unexpected contact: %#v", got)
	}
}

func TestNewMaps503ToErrPeerBusy(t *testing.T) {
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	item := &indexus.Item{Collection: "c", Location: "a", Id: "x", Metrics: []float64{1}}
	err := c.New(item, "@", nil)
	if err == nil {
		t.Fatal("New must surface 503 as an error")
	}
	if !errors.Is(err, indexus.ErrPeerBusy) {
		t.Fatalf("New: %v, want ErrPeerBusy", err)
	}
}

func TestRandomDecodesHandlerPayload(t *testing.T) {
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/random" {
			t.Fatalf("path=%s want /random", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"random": map[string]any{
				"name": "PeerBBBBBBBBBBBB",
				"ips":  map[string]any{"10.0.0.2": nil},
				"ip":   "10.0.0.2",
				"port": 21001,
			},
		})
	})

	got, err := c.Random(&fakeOrigin{name: "self", ips: map[string]any{"127.0.0.1": nil}, port: 1})
	if err != nil {
		t.Fatalf("Random: %v", err)
	}
	if got == nil || got.Name() != "PeerBBBBBBBBBBBB" {
		t.Fatalf("Random=%v want PeerBBBBBBBBBBBB", got)
	}
}

func TestRandomNullBodyIsNilInterface(t *testing.T) {
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"random":null}`))
	})
	got, err := c.Random(&fakeOrigin{name: "self", ips: map[string]any{"127.0.0.1": nil}, port: 1})
	if err != nil {
		t.Fatalf("Random: %v", err)
	}
	if got != nil {
		t.Fatalf("typed-nil Contact leaked as non-nil interface: %#v", got)
	}
}

func TestWriteEndpointsKeepDistinctWireContracts(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		for _, status := range []int{http.StatusCreated, http.StatusOK, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("delete=%t/status=%d", deletion, status), func(t *testing.T) {
				item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "row", RequestID: "stable"}
				_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
					path, header := "/item", "1"
					if deletion {
						path, header = "/item/delete", ""
					}
					if r.URL.Path != path || r.Header.Get(indexus.HandoffHeader) != header {
						t.Errorf("wire route/header changed: %s %q", r.URL.Path, r.Header.Get(indexus.HandoffHeader))
					}
					var body struct {
						Item *indexus.Item `json:"item"`
						Root string        `json:"root"`
						Via  string        `json:"via"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Item == nil || body.Item.RequestID != "stable" || body.Root != "@" || body.Via != "origin" {
						t.Errorf("write envelope changed: %+v", body)
					}
					w.WriteHeader(status)
				})
				write := c.New
				if deletion {
					write = c.Delete
				}
				err := write(item, "@", indexus.Visited{"origin"})
				switch {
				case status == http.StatusServiceUnavailable:
					if !errors.Is(err, indexus.ErrPeerBusy) {
						t.Fatalf("busy mapping: %v", err)
					}
				case status == http.StatusCreated || deletion:
					if err != nil {
						t.Fatal(err)
					}
				default:
					if err == nil {
						t.Fatal("insert accepted legacy delete-only status")
					}
				}
			})
		}
	}
}

func relayedSet() map[string]*indexus.Set {
	set := wire.NewSet()
	set.Put("aab", wire.NewAbelian(3, []float64{189056.25, 34.5, 5560.470588235294, 135.74761759038387, 183.12291497781908}))
	set.Put("aac:ref", wire.NewAbelian(1, []float64{0.1, 0.2}))
	return map[string]*indexus.Set{"aa": set}
}

// A relayed read asks the peer for IXB2 with float64 metrics, and keeps the
// count, every metric and the redirect as the peer holds them.
func TestGetDecodesBlockWithEveryMetric(t *testing.T) {
	redirects := []wire.SetsRedirect{{Location: "aa", Name: "AAAAAAAAAAAAAAAC", IP: "10.0.0.7", Port: 21007}}
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/sets" || query.Has("format") || query.Get("metrics") != "f64" || query.Get("deep") != "false" {
			t.Errorf("request %s", r.URL)
		}
		body, err := wire.EncodeSetsBlock(relayedSet(), []string{"aa"}, wire.SetsPrecision, redirects, wire.SetsBlockF64)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", wire.SetsBlockContentType)
		_, _ = w.Write(body)
	})
	contact, set, err := c.Get("Demo000000000000", "aa", false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if contact == nil || contact.Name() != "AAAAAAAAAAAAAAAC" || contact.Port() != 21007 {
		t.Fatalf("redirect contact %v", contact)
	}
	got, _ := set.Get("aab")
	want, _ := relayedSet()["aa"].Get("aab")
	if got == nil || !got.IsEqual(want) {
		t.Fatalf("aab = %v, want %v", got, want)
	}
	// Keys lose their reference on the wire, as in v1; the section is as wide
	// as its widest row.
	if leaf, _ := set.Get("aac"); leaf == nil || leaf.Count() != 1 || leaf.Metric(0) != 0.1 || leaf.Metric(1) != 0.2 || leaf.Metric(2) != 0 {
		t.Fatalf("aac = %v", leaf)
	}
}

// /sets answers IXB2 only: anything else, or a corrupt block, is an error.
func TestGetRefusesAnythingButIXB2(t *testing.T) {
	_, old := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 0, 0, 1})
	})
	if _, _, err := old.Get("Demo000000000000", "aa", false, nil, false); err == nil {
		t.Fatal("a v1 answer was accepted")
	}
	_, corrupt := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", wire.SetsBlockContentType)
		_, _ = w.Write([]byte("IXB1 not a block"))
	})
	if _, _, err := corrupt.Get("Demo000000000000", "aa", false, nil, false); err == nil {
		t.Fatal("corrupt block accepted")
	}
}

// BenchmarkGetRelayed prices one peer /sets read of 10 000 rows, as IXB2
// with float64 metrics. "wire" is the body the link carries, uncompressed
// as HttpClient asks; "gzip" what it would be at the level /sets uses.
func BenchmarkGetRelayed(b *testing.B) {
	set := wire.NewSet()
	for i := 0; i < 10_000; i++ {
		price := float64(150_000 + i*37%900_000)
		surface := float64(60 + i%140)
		set.Put(fmt.Sprintf("aa%06x", i), wire.NewAbelian(1+i%5, []float64{price, surface, price / surface, 138 + float64(i%997)/1e4, 182 + float64(i%991)/1e4}))
	}
	sets := map[string]*indexus.Set{"aa": set}
	block, err := wire.EncodeSetsBlock(sets, []string{"aa"}, wire.SetsPrecision, nil, wire.SetsBlockF64)
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct {
		name, contentType string
		body              []byte
	}{
		{"block-f64", wire.SetsBlockContentType, block},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				_, _ = w.Write(c.body)
			}))
			defer server.Close()
			address := server.Listener.Addr().(*net.TCPAddr)
			contact := &Contact{name: "PeerAAAAAAAAAAAA", ips: map[string]any{address.IP.String(): nil}, ip: address.IP.String(), port: address.Port}
			var zipped bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&zipped, 2)
			_, _ = zw.Write(c.body)
			_ = zw.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, got, err := contact.Get("Demo000000000000", "aa", true, nil, false)
				if err != nil || got == nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(c.body)), "wire")
			b.ReportMetric(float64(zipped.Len()), "gzip")
		})
	}
}
