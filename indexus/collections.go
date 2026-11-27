package indexus

import (
	"context"
	"errors"

	"github.com/indexus/go-indexus-core/internal/algebra"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Collection descriptions (docs/protocol/collection-description.md): the
// mathematical object a collection is named by. A collection without one is
// a legacy collection, every column a sum.

var (
	// ErrDescriptionCollision: another description is held under this name.
	ErrDescriptionCollision = algebra.ErrCollision
	// ErrNoDescription: no description is held under this name.
	ErrNoDescription = errors.New("no description for this collection")
)

// InvalidDescription reports an error that says the bytes or the name are
// wrong (not canonical, not this name's hash, an invalid schema or space,
// or a record that does not fit): the client's fault, not a retry.
func InvalidDescription(err error) bool {
	return errors.Is(err, algebra.ErrEncoding) || errors.Is(err, algebra.ErrName) ||
		errors.Is(err, algebra.ErrSchema) || errors.Is(err, storage.ErrDescription)
}

// AdmitCollection admits a description on every disk of the cluster and
// reports how many hold it. Retrying after an error is safe.
func (n *Node) AdmitCollection(ctx context.Context, name string, ixc1 []byte) (int, error) {
	if n.mesh == nil {
		return 0, ErrNotReady
	}
	return n.mesh.service.AdmitEverywhere(ctx, name, ixc1)
}

// CollectionDescription returns the IXC1 bytes held for a collection.
func (n *Node) CollectionDescription(name string) ([]byte, error) {
	if n.mesh == nil {
		return nil, ErrNotReady
	}
	c, err := n.mesh.service.Store.CollectionOf(name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNoDescription
	}
	return c.Bytes, nil
}
