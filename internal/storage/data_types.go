package storage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"

	"github.com/indexus/go-indexus-core/internal/protocol"
)

var (
	ErrRhoDiverged    = protocol.ErrRhoDiverged
	ErrConflict       = errors.New("cluster operation conflicts with committed state")
	ErrNotReady       = errors.New("cluster transaction is not ready")
	ErrReceiverClosed = fmt.Errorf("%w: receiver closed", ErrNotReady)
	ErrWrongCluster   = errors.New("wrong cluster identity")
)

// Topology is the persisted view representation. View rebuilds the indexes and
// verifies its digest; callers cannot smuggle an inconsistent Active/ID map.
type Topology struct {
	Generation uint64
	Digest     [32]byte
	Active     []tree.VNodeDescriptor
}

func encodeView(v *tree.View) Topology {
	return Topology{v.Generation, v.Digest, append([]tree.VNodeDescriptor(nil), v.Active...)}
}
func (t Topology) View() (*tree.View, error) {
	v, err := tree.NewViewFrom(t.Generation, t.Active)
	if err != nil {
		return nil, err
	}
	if v.Len() == 0 || v.Digest != t.Digest {
		return nil, fmt.Errorf("invalid topology digest or empty active set")
	}
	return v, nil
}

// ZoneSeal describes immutable source bytes at its durable freeze watermark.
// Digest covers canonical snapshot plus mirror through Seq, including tombstones.
type ZoneSeal struct {
	Zone   tree.ZoneKey
	Seq    uint64
	Digest [32]byte
	Items  int64
	Bytes  int64
}

type Receipt struct {
	Source string
	Target string
	Seal   ZoneSeal
}

func digest(v any) [32]byte         { b, _ := json.Marshal(v); return sha256.Sum256(b) }
func sealKey(z tree.ZoneKey) string { return z.Collection + "\x00" + z.Location }

// Record is canonical persisted user state. Transfer-control flags are not user
// data. A tombstone remains in the snapshot so a retry cannot resurrect a delete.
type Record struct {
	Collection string
	Location   string
	ID         string
	Metrics    []float64
	Deleted    bool
	// Weight is how many items a pre-aggregated record stands for: a read
	// counts it Weight times. 0 is 1, and is how 1 is stored.
	Weight float64 `json:",omitempty"`
	// Binary says the payload is raw bytes. Payload is then the JSON string
	// of their base64, as encoding/json writes []byte; the store keeps the
	// bytes themselves.
	Binary bool `json:",omitempty"`
	// Keep opaque content last: metric projections read only the canonical header.
	Payload json.RawMessage `json:",omitempty"`
}
type Mutation struct {
	RequestID string
	Seq       uint64
	Record    Record
}
type Snapshot struct {
	Zone     tree.ZoneKey
	Seq      uint64
	Records  []Record
	Requests []RequestReceipt
}

// WeightOf reads a stored weight: 0 is 1.
func WeightOf(weight float64) float64 {
	if weight == 0 {
		return 1
	}
	return weight
}
