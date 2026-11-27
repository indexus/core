package diagnostics

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strconv"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

// Explicit, loopback-only diagnostics for local memory audits. Disabled by
// default; never register profiling handlers on the public node/monitoring mux.
func Start(node *indexus.Node) (func(), error) {
	port := os.Getenv("INDEXUS_LOCAL_DEBUG_PORT")
	if port == "" {
		return func() {}, nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1024 || n > 65535 {
		return nil, fmt.Errorf("invalid INDEXUS_LOCAL_DEBUG_PORT")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/memory", func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/debug/summary", func(w http.ResponseWriter, r *http.Request) {
		if node != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"cache": node.SummaryCacheSizes(), "protocol": node.SummaryStats()})
			return
		}
		http.Error(w, "node starting", http.StatusServiceUnavailable)
	})
	go func() {
		if err := http.Serve(listener, mux); err != nil {
			slog.Error("local diagnostics stopped", "err", err)
		}
	}()
	return func() { _ = listener.Close() }, nil
}
