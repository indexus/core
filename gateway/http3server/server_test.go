package http3server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func certificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	defer seed.Close()
	cert := seed.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	crt, k := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(crt, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return crt, k, roots
}

func launch(t *testing.T, handler http.Handler, udpBlocked bool) (string, *tls.Config) {
	t.Helper()
	crt, key, roots := certificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if udpBlocked {
		udp, err := net.ListenPacket("udp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = udp.Close() })
	}
	server := &Server{HTTP: &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener, crt, key) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})
	return "https://" + listener.Addr().String(), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
}

func TestHTTP3AndHTTP2SameHandlerAndTCPFallback(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "quic", true: "udp-unavailable"}[blocked], func(t *testing.T) {
			endpoint, conf := launch(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Proto) }), blocked)
			tcpTransport := &http.Transport{TLSClientConfig: conf, ForceAttemptHTTP2: true}
			defer tcpTransport.CloseIdleConnections()
			tcp := &http.Client{Transport: tcpTransport, Timeout: 3 * time.Second}
			response, err := tcp.Get(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.ProtoMajor != 2 || string(body) != "HTTP/2.0" {
				t.Fatalf("TCP response: %s %s", response.Proto, body)
			}
			h1Transport := &http.Transport{TLSClientConfig: conf.Clone(), Protocols: new(http.Protocols)}
			h1Transport.Protocols.SetHTTP1(true)
			h1Transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
			defer h1Transport.CloseIdleConnections()
			h1 := &http.Client{Transport: h1Transport, Timeout: 3 * time.Second}
			h1Response, h1Err := h1.Get(endpoint)
			if h1Err != nil {
				t.Fatal(h1Err)
			}
			_, _ = io.Copy(io.Discard, h1Response.Body)
			h1Response.Body.Close()
			if h1Response.ProtoMajor != 1 {
				t.Fatalf("HTTP/1 fallback: %s", h1Response.Proto)
			}
			if blocked {
				if response.Header.Get("Alt-Svc") != "" {
					t.Fatal("advertised unavailable UDP")
				}
				return
			}
			if !strings.Contains(response.Header.Get("Alt-Svc"), "h3=") {
				t.Fatal("missing HTTP3 advertisement")
			}
			transport := &http3.Transport{TLSClientConfig: conf}
			defer transport.Close()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, err = client.Get(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(response.Body)
			response.Body.Close()
			if response.ProtoMajor != 3 || string(body) != "HTTP/3.0" {
				t.Fatalf("QUIC response: %s %s", response.Proto, body)
			}
		})
	}
}

func TestHTTP3CancellationReleasesProxyUpstream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blocked" {
			_, _ = io.WriteString(w, "ready")
			return
		}
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() == nil {
			http.Error(w, err.Error(), 502)
		}
	}
	endpoint, conf := launch(t, proxy, false)
	transport := &http3.Transport{TLSClientConfig: conf}
	defer transport.Close()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", endpoint+"/blocked", nil)
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("canceled HTTP3 stream kept upstream work alive")
	}
	response, err := client.Get(endpoint + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "ready" || response.ProtoMajor != 3 {
		t.Fatalf("connection unusable after cancellation: %s", body)
	}
}

// NET-2: one client connection may hold far more than quic-go's default 100
// requests open at once. Every request here waits until all of them have
// arrived, so a per-connection cap below the count deadlocks until the timeout.
func TestHTTP3OneConnectionCarriesManyConcurrentRequests(t *testing.T) {
	const concurrent = 300
	var arrived sync.WaitGroup
	arrived.Add(concurrent)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	base, conf := launch(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		select {
		case <-all:
		case <-r.Context().Done():
		}
	}), false)
	transport := &http3.Transport{TLSClientConfig: conf}
	defer transport.Close()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := client.Get(base + "/")
			if err != nil {
				failed.Add(1)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatalf("%d of %d concurrent requests on one connection did not complete", failed.Load(), concurrent)
	}
}
