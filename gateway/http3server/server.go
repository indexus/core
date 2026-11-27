// Package http3server serves the same handler over HTTPS (HTTP/1.1 and HTTP/2)
// and HTTP/3 on the same TCP/UDP port. Plain HTTP remains available without TLS.
package http3server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// MaxStreams bounds the concurrent requests one client connection may have
// open (NET-2). quic-go's default, 100, silently capped a whole load machine,
// or a peer relaying writes, at 100 requests per node: requests queued in the
// client waiting for stream credit while every CPU stayed idle. HTTP/2 over
// TCP had allowed 128 connections per host. Admission control stays with the
// node's write batcher, which refuses with an explicit overload.
const MaxStreams = 4096

type Server struct {
	HTTP    *http.Server
	mu      sync.Mutex
	quic    *http3.Server
	stopped bool
}

func (s *Server) Serve(listener net.Listener, certFile, keyFile string) error {
	if certFile == "" && keyFile == "" {
		return s.HTTP.Serve(listener)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		_ = listener.Close()
		return err
	}
	conf := s.HTTP.TLSConfig
	if conf == nil {
		conf = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		conf = conf.Clone()
	}
	conf.Certificates = []tls.Certificate{cert}
	s.HTTP.TLSConfig = conf
	udp, err := net.ListenPacket("udp", listener.Addr().String())
	if err != nil {
		slog.Warn("HTTP/3 unavailable; serving HTTPS over TCP", "error", err)
		return s.HTTP.ServeTLS(listener, "", "")
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	handler := s.HTTP.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	q := &http3.Server{Port: port, Handler: handler, TLSConfig: http3.ConfigureTLSConfig(conf.Clone()), IdleTimeout: time.Minute,
		QUICConfig: &quic.Config{MaxIncomingStreams: MaxStreams, KeepAlivePeriod: 20 * time.Second}}
	var active atomic.Bool
	active.Store(true)
	s.HTTP.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if active.Load() {
			_ = q.SetQUICHeaders(w.Header())
		}
		handler.ServeHTTP(w, r)
	})
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		_ = udp.Close()
		_ = listener.Close()
		return http.ErrServerClosed
	}
	s.quic = q
	s.mu.Unlock()
	defer udp.Close()
	defer q.Close()
	go func() {
		if err := q.Serve(udp); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			slog.Warn("HTTP/3 stopped; HTTPS TCP fallback remains", "error", err)
		}
		active.Store(false)
	}()
	return s.HTTP.ServeTLS(listener, "", "")
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	q := s.quic
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		if q != nil {
			done <- q.Shutdown(ctx)
		} else {
			done <- nil
		}
	}()
	err := s.HTTP.Shutdown(ctx)
	quicErr := <-done
	if ctx.Err() != nil {
		_ = s.HTTP.Close()
		if q != nil {
			_ = q.Close()
		}
	}
	return errors.Join(err, quicErr)
}
