package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/http/monitoring"
	"github.com/indexus/go-indexus-core/internal/adapter/http/p2p"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
)

// A vnode is monitored between Open and the end of its join: /status and
// /metrics answer, report it as joining, and stop once the join returned.
func TestVNodeIsMonitoredWhileItJoins(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	config := Config{Name: indexus.BASE64.Encode(zone.Bytes()), Storage: filepath.Join(t.TempDir(), "node"), Advertise: []string{"127.0.0.1"},
		ClusterID: "integration", ClusterCapacity: 1 << 24, P2pPort: port,
		Bootstraps: []indexus.Contact{peer.NewContact(source.node.Name(), map[string]any{"127.0.0.1": nil}, source.port)}}
	settings, err := indexus.NewSettings(config.Name, port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := indexus.NewNode(settings, peer.NewContact, config.Bootstraps)
	if err != nil {
		t.Fatal(err)
	}
	server := p2p.NewHttpHandler("", n, peer.NewContact)
	boot, err := configureCluster(config, n, server, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup((&runningDataNode{node: n, boot: boot, server: server, port: port}).stop)
	monitor := monitoring.NewHttpHandler("", n, "test")
	joining := func(path string, handle http.HandlerFunc) bool {
		t.Helper()
		rec := httptest.NewRecorder()
		handle(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body struct {
			Joining *bool `json:"joining"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || body.Joining == nil {
			t.Fatalf("%s: status %d, %v, body %s", path, rec.Code, err, rec.Body.String())
		}
		return *body.Joining
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := boot.Open(ctx, n); err != nil {
		t.Fatal(err)
	}
	if joining("/status", monitor.Status) || joining("/metrics", monitor.Metrics) {
		t.Fatal("joining before the join started")
	}

	// A join that runs out of time leaves the vnode joining: the process
	// keeps it running and keepJoining finishes it.
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := boot.Join(expired, n); !errors.Is(err, errJoinUnfinished) {
		t.Fatalf("join without time: %v", err)
	}
	if !joining("/status", monitor.Status) || !joining("/metrics", monitor.Metrics) {
		t.Fatal("an unfinished join is not reported as joining")
	}

	if err := boot.Join(ctx, n); err != nil {
		t.Fatal(err)
	}
	if joining("/status", monitor.Status) || joining("/metrics", monitor.Metrics) || !n.ClientReady() {
		t.Fatalf("after the join: joining or not ready (ready=%v)", n.ClientReady())
	}
}
