package p2p

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/indexus/go-indexus-core/indexus"
)

// GetLocalChildren observes just this peer; every successful response is partial.
func (h *Handler) GetLocalChildren(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.requireClientScope(w, r, "read") {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 2 || len(q["collection"]) != 1 || len(q["location"]) != 1 || q.Get("collection") == "" || q.Get("location") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one collection and one location"})
		return
	}
	children, err := h.Service.GetLocalChildrenContext(r.Context(), q.Get("collection"), q.Get("location"))
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, indexus.ErrInvalidKey) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"children": children, "complete": false})
}
