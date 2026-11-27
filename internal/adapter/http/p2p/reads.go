package p2p

import (
	"encoding/base64"
	"fmt"
	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) GetSet(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}
	query := r.URL.Query()
	collection := query.Get("collection")
	location := query.Get("location")
	deep, via, refresh := parseGetFlags(query)

	var contact indexus.Contact
	var set *indexus.Set
	var err error
	options, explicit, policyErr := parseReadOptions(query)
	if policyErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": policyErr.Error()})
		return
	}
	if explicit {
		result, readErr := h.Service.ReadContext(r.Context(), collection, location, options)
		contact, set, err = result.Contact, result.Set, readErr
		if err == nil {
			w.Header().Set("X-Indexus-Read-Source", result.Source)
			if result.FallbackError != "" {
				w.Header().Set("X-Indexus-Read-Fallback", "true")
			}
			w.Header().Set("X-Indexus-Observed-At", result.ObservedAt.UTC().Format(time.RFC3339Nano))
		}
	} else {
		contact, set, err = h.Service.GetSetContext(r.Context(), collection, location, deep, via, refresh)
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var body = struct {
		Contact Contact      `json:"contact"`
		Set     *indexus.Set `json:"set"`
	}{
		Set: set,
	}
	if contact != nil {
		body.Contact = Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
			IP:   contact.IP(),
		}
	}

	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) GetSets(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}

	query := r.URL.Query()
	collection := query.Get("collection")
	if collection == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection parameter is required"})
		return
	}

	locationsParam := query.Get("location")
	if locationsParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "location parameter is required"})
		return
	}

	locations := strings.Split(locationsParam, ",")
	if len(locations) > wire.MaxReadLocations {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read batch exceeds location budget"})
		return
	}
	deep, via, refresh := parseGetFlags(query)
	options, explicit, policyErr := parseReadOptions(query)
	if policyErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": policyErr.Error()})
		return
	}
	// IXB2 (docs/protocol/wire-block.md) is the only /sets encoding; it
	// carries owner redirects in its own section.
	var policy *indexus.ReadOptions
	if explicit {
		policy = &options
		w.Header().Set("X-Indexus-Read-Mode", string(options.Mode))
	}
	width := wire.SetsBlockF32
	if query.Get("metrics") == "f64" {
		width = wire.SetsBlockF64
	}
	var columns []int
	if raw := query.Get("columns"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			c, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "columns: " + err.Error()})
				return
			}
			columns = append(columns, c)
		}
		if err := wire.CheckSetsColumns(columns); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	body, err := h.Service.GetSetsBlockContext(r.Context(), collection, locations, deep, via, refresh, policy, width, columns)
	h.writeSets(w, r, body, err, wire.SetsBlockContentType)
}

// writeSets sends one IXB2 /sets body with the ingress hint.
func (h *Handler) writeSets(w http.ResponseWriter, r *http.Request, body []byte, err error, contentType string) {
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	h.writeIngressHint(w, r)

	w.Header().Set("Content-Type", contentType)
	if err := writeSetsBody(w, r, body); err != nil {
		slog.Warn("sets response truncated", "err", err)
	}
}

// writeIngressHint decodes the client session key and, when a closer peer
// exists, advertises it so sticky browsers can migrate without rediscovery.
func (h *Handler) writeIngressHint(w http.ResponseWriter, r *http.Request) {
	keyRaw := r.Header.Get("X-Indexus-Routing-Key")
	if keyRaw == "" {
		return
	}
	key, ok := decodeRoutingKey(keyRaw)
	if !ok {
		return
	}
	hintSvc, ok := h.Service.(interface {
		RoutingHint([]byte) indexus.Contact
	})
	if !ok {
		return
	}
	hint := hintSvc.RoutingHint(key)
	if hint == nil {
		return
	}
	w.Header().Set("X-Indexus-Ingress-Name", hint.Name())
	w.Header().Set("X-Indexus-Ingress-IP", hint.IP())
	w.Header().Set("X-Indexus-Ingress-Port", strconv.Itoa(hint.Port()))
}

// Routing keys use the SDK URL64 alphabet, with optional base64 padding.
func decodeRoutingKey(raw string) ([]byte, bool) {
	raw = strings.TrimSpace(raw)
	raw += strings.Repeat("=", (4-len(raw)%4)%4)
	key, err := base64.URLEncoding.DecodeString(raw)
	return key, err == nil && len(key) > 0
}

func parseGetFlags(query url.Values) (deep bool, via indexus.Visited, refresh bool) {
	deep = true
	if raw := query.Get("deep"); raw != "" {
		switch strings.ToLower(raw) {
		case "0", "false", "no":
			deep = false
		default:
			deep = true
		}
	}
	via = indexus.ParseVisited(query.Get("via"))
	if raw := query.Get("refresh"); raw != "" {
		switch strings.ToLower(raw) {
		case "1", "true", "yes":
			refresh = true
		}
	}
	return deep, via, refresh
}

func (h *Handler) GetAggregates(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}
	query := r.URL.Query()
	collection := query.Get("collection")
	locParam := query.Get("location")
	if collection == "" || locParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection and location are required"})
		return
	}
	aggSvc, ok := h.Service.(interface {
		GetAggregates(string, []string) (map[string]*indexus.Abelian, error)
	})
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "aggregates not supported"})
		return
	}
	locations := strings.Split(locParam, ",")
	aggs, err := aggSvc.GetAggregates(collection, locations)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aggregates": aggs})
}

func parseReadOptions(query url.Values) (indexus.ReadOptions, bool, error) {
	mode := query.Get("read_mode")
	if mode == "" {
		return indexus.ReadOptions{}, false, nil
	}
	options := indexus.ReadOptions{Mode: indexus.ReadMode(mode)}
	switch options.Mode {
	case indexus.ReadFresh:
	case indexus.ReadCached, indexus.ReadStaleIfError:
		age, err := time.ParseDuration(query.Get("max_age"))
		if err != nil || age <= 0 {
			return options, true, fmt.Errorf("cache read requires positive max_age duration")
		}
		options.MaxAge = age
	default:
		return options, true, fmt.Errorf("unknown read_mode %q", mode)
	}
	return options, true, nil
}
