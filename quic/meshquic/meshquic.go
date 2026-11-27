// Package meshquic puts the node on HTTP/3 (NET-1): its p2p and monitoring
// servers answer HTTP/3 over UDP on the port that serves HTTPS over TCP, and
// its mesh client calls https peers over HTTP/3 with a TCP fallback.
//
// Install fills internal/adapter/transport, the node's extension point; the
// node's own module links no QUIC stack.
package meshquic

import (
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/indexus/go-indexus-core/gateway/http3server"
	"github.com/indexus/go-indexus-core/internal/adapter/transport"
	"github.com/indexus/go-indexus-core/internal/metrics"
)

// Install must run before the node starts: servers and the shared mesh
// client read the hooks once.
func Install() {
	transport.NewServer = func(server *http.Server) transport.Server { return &http3server.Server{HTTP: server} }
	transport.Client = func(tcp *http.Transport) http.RoundTripper { return newTransport(tcp) }
}

// Mesh calls between nodes take the same path as clients (NET-1): HTTP/3
// over QUIC to an https peer, on the port its HTTPS listener also serves
// over TCP. Plain http peers stay on TCP.
//
// A peer that cannot be reached over QUIC (UDP filtered, HTTP/3 off) is
// retried at once over TCP and stays on TCP for tcpFallbackFor. Retrying is
// safe: every mesh request is idempotent by its RequestID or Operation, and
// callers already retry transport failures.
const tcpFallbackFor = 5 * time.Minute

type roundTripper struct {
	tcp *http.Transport
	h3  *http3.Transport

	mu       sync.Mutex
	tcpUntil map[string]time.Time
}

func newTransport(tcp *http.Transport) *roundTripper {
	var roots = tcp.TLSClientConfig
	conf := &tls.Config{MinVersion: tls.VersionTLS13}
	if roots != nil {
		conf.RootCAs = roots.RootCAs
	}
	return &roundTripper{
		tcp: tcp,
		h3: &http3.Transport{
			TLSClientConfig: conf,
			QUICConfig: &quic.Config{
				HandshakeIdleTimeout: 2 * time.Second,
				MaxIdleTimeout:       time.Minute,
				KeepAlivePeriod:      20 * time.Second,
			},
		},
		tcpUntil: map[string]time.Time{},
	}
}

func (t *roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || t.onTCP(r.URL.Host) {
		metrics.Transport.TCP.Add(1)
		return t.tcp.RoundTrip(r)
	}
	response, err := t.h3.RoundTrip(r)
	if err == nil {
		metrics.Transport.HTTP3.Add(1)
		return response, nil
	}
	if r.Context().Err() != nil {
		return nil, err
	}
	retry := r.Clone(r.Context())
	if r.Body != nil && r.Body != http.NoBody {
		if r.GetBody == nil {
			return nil, err
		}
		body, bodyErr := r.GetBody()
		if bodyErr != nil {
			return nil, err
		}
		retry.Body = body
	}
	t.mu.Lock()
	t.tcpUntil[r.URL.Host] = time.Now().Add(tcpFallbackFor)
	t.mu.Unlock()
	metrics.Transport.Fallbacks.Add(1)
	metrics.Transport.TCP.Add(1)
	return t.tcp.RoundTrip(retry)
}

func (t *roundTripper) onTCP(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	until, ok := t.tcpUntil[host]
	if ok && time.Now().After(until) {
		delete(t.tcpUntil, host)
		return false
	}
	return ok
}

// CloseIdleConnections lets http.Client release both pools.
func (t *roundTripper) CloseIdleConnections() {
	t.tcp.CloseIdleConnections()
	t.h3.CloseIdleConnections()
}
