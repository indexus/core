package indexus

import (
	"context"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// SubtreeFingerprint describes one disk and one storage snapshot.
type SubtreeFingerprint = storage.SubtreeFingerprint

// ReadLocalFingerprintContext observes the entire prefix on this disk,
// including its direct records. A split prefix can span multiple peers.
// An empty token means this peer cannot currently supply a witness.
func (n *Node) ReadLocalFingerprintContext(ctx context.Context, collection, location string) (SubtreeFingerprint, error) {
	if err := checkKey(&Item{Collection: collection, Location: location}, BASE64.Root()); err != nil {
		return SubtreeFingerprint{}, err
	}
	if err := ctx.Err(); err != nil {
		return SubtreeFingerprint{}, err
	}
	if n.mesh == nil {
		return SubtreeFingerprint{}, ErrNotReady
	}
	ctx, done := n.mesh.clientActivity.Begin(ctx)
	defer done()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case n.mesh.readSlots <- struct{}{}:
	case <-ctx.Done():
		return SubtreeFingerprint{}, ctx.Err()
	}
	defer func() { <-n.mesh.readSlots }()
	return n.mesh.service.Store.LocalSubtreeFingerprint(ctx, collection, location)
}
