package indexus

import "github.com/indexus/go-indexus-core/internal/wire"

// The data of the node API. Their wire formats (IXB2, IXRB) are in
// internal/wire.
type (
	Item    = wire.Item
	Set     = wire.Set
	Abelian = wire.Abelian
)
