package meshhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
)

const maxBody = 32 << 20

type failure struct {
	Code    string
	Message string
}

func (f failure) Error() string { return f.Message }
func (f failure) Is(target error) bool {
	if f.Code == "read-changed" {
		return errors.Is(storage.ErrReadChanged, target)
	}
	if f.Code == "handoff-pending" {
		return errors.Is(mesh.ErrHandoffPending, target)
	}
	if f.Code == "receiver-closed" {
		return errors.Is(storage.ErrReceiverClosed, target)
	}
	return code(target) != "unavailable" && f.Code == code(target)
}
func code(err error) string {
	for _, v := range []struct {
		err  error
		code string
	}{
		{storage.ErrReadChanged, "read-changed"}, {storage.ErrReceiverClosed, "receiver-closed"}, {mesh.ErrHandoffPending, "handoff-pending"}, {mesh.ErrNotNeeded, "not-needed"}, {storage.ErrInventoryItemTooLarge, "inventory-item-too-large"},
		{causal.ErrConflict, "causal-conflict"}, {causal.ErrDependency, "dependency"}, {storage.ErrMissingData, "missing-data"}, {storage.ErrWrongCluster, "cluster"}, {storage.ErrConflict, "conflict"}, {storage.ErrNotReady, "not-ready"},
		{storage.ErrWriteOverload, "write-overload"}, {storage.ErrFrozen, "frozen"}, {storage.ErrNotOwner, "not-owner"}, {storage.ErrSequence, "sequence"}, {storage.ErrDataCapacity, "capacity"},
		{storage.ErrRhoDiverged, "diverged"},
	} {
		if errors.Is(err, v.err) {
			return v.code
		}
	}
	return "unavailable"
}

// All default clients share this connection pool. Callers can still supply
// Client.HTTP for another explicit timeout or connection budget.
var sharedHTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: &layered{tcp: sharedTCP},
}

var sharedTCP = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        512,
	MaxIdleConnsPerHost: 128,
	MaxConnsPerHost:     128,
	// Under the servers' IdleTimeout (one minute): a connection the
	// server is closing is not reused for a request.
	IdleConnTimeout:       50 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	ExpectContinueTimeout: time.Second,
}

func NewClient() *Client { return &Client{HTTP: sharedHTTPClient} }

type Client struct {
	HTTP         *http.Client
	Authenticate func(*http.Request)
}

// countingReader counts the bytes a response body yields.
type countingReader struct {
	io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += n
	return n, err
}

// request returns the bytes sent and received, for the wire counters.
func (c *Client) request(ctx context.Context, endpoint, path string, body, out any) (sentBytes, receivedBytes int, err error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, 0, err
	}
	sentBytes = len(raw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return sentBytes, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Every mesh request may be sent twice (writes carry their request id,
	// handoff steps their operation): the transport may then resend one
	// that a reused connection lost before any answer, instead of an EOF.
	// A nil value marks the request without sending a header.
	req.Header["Idempotency-Key"] = nil
	if c.Authenticate != nil {
		c.Authenticate(req)
	}
	client := c.HTTP
	if client == nil {
		client = sharedHTTPClient
	}
	response, err := client.Do(req)
	if err != nil {
		return sentBytes, 0, err
	}
	defer response.Body.Close()
	counted := &countingReader{Reader: io.LimitReader(response.Body, maxBody)}
	defer func() { receivedBytes = counted.n }()
	decoder := json.NewDecoder(counted)
	if response.StatusCode != http.StatusOK {
		var f failure
		if err := decoder.Decode(&f); err != nil {
			return sentBytes, 0, fmt.Errorf("cluster RPC status %d: %w", response.StatusCode, err)
		}
		return sentBytes, 0, f
	}
	return sentBytes, 0, decoder.Decode(out)
}

type meshEnvelope struct {
	Response mesh.Response
	Failure  *failure
}

func (c *Client) Exchange(ctx context.Context, endpoint string, request mesh.Request) (mesh.Response, error) {
	var out meshEnvelope
	started := time.Now()
	sentBytes, receivedBytes, err := c.request(ctx, endpoint, "/cluster/mesh", request, &out)
	if err == nil && out.Failure != nil {
		err = *out.Failure
	}
	membership := request.Kind == mesh.KindMembership
	metrics.Sent(request.Kind).Record(sentBytes, receivedBytes, time.Since(started), err != nil, membership)
	return out.Response, err
}

// countingWriter counts response bytes.
type countingWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += n
	return n, err
}

type Handler struct {
	Mesh      func(context.Context, mesh.Request) (mesh.Response, error)
	Authorize func(*http.Request) error
}

// ServeHTTP admits a cluster RPC (POST, authorized, bounded body) and hands
// it to the handler of its path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeFailure(w, http.StatusMethodNotAllowed, failure{Code: "method", Message: "POST required"})
		return
	}
	if h.Authorize != nil {
		if err := h.Authorize(r); err != nil {
			writeFailure(w, http.StatusUnauthorized, failure{Code: "unauthorized", Message: err.Error()})
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	switch r.URL.Path {
	case "/cluster/mesh":
		h.serveMesh(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeFailure(w http.ResponseWriter, status int, f failure) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(f)
}

// serveMesh decodes one mesh request, runs it and writes its envelope; the
// bytes read and written are recorded under the request's kind when it
// returns. A protocol refusal travels inside a 200 envelope. Both peers
// count it as a failed RPC, independently of the HTTP status.
func (h *Handler) serveMesh(w http.ResponseWriter, r *http.Request) {
	var request mesh.Request
	var err, meshErr error
	in := &countingReader{Reader: r.Body}
	started := time.Now()
	out := &countingWriter{ResponseWriter: w}
	defer func() {
		metrics.Handled(request.Kind).Record(in.n, out.n, time.Since(started), err != nil || meshErr != nil, request.Kind == mesh.KindMembership)
	}()
	if err = json.NewDecoder(in).Decode(&request); err == nil && h.Mesh == nil {
		err = storage.ErrNotReady
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeFailure(out, status, failure{Code: code(err), Message: err.Error()})
		return
	}
	response, meshErr := h.Mesh(r.Context(), request)
	if errors.Is(meshErr, storage.ErrInventoryItemTooLarge) {
		writeFailure(out, http.StatusRequestEntityTooLarge, failure{Code: code(meshErr), Message: meshErr.Error()})
		return
	}
	envelope := meshEnvelope{Response: response}
	if meshErr != nil {
		envelope.Failure = &failure{Code: code(meshErr), Message: meshErr.Error()}
	}
	if request.Page != nil {
		err = writePage(out, envelope)
		return
	}
	err = json.NewEncoder(out).Encode(envelope)
}

// writePage writes the envelope of a paged request, whose body must fit
// maxBody: a larger one is replaced by the inventory-item-too-large failure.
func writePage(w http.ResponseWriter, envelope meshEnvelope) error {
	raw, err := json.Marshal(envelope)
	if err != nil {
		writeFailure(w, http.StatusServiceUnavailable, failure{Code: code(err), Message: err.Error()})
		return err
	}
	if len(raw) > maxBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		err = storage.ErrInventoryItemTooLarge
		raw, _ = json.Marshal(failure{Code: code(err), Message: err.Error()})
	}
	_, writeErr := w.Write(raw)
	return errors.Join(err, writeErr)
}
