package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/observability/logging"
	"github.com/indexus/go-indexus-core/internal/adapter/transport"
	"github.com/indexus/go-indexus-core/internal/auth"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type Contact struct {
	Scheme string         `json:"scheme,omitempty"`
	Name   string         `json:"name"`
	IPs    map[string]any `json:"ips"`
	Port   int            `json:"port"`
	IP     string         `json:"ip"`
	Cert   *auth.NodeCert `json:"cert,omitempty"`
}

type Peer struct {
	id   []byte
	name string
}

func NewPeer(name string) (*Peer, error) {

	id, err := indexus.DecodeVnodeID(name)
	if err != nil {
		return nil, err
	}

	return &Peer{
		id:   id,
		name: name,
	}, nil
}

func (p *Peer) ID() []byte {
	return p.id
}

func (p *Peer) Name() string {
	return p.name
}

type Service interface {
	Name() string
	Ping(indexus.Contact) (indexus.Contact, error)
	Neighbors(indexus.Peer) ([]indexus.Contact, error)
	Random(indexus.Peer) (indexus.Contact, error)
	GetSetContext(context.Context, string, string, bool, indexus.Visited, bool) (indexus.Contact, *indexus.Set, error)
	ReadContext(context.Context, string, string, indexus.ReadOptions) (indexus.ReadResult, error)
	GetSetsBlockContext(context.Context, string, []string, bool, indexus.Visited, bool, *indexus.ReadOptions, wire.SetsBlockMetrics, []int) ([]byte, error)
	GetRecordsContext(context.Context, string, string, int) ([]indexus.Item, error)
	GetLocalChildrenContext(context.Context, string, string) ([]string, error)
	GetDirectRecordsContext(context.Context, string, string, int) ([]indexus.Item, error)
	New(*indexus.Item, string, indexus.Visited) error
	Delete(*indexus.Item, string, indexus.Visited) error
	IngestBulk(context.Context, io.Reader, bool) (indexus.BulkResult, error)
}

type Handler struct {
	Ready      func() bool
	Cluster    http.Handler
	Service    Service
	NewContact func(string, map[string]any, int) indexus.Contact

	Verifier    *auth.Verifier
	RequireAuth bool
	SelfCert    *auth.NodeCert

	tlsDir string
	server *http.Server
	serve  transport.Server // TCP, or TCP and HTTP/3 in core/quic (NET-1)
}

func NewHttpHandler(tlsDir string, service Service, newContact func(string, map[string]any, int) indexus.Contact) *Handler {
	handler := &Handler{
		Service:    service,
		NewContact: newContact,
		tlsDir:     tlsDir,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/", func(w http.ResponseWriter, r *http.Request) {
		if handler.Cluster == nil {
			http.NotFound(w, r)
			return
		}
		handler.Cluster.ServeHTTP(w, r)
	})

	mux.HandleFunc("/ping", handler.Ping)
	mux.HandleFunc("/join-hint", handler.JoinHint)

	mux.HandleFunc("/neighbors", handler.Neighbors)
	mux.HandleFunc("/random", handler.Random)

	mux.HandleFunc("/set", handler.GetSet)
	mux.HandleFunc("/set/cached", handler.GetCachedSet)
	mux.HandleFunc("/records", handler.GetRecords)
	mux.HandleFunc("/records/direct", handler.GetDirectRecords)
	mux.HandleFunc("/zones/children/local", handler.GetLocalChildren)
	mux.HandleFunc("/content", handler.GetContent)
	mux.HandleFunc("/sets", handler.GetSets)
	mux.HandleFunc("/aggregates", handler.GetAggregates)
	mux.HandleFunc("/item", handler.New)
	mux.HandleFunc("/item/delete", handler.Delete)
	mux.HandleFunc("/item/degraded", handler.GetDegradedItem)
	mux.HandleFunc("/bulk", handler.Bulk)
	mux.HandleFunc("/collection/", handler.Collection)

	handler.server = &http.Server{
		Handler: withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if handler.Ready != nil && !handler.Ready() && !strings.HasPrefix(r.URL.Path, "/cluster/") {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cluster starting"})
				return
			}
			mux.ServeHTTP(w, r)
		})),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          logging.StdLogger(slog.LevelWarn),
	}
	handler.serve = transport.NewServer(handler.server)

	return handler
}

func (h *Handler) Serve(lis net.Listener) error {
	slog.Info("p2p server listening", "addr", lis.Addr().String(), "tls", h.tlsDir != "", "auth", h.RequireAuth)

	// With TLS the port serves HTTPS over TCP; a node built from core/quic
	// also serves HTTP/3 over UDP on the same port number (NET-1).
	if h.tlsDir != "" {
		return h.serve.Serve(lis, h.tlsDir+"/server.crt", h.tlsDir+"/server.key")
	}
	return h.serve.Serve(lis, "", "")
}

func (h *Handler) Shutdown(ctx context.Context) error {
	return h.serve.Shutdown(ctx)
}

func (h *Handler) clientTokenOK(r *http.Request, scope string) bool {
	if h.Verifier == nil {
		return false
	}
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return false
	}
	tok, err := auth.DecodeToken(strings.TrimPrefix(raw, "Bearer "))
	if err != nil {
		return false
	}
	if err := h.Verifier.VerifyClientToken(tok); err != nil {
		return false
	}
	return tok.HasScope(scope)
}

func (h *Handler) requireClientScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	if !h.RequireAuth {
		return true
	}
	if !h.clientTokenOK(r, scope) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// JoinHint is public: a node without a name yet asks a bootstrap where to
// join, before it holds any credential (JOIN-1). It reveals one random point of
// an already public topology.
func (h *Handler) JoinHint(w http.ResponseWriter, r *http.Request) {
	hinter, ok := h.Service.(interface{ JoinHint() (string, error) })
	if !ok {
		http.NotFound(w, r)
		return
	}
	name, err := hinter.JoinHint()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {

	var bodyReq = struct {
		Origin Contact `json:"origin"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&bodyReq); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ips, err := getClientIPs(r)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if bodyReq.Origin.IPs == nil {
		bodyReq.Origin.IPs = make(map[string]any)
	}
	for _, ip := range ips {
		bodyReq.Origin.IPs[ip] = nil
	}

	if h.RequireAuth && h.Verifier != nil {

		if bodyReq.Origin.Cert != nil {
			if bodyReq.Origin.Cert.NodeID != "" && bodyReq.Origin.Name != "" && bodyReq.Origin.Cert.NodeID != bodyReq.Origin.Name {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "cert node_id mismatch"})
				return
			}
			if err := h.Verifier.VerifyPeerCert(bodyReq.Origin.Cert); err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
				return
			}
		} else if !h.clientTokenOK(r, "read") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing node cert or client token"})
			return
		} else {

			bodyReq.Origin.Name = ""
		}
	}

	if scheme := bodyReq.Origin.Scheme; scheme != "" && scheme != "http" && scheme != "https" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid peer scheme"})
		return
	}
	origin := h.NewContact(bodyReq.Origin.Name, bodyReq.Origin.IPs, bodyReq.Origin.Port)
	if c, ok := origin.(interface{ SetScheme(string) }); ok && bodyReq.Origin.Scheme != "" {
		c.SetScheme(bodyReq.Origin.Scheme)
	}
	if c, ok := origin.(interface{ SetCert(*auth.NodeCert) }); ok && bodyReq.Origin.Cert != nil {
		c.SetCert(bodyReq.Origin.Cert)
	}

	contact, err := h.Service.Ping(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if contact == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	respCert := h.SelfCert
	if respCert == nil {
		if cc, ok := contact.(interface{ Cert() *auth.NodeCert }); ok {
			respCert = cc.Cert()
		}
	}

	scheme := "http"
	if h.tlsDir != "" {
		scheme = "https"
	}
	var bodyResp = struct {
		Contact     Contact `json:"contact"`
		ClientReady bool    `json:"client_ready"`
	}{
		Contact: Contact{
			Scheme: scheme,
			Name:   contact.Name(),
			IPs:    contact.IPs(),
			Port:   contact.Port(),
			Cert:   respCert,
		},
		ClientReady: true,
	}
	if cr, ok := h.Service.(interface{ ClientReady() bool }); ok {
		bodyResp.ClientReady = cr.ClientReady()
	}
	writeJSON(w, http.StatusOK, bodyResp)
}

func (h *Handler) Neighbors(w http.ResponseWriter, r *http.Request) {

	origin, err := NewPeer(r.URL.Query().Get("origin"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	contacts, err := h.Service.Neighbors(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var body = struct {
		Neighbors []Contact `json:"neighbors"`
	}{
		Neighbors: []Contact{},
	}
	for _, contact := range contacts {
		body.Neighbors = append(body.Neighbors, Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
			IP:   contact.IP(),
		})
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) Random(w http.ResponseWriter, r *http.Request) {

	origin, err := NewPeer(r.URL.Query().Get("origin"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	random, err := h.Service.Random(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if random == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var body = struct {
		Random Contact `json:"random"`
	}{
		Random: Contact{
			Name: random.Name(),
			IPs:  random.IPs(),
			Port: random.Port(),
			IP:   random.IP(),
		},
	}
	writeJSON(w, http.StatusOK, body)
}

// routedWriter is a Service that tells where a relayed write was applied.
type routedWriter interface {
	NewRouted(*indexus.Item, string, indexus.Visited) (indexus.WriteRoute, error)
}

// New writes an item. A relayed write's answer carries its owner
// (X-Indexus-Owner, its address) and the owner's cell (X-Indexus-Owner-Cell,
// mask/value): a client may write that cell's keys to the owner directly.
func (h *Handler) New(w http.ResponseWriter, r *http.Request) {
	rw, ok := h.Service.(routedWriter)
	if !ok {
		h.writeItem(w, r, h.Service.New)
		return
	}
	h.writeItem(w, r, func(item *indexus.Item, root string, via indexus.Visited) error {
		route, err := rw.NewRouted(item, root, via)
		if err == nil && route.Owner != "" {
			w.Header().Set("X-Indexus-Owner", route.Owner)
			w.Header().Set("X-Indexus-Owner-Cell", route.Cell)
		}
		return err
	})
}
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	h.writeItem(w, r, h.Service.Delete)
}

func (h *Handler) writeItem(w http.ResponseWriter, r *http.Request, accept func(*indexus.Item, string, indexus.Visited) error) {
	started := time.Now()
	defer func() { metrics.Writes.Handler.Observe(time.Since(started)) }()
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Item *indexus.Item `json:"item"`
		Root string        `json:"root"`
		Via  string        `json:"via"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := accept(body.Item, body.Root, indexus.ParseVisited(body.Via)); err != nil {
		if errors.Is(err, indexus.ErrTooManyMetrics) || errors.Is(err, indexus.ErrInvalidKey) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, indexus.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func getClientIPs(r *http.Request) ([]string, error) {
	var ips []string

	parseAndAppendIPs := func(ipStr string) {
		ipList := strings.Split(ipStr, ",")
		for _, ip := range ipList {
			ip = strings.TrimSpace(ip)
			if ip == "" {
				continue
			}
			parsedIP := net.ParseIP(ip)
			if parsedIP != nil {
				ips = append(ips, parsedIP.String())
			}
		}
	}

	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		parseAndAppendIPs(ip)
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parseAndAppendIPs(xff)
	}

	remoteIP := r.RemoteAddr
	if remoteIP != "" {

		host, _, err := net.SplitHostPort(remoteIP)
		if err != nil {

			host = remoteIP
		}
		host = strings.TrimSpace(host)
		parsedIP := net.ParseIP(host)
		if parsedIP != nil {
			ips = append(ips, parsedIP.String())
		}
	}

	if len(ips) == 0 {
		return nil, errors.New("no valid IPs found")
	}

	return ips, nil
}

// parseReadOptions leaves historical flags unchanged unless a policy is explicit.
