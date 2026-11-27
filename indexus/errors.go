package indexus

import (
	"errors"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// ErrPeerBusy is returned when a forward target answers 503 (queue/memory
// full). Callers should fail fast and back off rather than burn the local
// forwardRate budget on a peer that cannot accept work.
var ErrPeerBusy = errors.New("peer ingress full")

// HandoffHeader marks a write on /item as a forward from another node rather
// than a client write, so the receiver can tell load already inside the mesh
// from load arriving at its door.
const HandoffHeader = "X-Indexus-Handoff"

// ErrNotReady rejects data operations until a durable runtime is attached.
var ErrNotReady = storage.ErrNotReady

// ErrReadChanged asks the reader to restart an inventory that changed between pages.
var ErrReadChanged = storage.ErrReadChanged

// ErrReceiverClosed rejects incoming handoffs when this disk is draining or moving.
var ErrReceiverClosed = storage.ErrReceiverClosed

// ErrConflict identifies a request incompatible with committed durable state.
var ErrConflict = storage.ErrConflict

// Refusals of a bulk ingest (Node.IngestBulk), as storage names them.
var (
	ErrNotOwner     = storage.ErrNotOwner
	ErrFrozen       = storage.ErrFrozen
	ErrDataCapacity = storage.ErrDataCapacity
	// ErrTooManyMetrics: a record with more than storage.MaxMetrics metrics.
	ErrTooManyMetrics = storage.ErrTooManyMetrics
	ErrBulkInvalid    = storage.ErrBulkInvalid
)
