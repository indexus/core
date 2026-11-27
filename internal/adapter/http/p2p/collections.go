package p2p

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

// maxDescription bounds a PUT body: 255 axes, a full schedule and 4,096
// columns in separate runs stay under 20 KiB.
const maxDescription = 64 << 10

type collectionService interface {
	AdmitCollection(context.Context, string, []byte) (int, error)
	CollectionDescription(string) ([]byte, error)
}

// Collection serves /collection/<name>: GET returns the description's IXC1
// bytes, which the client verifies itself (the name is their hash); PUT
// admits them on every disk of the cluster.
func (h *Handler) Collection(w http.ResponseWriter, r *http.Request) {
	service, ok := h.Service.(collectionService)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/collection/")
	if len(name) != 16 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a collection name is 16 characters"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !h.requireClientScope(w, r, "read") {
			return
		}
		b, err := service.CollectionDescription(name)
		switch {
		case errors.Is(err, indexus.ErrNoDescription):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			w.Write(b)
		}
	case http.MethodPut:
		if !h.requireClientScope(w, r, "write") {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxDescription+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if len(body) > maxDescription {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "description too large"})
			return
		}
		held, err := service.AdmitCollection(r.Context(), name, body)
		switch {
		case indexus.InvalidDescription(err):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, indexus.ErrDescriptionCollision):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "hosts": held})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"name": name, "hosts": held})
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
