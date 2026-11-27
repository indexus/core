package meshhttp

import (
	"net/http"
	"sync"

	"github.com/indexus/go-indexus-core/internal/adapter/transport"
	"github.com/indexus/go-indexus-core/internal/metrics"
)

// layered is the shared client's transport: transport.Client over the TCP
// pool, resolved at the first call so that core/quic can set the hook in
// main before the node starts. The node's own build stays on TCP; the quic
// build calls https peers over HTTP/3 with a TCP fallback (NET-1).
type layered struct {
	tcp  *http.Transport
	once sync.Once
	next http.RoundTripper
}

func (l *layered) resolve() http.RoundTripper {
	l.once.Do(func() { l.next = NewTransport(l.tcp) })
	return l.next
}

func (l *layered) RoundTrip(r *http.Request) (*http.Response, error) {
	return l.resolve().RoundTrip(r)
}

// CloseIdleConnections lets http.Client release every pool.
func (l *layered) CloseIdleConnections() {
	if closer, ok := l.resolve().(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// NewTransport layers transport.Client over tcp, for other clients of the
// cluster (the experiment loader) that should reach nodes like the mesh.
// Without the quic hook every call is counted under transport.tcp.
func NewTransport(tcp *http.Transport) http.RoundTripper {
	if transport.Client != nil {
		return transport.Client(tcp)
	}
	return countedTCP{tcp}
}

type countedTCP struct{ *http.Transport }

func (c countedTCP) RoundTrip(r *http.Request) (*http.Response, error) {
	metrics.Transport.TCP.Add(1)
	return c.Transport.RoundTrip(r)
}
