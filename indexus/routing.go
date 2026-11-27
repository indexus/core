package indexus

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"math/rand/v2"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func (n *Node) topologyView() *tree.View {
	if n == nil || n.mesh == nil {
		return tree.NewView(0)
	}
	return n.mesh.summary.View()
}

// JoinHint suggests a name for a node about to join: one that halves one of
// the largest XOR cells of the active set (JOIN-1). It is advisory and public;
// the joiner may ignore it. Hints given lately count as joined until gossip
// shows their joiners (REM-120).
func (n *Node) JoinHint() (string, error) {
	view := n.topologyView()
	if view == nil || len(view.Active) == 0 {
		return "", ErrNotReady
	}
	var seed [16]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		return "", err
	}
	rng := rand.New(rand.NewPCG(binary.BigEndian.Uint64(seed[:8]), binary.BigEndian.Uint64(seed[8:])))
	return tree.EncodeName(n.joinHints.Next(view.Active, time.Now(), joinHintMemory, rng)), nil
}

// joinHintMemory is how long a given hint counts as a joined vnode: a few
// gossip rounds, the time a join takes to reach this node's view.
const joinHintMemory = 2 * time.Minute
