package meshquic

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/gateway/http3server"
	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"github.com/indexus/go-indexus-core/internal/adapter/transport"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/metrics"
)

// selfSigned writes a certificate for 127.0.0.1 and returns its files and pool.
func selfSigned(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	crt, k := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return crt, k, pool
}

func trustingClient(pool *x509.CertPool) (*meshhttp.Client, *roundTripper) {
	tcp := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: pool}}
	t := newTransport(tcp)
	return &meshhttp.Client{HTTP: &http.Client{Timeout: 10 * time.Second, Transport: t}}, t
}

func echo(protocols *atomic.Int32) http.Handler {
	mesh := &meshhttp.Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) { return mesh.Response{Applied: true}, nil }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocols.Store(int32(r.ProtoMajor))
		mesh.ServeHTTP(w, r)
	})
}

// NET-1: an https peer is called over HTTP/3, on the port that also serves
// HTTPS over TCP, the same listener clients reach.
func TestMeshCallsHTTPSPeersOverHTTP3(t *testing.T) {
	crt, key, pool := selfSigned(t)
	var protocol atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http3server.Server{HTTP: &http.Server{Handler: echo(&protocol)}}
	go server.Serve(listener, crt, key)
	defer server.Shutdown(context.Background())
	client, _ := trustingClient(pool)
	before, fallbacks := metrics.Transport.HTTP3.Load(), metrics.Transport.Fallbacks.Load()
	var response mesh.Response
	for range 50 {
		response, err = client.Exchange(context.Background(), "https://"+listener.Addr().String(), mesh.Request{Kind: "exchange"})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond) // the UDP side starts beside TCP
	}
	if err != nil || !response.Applied {
		t.Fatalf("call failed: %+v %v", response, err)
	}
	if protocol.Load() != 3 || metrics.Transport.HTTP3.Load() == before {
		t.Fatalf("peer reached over HTTP/%d, not HTTP/3", protocol.Load())
	}
	if metrics.Transport.Fallbacks.Load() != fallbacks {
		t.Fatal("fell back to TCP with HTTP/3 available")
	}
}

// NET-1: a peer without HTTP/3 is retried over TCP at once and then stays on
// TCP, so one filtered UDP path costs one failed handshake, not every call.
func TestMeshFallsBackToTCPWithoutHTTP3(t *testing.T) {
	_, _, pool := selfSigned(t)
	var protocol atomic.Int32
	server := httptest.NewUnstartedServer(echo(&protocol))
	crt, key, _ := selfSigned(t)
	cert, err := tls.LoadX509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	defer server.Close()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pool.AddCert(leaf)
	client, tr := trustingClient(pool)
	fallbacks := metrics.Transport.Fallbacks.Load()
	for i := range 3 {
		response, err := client.Exchange(context.Background(), server.URL, mesh.Request{Kind: "exchange"})
		if err != nil || !response.Applied {
			t.Fatalf("call %d failed: %+v %v", i, response, err)
		}
	}
	// A bodiless GET falls back the same way (monitoring, health checks).
	fresh, _ := trustingClient(pool)
	get, err := fresh.HTTP.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("GET did not fall back: %v", err)
	}
	get.Body.Close()
	if got := metrics.Transport.Fallbacks.Load() - fallbacks; got != 2 {
		t.Fatalf("%d fallbacks for one peer, want 2 (one per client)", got)
	}
	if protocol.Load() == 3 || !tr.onTCP(server.Listener.Addr().String()) {
		t.Fatal("peer not kept on TCP after the fallback")
	}
}

// Install fills the node's extension point: servers answer HTTP/3 beside
// HTTPS, and the shared mesh client layers HTTP/3 over its TCP pool.
func TestInstallFillsTheTransportHooks(t *testing.T) {
	server, client := transport.NewServer, transport.Client
	defer func() { transport.NewServer, transport.Client = server, client }()
	Install()
	if _, ok := transport.NewServer(&http.Server{}).(*http3server.Server); !ok {
		t.Fatal("servers do not serve HTTP/3")
	}
	if _, ok := meshhttp.NewTransport(&http.Transport{}).(*roundTripper); !ok {
		t.Fatal("cluster clients do not call over HTTP/3")
	}
}

func TestNeighbourProbeAllowsQUICHandshakeThenTLSFallback(t *testing.T) {
	const name = "AAAAAAAAAAAAAAAB"
	var port int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.URL.Path != "/ping" {
			t.Errorf("probe lost TLS: %s", r.URL)
		}
		json.NewEncoder(w).Encode(map[string]any{"contact": map[string]any{"name": name, "ips": map[string]any{"127.0.0.1": nil}, "port": port, "scheme": "https"}})
	}))
	defer server.Close()
	port = server.Listener.Addr().(*net.TCPAddr).Port
	// Real UDP socket with no reader: the QUIC attempt must exhaust its own
	// handshake budget before the authenticated TCP/TLS request gets a turn.
	blackhole, err := net.ListenPacket("udp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	client, tr := trustingClient(pool)
	defer tr.CloseIdleConnections()
	previous := peer.HttpClient
	client.HTTP.Timeout = previous.Timeout
	peer.HttpClient = client.HTTP
	defer func() { peer.HttpClient = previous }()
	contact := peer.NewTLSContact(name, map[string]any{"127.0.0.1": nil}, port)
	for range 2 {
		if _, err := contact.Ping(contact); err != nil {
			t.Fatal("neighbour probe deadline prevented TLS fallback", err)
		}
	}
	if !tr.onTCP(server.Listener.Addr().String()) {
		t.Fatal("fallback was not retained for the next probe")
	}
}
