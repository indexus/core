package p2p

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type contentPart struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	Part     int    `json:"part"`
	Parts    int    `json:"parts"`
	Data     string `json:"data"`
}

// Project canonical content records to a binary HTTP stream. No new storage
// copy, static-file adapter or whole decoded asset allocation is needed.
func contentEnvelope(rows []indexus.Item, path string) ([]contentPart, int64, error) {
	if len(rows) == 0 || len(rows) > wire.MaxContentParts {
		return nil, 0, errors.New("missing content")
	}
	parts := make([]contentPart, len(rows))
	var encoded int64
	for i, row := range rows {
		if err := json.Unmarshal(row.Payload, &parts[i]); err != nil {
			return nil, 0, err
		}
		p, h := parts[i], parts[0]
		if p.Path != path || p.Part != i || p.Parts != len(rows) || p.Encoding != h.Encoding || p.Bytes != h.Bytes || p.SHA256 != h.SHA256 || p.Bytes < 0 || p.Bytes > wire.MaxContentBytes || len(p.Data)%4 != 0 {
			return nil, 0, errors.New("inconsistent content")
		}
		if p.Encoding != "gzip-base64" && p.Encoding != "identity-base64" {
			return nil, 0, errors.New("unsupported content encoding")
		}
		n := base64.StdEncoding.DecodedLen(len(p.Data))
		if strings.HasSuffix(p.Data, "==") {
			n -= 2
		} else if strings.HasSuffix(p.Data, "=") {
			n--
		}
		encoded += int64(n)
		if encoded > wire.MaxContentBytes {
			return nil, 0, errors.New("content byte budget exceeded")
		}
	}
	hash, err := hex.DecodeString(parts[0].SHA256)
	if err != nil || len(hash) != 32 {
		return nil, 0, errors.New("invalid content checksum")
	}
	if parts[0].Encoding == "identity-base64" && encoded != parts[0].Bytes {
		return nil, 0, errors.New("content length mismatch")
	}
	return parts, encoded, nil
}

func contentReader(parts []contentPart) io.Reader {
	readers := make([]io.Reader, len(parts))
	for i, p := range parts {
		readers[i] = base64.NewDecoder(base64.StdEncoding, strings.NewReader(p.Data))
	}
	return io.MultiReader(readers...)
}

func (h *Handler) GetContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.requireClientScope(w, r, "read") {
		return
	}
	query := r.URL.Query()
	path, location := query.Get("path"), query.Get("location")
	codec := query.Get("encoding")
	if codec == "" {
		codec = "auto"
	}
	if path == "" || len(location) != wire.ContentLocationLength || (codec != "auto" && codec != "gzip" && codec != "identity") {
		writeJSON(w, 400, map[string]string{"error": "invalid content request"})
		return
	}

	started := time.Now()
	rows, err := h.Service.GetRecordsContext(r.Context(), query.Get("collection"), location, wire.MaxContentParts)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}
	lookup := time.Since(started)
	parts, encoded, err := contentEnvelope(rows, path)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	head := parts[0]
	storedGzip := head.Encoding == "gzip-base64"
	wireGzip := codec == "gzip" || (codec == "auto" && storedGzip)
	var input io.Reader = contentReader(parts)
	if storedGzip && !wireGzip {
		z, e := getGzipReader(input)
		if e != nil {
			writeJSON(w, 422, map[string]string{"error": "invalid gzip content"})
			return
		}
		defer putGzipReader(z)
		input = z
	}
	// Explicit identity experiments must not be silently recompressed by a proxy.
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set(wire.HeaderDecodedBytes, strconv.FormatInt(head.Bytes, 10))
	w.Header().Set(wire.HeaderSHA256, head.SHA256)
	w.Header().Set(wire.HeaderStoredEncoding, head.Encoding)
	w.Header().Set("Server-Timing", fmt.Sprintf("content-lookup;dur=%.3f, content-envelope;dur=%.3f", float64(lookup.Microseconds())/1000, float64(time.Since(started).Microseconds()-lookup.Microseconds())/1000))
	if wireGzip {
		w.Header().Set("Content-Encoding", "gzip")
	}
	if storedGzip == wireGzip {
		w.Header().Set("Content-Length", strconv.FormatInt(encoded, 10))
	}
	w.WriteHeader(http.StatusOK)
	var target io.Writer = w
	var zipped *gzip.Writer
	if wireGzip && !storedGzip {
		zipped = fastGzip.get(w)
		defer fastGzip.put(zipped)
		target = zipped
	}
	// Uncompressed output has a strict expansion bound. Forwarded gzip is kept
	// intact and is checked by the browser decoder and SDK SHA-256 verification.
	limit := head.Bytes
	if storedGzip && wireGzip {
		limit = encoded
	}
	n, err := io.Copy(target, io.LimitReader(input, limit+1))
	if err == nil && n != limit {
		err = errors.New("content length mismatch")
	}
	if zipped != nil {
		if e := zipped.Close(); err == nil {
			err = e
		}
	}
	if err != nil {
		panic(http.ErrAbortHandler)
	}
}
