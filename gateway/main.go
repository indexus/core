// gateway adds a same-origin HTTPS/HTTP3 entry point to an existing dashboard.
// Its upstream remains loopback HTTP; it never replays application requests.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/gateway/http3server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:5443", "HTTPS TCP and HTTP/3 UDP address")
	upstream := flag.String("upstream", "http://127.0.0.1:5173", "dashboard upstream")
	cert := flag.String("cert", "", "TLS certificate, trusted by clients")
	key := flag.String("key", "", "TLS private key")
	flag.Parse()
	target, err := url.Parse(*upstream)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") || *cert == "" || *key == "" {
		log.Fatal("valid upstream and -cert/-key are required")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() == nil {
			log.Printf("gateway upstream: %v", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
	}
	server := &http3server.Server{HTTP: &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Indexus-Transport", r.Proto)
		proxy.ServeHTTP(w, r)
	})}}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener, *cert, *key) }()
	log.Printf("dashboard HTTPS + HTTP/3: https://%s -> %s", listener.Addr(), target)
	select {
	case err = <-failed:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
