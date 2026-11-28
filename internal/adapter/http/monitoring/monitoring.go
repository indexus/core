package monitoring

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/observability/logging"
	"github.com/indexus/go-indexus-core/internal/adapter/transport"
	"github.com/indexus/go-indexus-core/internal/metrics"
)

// Service is the part of the node the monitoring endpoints report on.
type Service interface {
	Name() string
	ID() []byte
	Acknowledged() ([]indexus.Contact, error)
	Registered() ([]indexus.Contact, error)
	Routing() ([]indexus.Contact, error)
	Ownership() (map[string]map[string]map[string]any, error)
	MeasuredOwnershipStats() (collections, zones int)
	Items() int
	AutoscaleSnapshot() map[string]any
	ClientReady() bool
	StorageUsage() (capacity, owned int64, err error)
	CommitStats() map[string]any
	SummaryMetrics() map[string]any
}

type Handler struct {
	service Service
	version string
	build   buildInfo
	tlsDir  string
	started time.Time
	server  *http.Server
	serve   transport.Server // TCP, or TCP and HTTP/3 in core/quic (NET-1)
}

// NewHttpHandler wires the monitoring endpoints. A non-empty tlsDir must hold
// server.crt and server.key.
func NewHttpHandler(tlsDir string, service Service, version string) *Handler {
	handler := &Handler{
		service: service,
		version: version,
		build:   currentBuild(),
		tlsDir:  tlsDir,
		started: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.Health)
	mux.HandleFunc("/status", handler.Status)
	mux.HandleFunc("/acknowledged", hosts(service.Acknowledged))
	mux.HandleFunc("/registered", hosts(service.Registered))
	mux.HandleFunc("/routing", hosts(service.Routing))
	mux.HandleFunc("/ownership", handler.Ownership)
	mux.HandleFunc("/count", handler.Count)
	mux.HandleFunc("/autoscale", handler.Autoscale)
	mux.HandleFunc("/metrics", handler.Metrics)
	if os.Getenv("INDEXUS_DEBUG_STACKS") == "1" {
		// Experiments only (the agent sets it): where every goroutine waits,
		// aggregated by stack, to find what a saturated node is waiting on.
		mux.HandleFunc("/debug/stacks", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_ = pprof.Lookup("goroutine").WriteTo(w, 1)
		})
	}

	handler.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          logging.StdLogger(slog.LevelWarn),
	}
	handler.serve = transport.NewServer(handler.server)

	return handler
}

// Serve runs until Shutdown is called, then returns http.ErrServerClosed.
func (h *Handler) Serve(lis net.Listener) error {
	slog.Info("monitoring server listening", "addr", lis.Addr().String(), "tls", h.tlsDir != "")

	if h.tlsDir != "" {
		return h.serve.Serve(lis, h.tlsDir+"/server.crt", h.tlsDir+"/server.key")
	}
	return h.serve.Serve(lis, "", "")
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (h *Handler) Shutdown(ctx context.Context) error {
	return h.serve.Shutdown(ctx)
}

// Health reports whether the durable runtime has activated this vnode.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if !h.service.ClientReady() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "joining"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Status reports the last background measurement of items, zones and
// collections. Like /count, it never scans the data disk on a polling request.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	collections, zones := h.service.MeasuredOwnershipStats()
	peers, err := h.service.Registered()
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":         h.service.Name(),
		"version":      h.version,
		"build":        h.build,
		"uptime_s":     int64(time.Since(h.started).Seconds()),
		"peers":        len(peers),
		"collections":  collections,
		"zones":        zones,
		"items":        h.service.Items(),
		"client_ready": h.service.ClientReady(),
		"joining":      h.joining(),
		"autoscale":    h.service.AutoscaleSnapshot(),
	})
}

// joining: the vnode is still receiving its cell (its initial join runs).
// Placement reads it to count the joins a machine is paying for.
func (h *Handler) joining() bool {
	j, ok := h.service.(interface{ Joining() bool })
	return ok && j.Joining()
}

// Metrics is the experiment document: cumulative wire, write, gossip and
// handoff counters (internal/metrics), plus storage and process gauges. It is
// cheap enough to poll every second.
func (h *Handler) Metrics(w http.ResponseWriter, r *http.Request) {
	body := metrics.Snapshot()
	body["name"] = h.service.Name()
	body["client_ready"] = h.service.ClientReady()
	body["joining"] = h.joining()
	body["autoscale"] = h.service.AutoscaleSnapshot()
	body["id"] = hex.EncodeToString(h.service.ID()) // the vnode's XOR address, for the dashboard's trie
	collections, zones := h.service.MeasuredOwnershipStats()
	capacity, owned, _ := h.service.StorageUsage()
	body["storage"] = map[string]any{"capacity_bytes": capacity, "owned_bytes": owned, "items": h.service.Items(), "zones": zones, "collections": collections}
	if reporter, ok := h.service.(interface{ NativeAddressShare() (float64, bool) }); ok {
		if share, known := reporter.NativeAddressShare(); known {
			body["native_address_share"] = share
		}
	}
	body["disk"] = h.service.CommitStats()
	body["summary"] = h.service.SummaryMetrics()
	// Outgoing transfers with frozen zones and not installed: while one is
	// open, complete reads on this node refuse (ErrFrozen). One that stays is
	// a fence left behind.
	if f, ok := h.service.(interface{ FrozenTransfers() []string }); ok {
		body["frozen_transfers"] = f.FrozenTransfers()
	}
	// Write demand at this vnode, counted on arrival and not after service:
	// it stays visible when the store stops acknowledging.
	if a, ok := h.service.(interface{ ItemsAge() (time.Duration, bool) }); ok {
		storage := body["storage"].(map[string]any)
		age, counted := a.ItemsAge()
		storage["items_counted"], storage["items_age_ms"] = counted, age.Milliseconds()
	}
	if d, ok := h.service.(interface{ WriteDemand() map[string]any }); ok {
		body["demand"] = d.WriteDemand()
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	body["process"] = map[string]any{
		"goroutines": runtime.NumGoroutine(), "heap_alloc_bytes": mem.HeapAlloc, "heap_sys_bytes": mem.HeapSys,
		"gc_cycles": mem.NumGC, "cpu_seconds": cpuSeconds(),
	}
	if udp := udpCounters("/proc/net/snmp"); udp != nil {
		body["udp"] = udp
	}
	writeJSON(w, http.StatusOK, body)
}

// Ownership explicitly enumerates writable zones and may scan the data disk.
// Frequent polling should use /status and its background measurements.
func (h *Handler) Ownership(w http.ResponseWriter, r *http.Request) {
	body, err := h.service.Ownership()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// Queue reports the ingress backlog and, when it is not draining, the errors
// holding it there.

// Count reports the last background MeasureItems totals (same atomics as
// /status). It does not walk the collection on the request path.
func (h *Handler) Count(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"count": h.service.Items(),
	})
}

// Leave hands every owned zone over to peers, then snapshots. The autoscaler
// calls it before terminating an instance.

// Autoscale exposes the insert window and the hysteresis state behind scaling.
func (h *Handler) Autoscale(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.AutoscaleSnapshot())
}

// Checkpoint flushes a local snapshot when the WAL is dirty, then zone snaps.

// Snapshots lists object-store keys under zones/, nodes/, and snapshots/.

// SnapshotsClear deletes object-store keys under zones/, nodes/, and snapshots/.

// hosts renders one of the routing tables as a list of hosts.
func hosts(list func() ([]indexus.Contact, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contacts, err := list()
		if err != nil {
			writeError(w, err)
			return
		}

		body := make([]string, 0, len(contacts))
		for _, contact := range contacts {
			body = append(body, contact.Host())
		}
		writeJSON(w, http.StatusOK, map[string][]string{"hosts": body})
	}
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
}

// cpuSeconds is user plus system CPU time of this process.
// udpCounters reads the host's UDP counters (Linux /proc/net/snmp): QUIC
// carries the mesh over UDP, and RcvbufErrors or SndbufErrors growing under
// load mean the kernel dropped datagrams for want of socket buffer.
func udpCounters(path string) map[string]int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "Udp:" {
			continue
		}
		if names == nil {
			names = fields[1:]
			continue
		}
		out := make(map[string]int64, len(names))
		for i, name := range names {
			if i+1 < len(fields) {
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					out[name] = v
				}
			}
		}
		return out
	}
	return nil
}

func cpuSeconds() float64 {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}
