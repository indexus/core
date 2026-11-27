// Package transport is the node's extension point for an optional network
// stack (NET-1, docs/http3.md).
//
// The node's module links net/http only: its servers speak HTTPS over TCP
// (HTTP/1.1 and HTTP/2) and its mesh client keeps a pooled TCP transport.
// HTTP/3 needs quic-go, which the node's module does not require. The
// separate module core/quic links it and sets these two hooks before the
// node starts; nothing else may assign them.
package transport

import (
	"context"
	"net"
	"net/http"
)

// Server serves one handler on a listener: with a certificate pair over
// TLS, without one in plain HTTP.
type Server interface {
	Serve(listener net.Listener, certFile, keyFile string) error
	Shutdown(ctx context.Context) error
}

// NewServer wraps the node's p2p and monitoring servers. The default serves
// TCP only.
var NewServer = func(server *http.Server) Server { return tcpServer{server} }

// Client, when set, layers the mesh client (and other cluster clients) over
// their TCP transport. Unset, they call over TCP.
var Client func(tcp *http.Transport) http.RoundTripper

type tcpServer struct{ *http.Server }

func (s tcpServer) Serve(listener net.Listener, certFile, keyFile string) error {
	if certFile == "" && keyFile == "" {
		return s.Server.Serve(listener)
	}
	return s.Server.ServeTLS(listener, certFile, keyFile)
}
