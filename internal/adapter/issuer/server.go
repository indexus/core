package issuer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/adapter/observability/logging"
	"github.com/indexus/go-indexus-core/internal/auth"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type Server struct {
	Issuer       *auth.Issuer
	NodeTTL      time.Duration
	TokenTTL     time.Duration
	BootstrapIPs []string
	P2PPort      int

	mu    sync.Mutex
	certs map[string]*auth.NodeCert
}

func NewServer(iss *auth.Issuer) *Server {
	return &Server{Issuer: iss, NodeTTL: 30 * 24 * time.Hour, TokenTTL: 24 * time.Hour, P2PPort: 21000, certs: map[string]*auth.NodeCert{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "network_id": s.Issuer.NetworkID})
	})
	mux.HandleFunc("/v1/network/pubkey", s.getPubKey)
	mux.HandleFunc("/v1/issue/node", s.issueNode)
	mux.HandleFunc("/v1/issue/token", s.issueToken)
	mux.HandleFunc("/v1/cert", s.getCert)
	return mux
}

// Serve runs the issuer until ctx is cancelled, then drains in-flight requests.
func (s *Server) Serve(ctx context.Context, addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          logging.StdLogger(slog.LevelWarn),
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Warn("issuer shutdown", "err", err)
		}
	}()

	slog.Info("issuer listening",
		"addr", addr,
		"network", s.Issuer.NetworkID)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) getPubKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"network_id": s.Issuer.NetworkID,
		"public_key": auth.PublicKeyBase64(s.Issuer.Key.Public),
	})
}

type issueNodeReq struct {
	NodeID     string `json:"node_id"`
	PubKey     string `json:"pub_key"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	InstanceID string `json:"instance_id"`
	// PreferNear is an optional node id / name whose XOR neighborhood
	// should be used when allocating a new node_id (scale-out relief).
	PreferNear string `json:"prefer_near"`
}

func (s *Server) issueNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req issueNodeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.NodeID == "" {
		var name string
		var err error
		if req.PreferNear != "" {
			if target, decErr := tree.BASE64.Decode(req.PreferNear); decErr == nil {
				// Keep ~16 bits of XOR proximity to the hot requester.
				name, err = tree.BASE64.RandomNameNear(target, 20)
			}
		}
		if name == "" {
			name, err = tree.BASE64.RandomName()
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		req.NodeID = name
	}
	if req.Port == 0 {
		req.Port = s.P2PPort
	}
	if req.IP == "" || req.PubKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ip and pub_key required"})
		return
	}
	cert, err := s.Issuer.SignNodeCert(req.NodeID, req.PubKey, req.IP, req.Port, s.NodeTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.certs[req.NodeID] = cert
	if req.InstanceID != "" {
		s.certs[req.InstanceID] = cert
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"cert": cert})
}

type issueTokenReq struct {
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req issueTokenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.ClientID == "" {
		req.ClientID = "anonymous"
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"read"}
	}
	tok, err := s.Issuer.SignClientToken(req.ClientID, req.Scopes, s.TokenTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	enc, err := auth.EncodeToken(tok)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": enc})
}

func (s *Server) getCert(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		id = r.URL.Query().Get("instance_id")
	}
	s.mu.Lock()
	cert := s.certs[id]
	s.mu.Unlock()
	if cert == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cert not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cert":          cert,
		"bootstrap_ips": s.BootstrapIPs,
		"network_id":    s.Issuer.NetworkID,
	})
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
