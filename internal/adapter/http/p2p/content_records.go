package p2p

import (
	"context"
	"encoding/json"
	"errors"
	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

type degradedReader interface {
	DegradedRead(ctx context.Context, collection, location, id string) (*indexus.Item, bool, error)
}

// GetDegradedItem serves a non-authoritative degraded point read from a
// replica's fallback log overlaid on its shadow copy (READ-1 degraded path).
func (h *Handler) GetDegradedItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.requireClientScope(w, r, "read") {
		return
	}
	reader, ok := h.Service.(degradedReader)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "degraded read unavailable"})
		return
	}
	q := r.URL.Query()
	collection, location, id := q.Get("collection"), q.Get("location"), q.Get("id")
	if collection == "" || location == "" || id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection, location, and id are required"})
		return
	}
	item, found, err := reader.DegradedRead(r.Context(), collection, location, id)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("X-Indexus-Degraded", "true")
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"degraded": true, "item": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"degraded": true, "item": item})
}

func (h *Handler) GetRecords(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.requireClientScope(w, r, "read") {
		return
	}
	query := r.URL.Query()
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit < 1 || limit > wire.MaxContentRecords {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid record budget"})
		return
	}

	rows, err := h.Service.GetRecordsContext(r.Context(), query.Get("collection"), query.Get("location"), limit)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, indexus.ErrQueryBudget) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	// The answer streams, gzipped (fastest level: payloads are most of it)
	// when the client accepts it.
	w.Header().Add("Vary", "Accept-Encoding")
	var out io.Writer = w
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		zw := fastGzip.get(w)
		defer func() {
			if err := zw.Close(); err != nil {
				slog.Warn("records response truncated", "err", err)
			}
			fastGzip.put(zw)
		}()
		w.Header().Set("Content-Encoding", "gzip")
		out = zw
	}
	if query.Get("format") == "binary" {
		w.Header().Set("Content-Type", wire.RecordsContentType)
		if err := wire.EncodeRecords(out, rows); err != nil {
			slog.Warn("records response truncated", "err", err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(out).Encode(map[string]any{"records": rows}); err != nil {
		slog.Warn("records response truncated", "err", err)
	}
}

// GetDirectRecords serves local rows only. The client decides where to go
// after an ownership refusal; the peer never resolves the read over the mesh.
func (h *Handler) GetDirectRecords(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.requireClientScope(w, r, "read") {
		return
	}
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 || limit > wire.MaxContentRecords || q.Get("collection") == "" || q.Get("location") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection, location and valid record limit are required"})
		return
	}
	includePayloads := false
	if values, ok := q["payloads"]; ok {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payloads flag"})
			return
		}
		includePayloads = values[0] == "true"
	}
	rows, err := h.Service.ReadDirectRecordsContext(r.Context(), q.Get("collection"), q.Get("location"), limit, includePayloads)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, indexus.ErrNotOwner):
			status = http.StatusConflict
		case errors.Is(err, indexus.ErrInvalidKey):
			status = http.StatusBadRequest
		case errors.Is(err, indexus.ErrQueryBudget):
			status = http.StatusRequestEntityTooLarge
		}
		reply := map[string]any{"error": err.Error()}
		if errors.Is(err, indexus.ErrNotOwner) || errors.Is(err, indexus.ErrFrozen) {
			if router, ok := h.Service.(interface {
				DirectRecordContact(string, string) indexus.Contact
			}); ok {
				if c := router.DirectRecordContact(q.Get("collection"), q.Get("location")); c != nil {
					reply["contact"] = Contact{Name: c.Name(), IP: c.IP(), IPs: c.IPs(), Port: c.Port()}
				}
			}
		}
		writeJSON(w, status, reply)
		return
	}
	reply := map[string]any{"records": rows}
	if includePayloads {
		reply["payloads"] = true
	}
	writeJSON(w, http.StatusOK, reply)
}
