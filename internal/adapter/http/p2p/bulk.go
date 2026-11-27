package p2p

import (
	"errors"
	"net/http"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

// Bulk installs a staged batch on this node: POST /bulk[?replace=1] with the
// batch as a tar stream (manifest.json, rows.bin, payload.bin), authorized
// like a write. The batch is for zones this node holds: there is no
// forwarding, a batch for zones held elsewhere is refused (421).
func (h *Handler) Bulk(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST a batch"})
		return
	}
	replace := r.URL.Query().Get("replace")
	result, err := h.Service.IngestBulk(r.Context(), r.Body, replace == "1" || replace == "true")
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, indexus.ErrBulkInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, indexus.ErrNotOwner), errors.Is(err, indexus.ErrFrozen):
		status = http.StatusMisdirectedRequest
	case errors.Is(err, indexus.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, indexus.ErrDataCapacity):
		status = http.StatusInsufficientStorage
	default:
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
