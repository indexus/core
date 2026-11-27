package meshhttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestSharedHTTPPoolPreservesRefusalAndCancellation(t *testing.T) {
	first, second := NewClient(), NewClient()
	if first.HTTP != second.HTTP || first.HTTP != sharedHTTPClient {
		t.Fatal("default clients do not share a pool")
	}
	_, ok := sharedHTTPClient.Transport.(*layered)
	if tcp := sharedTCP; !ok || tcp.MaxIdleConns != 512 || tcp.MaxIdleConnsPerHost != 128 || tcp.MaxConnsPerHost != 128 || !tcp.ForceAttemptHTTP2 {
		t.Fatal("missing explicit transport budgets")
	}
	var connections atomic.Int64
	server := httptest.NewUnstartedServer(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
		return mesh.Response{Applied: true}, &storage.WriteAdmissionError{Pending: 2, MaxPending: 2, Bytes: 100, MaxBytes: 100}
	}})
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	// A zero-value Client uses the same shared pool as explicitly constructed clients.
	for _, client := range []*Client{first, second, {}} {
		response, err := client.Exchange(context.Background(), server.URL, mesh.Request{Kind: "write"})
		if !errors.Is(err, storage.ErrWriteOverload) || !response.Applied {
			t.Fatalf("lost typed refusal or envelope: %+v %v", response, err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("pool opened %d connections for sequential calls", connections.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := first.Exchange(ctx, server.URL, mesh.Request{Kind: "membership"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RPC: %v", err)
	}
}

func TestSharedHTTPPoolKeepsRequestDeadline(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := NewClient().Exchange(ctx, server.URL, mesh.Request{Kind: "membership"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline replaced by transport timeout: %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("request did not reach server")
	}
}
