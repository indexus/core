package causal

import (
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/indexus/go-indexus-core/internal/tree"
)

type Digest [32]byte

var ErrConflict = errors.New("conflicting local permission or membership")
var ErrDependency = errors.New("required peer state missing")

func Hash(value any) Digest {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err) // Protocol values must have a deterministic JSON encoding.
	}
	return sha256.Sum256(raw)
}

// Genesis establishes the initial installed owner. It is a cluster identity,
// not the start of a proof graph that every later receiver must download.
type Genesis struct {
	Cluster string
	Holder  Endpoint
}

func (g Genesis) Right() Right {
	return Right{ID: Hash(g), Region: tree.Region{}, Owner: g.Holder.Owner}
}

// ZoneTransfer binds one early point to the confirmed reservation of its
// regional cession. Receiving this message alone does not install permission;
// the matching local ticket and certified bytes must also be present.
type ZoneTransfer struct {
	Operation  TransferID
	Ticket     uint64
	Parent     Digest
	Zone       tree.ZoneKey
	From, To   Endpoint
	Seq        uint64
	DataDigest Digest
}

func (z ZoneTransfer) Digest() Digest { return Hash(z) }
