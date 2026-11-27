package mockup

import (
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type durableNetwork struct {
	mu         sync.Mutex
	causalRoot *mesh.Service
	ready      map[string]bool
	after      func(string)
}

var durableMeshes sync.Map

func enableDurableSimulation(t *testing.T, network *Network, node *indexus.Node, join bool) {
	t.Helper()
	enableCausalSimulation(t, network, node, join)
}

var requestSequence atomic.Uint64

func mustInsert(t *testing.T, node *indexus.Node, item *indexus.Item) {
	t.Helper()
	item.RequestID = fmt.Sprintf("simulation-write-%d", requestSequence.Add(1))
	deadline := time.Now().Add(45 * time.Second)
	for {
		err := node.New(item, "@", nil)
		if err == nil {
			return
		}
		if !errors.Is(err, storage.ErrFrozen) || time.Now().After(deadline) {
			t.Fatalf("seed was not accepted: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Random vnode IDs do not imply a minimum number of populated nodes. Compare
// every node's count with the independent XOR oracle for every seeded key.
func assertSeedPlacement(t *testing.T, nodes []*indexus.Node, collections []string, perColl int, twoChars bool) {
	t.Helper()
	counts := map[string]int{}
	chars := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	for ci, collection := range collections {
		for j := 0; j < perColl; j++ {
			location := string(chars[j%len(chars)])
			if twoChars {
				idx := ci*perColl + j
				location = string(chars[idx%len(chars)]) + string(chars[(idx/len(chars))%len(chars)])
			}
			id, err := indexus.MergeEncodings(indexus.BASE64, indexus.BASE64, location, collection)
			if err != nil {
				t.Fatal(err)
			}
			counts[closestNode(id, nodes).Name()]++
		}
	}
	for _, node := range nodes {
		got, err := node.Count()
		if err != nil {
			t.Fatal(err)
		}
		if got != counts[node.Name()] {
			t.Errorf("node %s holds %d items; XOR oracle requires %d", node.Name(), got, counts[node.Name()])
		}
	}
}
