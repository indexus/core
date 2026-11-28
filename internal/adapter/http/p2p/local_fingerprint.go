package p2p

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/indexus/go-indexus-core/indexus"
)

// Fingerprints are optional hints for one disk. A server without the
// observation capability can still serve ordinary traversal requests.
type localFingerprintReader interface {
	ReadLocalFingerprintContext(context.Context, string, string) (indexus.SubtreeFingerprint, error)
}

func (h *Handler) GetLocalFingerprint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.requireClientScope(w, r, "read") {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 2 || len(q["collection"]) != 1 || len(q["location"]) != 1 ||
		q.Get("collection") == "" || q.Get("location") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one collection and one location"})
		return
	}
	reader, ok := h.Service.(localFingerprintReader)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "local fingerprint unavailable"})
		return
	}
	collection, location := q.Get("collection"), q.Get("location")
	observation, err := reader.ReadLocalFingerprintContext(r.Context(), collection, location)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, indexus.ErrInvalidKey) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	var fingerprint, guard *string
	var monoid map[string]string
	if observation.Fingerprint != "" && observation.Guard != "" {
		fingerprint, guard = &observation.Fingerprint, &observation.Guard
		monoid = map[string]string{
			"algorithm": "sha256-add256-v1",
			"sum":       hex.EncodeToString(observation.Sum[:]),
			"count":     strconv.FormatUint(observation.Count, 10),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"collection": collection, "location": location, "fingerprint": fingerprint,
		"guard": guard, "monoid": monoid, "complete": false,
	})
}
