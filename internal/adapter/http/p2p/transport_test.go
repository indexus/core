package p2p

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeHTTPAndTLSKeepAuthenticationAndShutdown(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "tls-http2"}[secure], func(t *testing.T) {
			dir := ""
			scheme := "http"
			transport := &http.Transport{ForceAttemptHTTP2: true}
			defer transport.CloseIdleConnections()
			if secure {
				scheme = "https"
				dir = t.TempDir()
				seed := httptest.NewTLSServer(http.NotFoundHandler())
				cert := seed.TLS.Certificates[0]
				seed.Close()
				key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				raw := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
				if err := os.WriteFile(filepath.Join(dir, "server.crt"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "server.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM(raw)
				transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			handler := NewHttpHandler(dir, nil, nil)
			handler.RequireAuth = true
			// An auth-gated adapter must refuse access even if the verifier is unavailable.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- handler.Serve(listener) }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			defer func() {
				if err := handler.Shutdown(ctx); err != nil {
					t.Error(err)
				}
				if err := <-done; !errors.Is(err, http.ErrServerClosed) {
					t.Error(err)
				}
			}()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			response, err := client.Get(scheme + "://" + listener.Addr().String() + "/records?collection=test&location=@")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated read: %d", response.StatusCode)
			}
			content, err := client.Get(scheme + "://" + listener.Addr().String() + "/content?collection=test&location=0000000000000000&path=test.bin")
			if err != nil {
				t.Fatal(err)
			}
			content.Body.Close()
			if content.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated content read: %d", content.StatusCode)
			}
			if secure && response.ProtoMajor != 2 {
				t.Fatalf("TLS did not negotiate HTTP/2: %s", response.Proto)
			}
			if response.Header.Get("Alt-Svc") != "" {
				t.Fatal("backend advertises QUIC")
			}
		})
	}
}
