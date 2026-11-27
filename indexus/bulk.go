package indexus

import (
	"context"
	"io"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// BulkResult is what a node answers for a staged batch.
type BulkResult = storage.BulkResult

// IngestBulk installs a staged batch, sent as a tar stream (internal/bulk),
// on this node's disk. Then it rebuilds the node's summaries once from the
// disk, so every boundary below a remote parent is queued once and pushed
// with the next flush, and it forgets cached reads of the collection.
func (n *Node) IngestBulk(ctx context.Context, body io.Reader, replace bool) (BulkResult, error) {
	if n.mesh == nil {
		return BulkResult{}, ErrNotReady
	}
	result, err := n.mesh.service.Store.IngestBulkStream(ctx, body, replace)
	if err != nil || result.Already {
		return result, err
	}
	n.mesh.lastWrite.Store(time.Now().UnixNano())
	n.mesh.reads.forget(result.Collection)
	_, err = n.mesh.summary.Fold(ctx, true)
	return result, err
}
