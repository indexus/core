package node

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

// Concurrent first writes into fresh zones, spread over three nodes, must
// leave the root summary of "@" at the exact number of records once the mesh
// is quiet. A lost delta stays lost: no later write repairs it, so only the
// periodic Update (here every 50ms, as the worker would) may close the gap.
func TestSummaryRootConvergesAfterConcurrentMeshWrites(t *testing.T) {
	first := startDataNode(t, "0000000000000001", nil)
	second := startDataNode(t, "F000000000000000", first)
	third := startDataNode(t, "U000000000000000", first)
	nodes := []*runningDataNode{first, second, third}
	for _, node := range nodes {
		if err := node.node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	const (
		rounds    = 20
		locations = 300
		perZone   = 6
		workers   = 64
	)
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	random := rand.New(rand.NewSource(7))
	stale := 0
	for round := range rounds {
		collection := fmt.Sprintf("Converge%08d", round)
		type write struct{ location, id string }
		var writes []write
		seen := map[string]bool{}
		for len(seen) < locations {
			location := string([]byte{alphabet[random.Intn(64)], alphabet[random.Intn(64)], alphabet[random.Intn(64)]})
			if seen[location] {
				continue
			}
			seen[location] = true
			for i := range perZone {
				writes = append(writes, write{location, fmt.Sprintf("r%d", i)})
			}
		}
		// Each zone's records sit next to each other, so its first writes race.
		jobs := make(chan int)
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := range workers {
			wg.Go(func() {
				for i := range jobs {
					// Jitter spreads one zone's writes over several group commits,
					// as HTTP clients and relays do.
					time.Sleep(time.Duration(rand.Intn(2000)) * time.Microsecond)
					item := &indexus.Item{Collection: collection, Location: writes[i].location, Id: writes[i].id, Metrics: []float64{1}, RequestID: fmt.Sprintf("%s-%d", collection, i)}
					if err := nodes[(i+w)%len(nodes)].node.New(item, "@", nil); err != nil {
						errs <- err
						return
					}
				}
			})
		}
		done := make(chan struct{})
		var background sync.WaitGroup
		for _, node := range nodes {
			background.Go(func() {
				for {
					select {
					case <-done:
						return
					default:
					}
					_ = node.node.UpdateContext(t.Context())
					_, _ = node.node.GetAggregates(collection, []string{"@"})
				}
			})
		}
		for i := range writes {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(done)
		background.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		want := len(writes)
		deadline := time.Now().Add(10 * time.Second)
		got := 0
		for {
			for _, node := range nodes {
				if err := node.node.UpdateContext(t.Context()); err != nil {
					t.Logf("update %s: %v", node.node.Name(), err)
				}
			}
			got = ownerAggregates(t, nodes, collection, []string{"@"})["@"].Count()
			if got == want || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if got != want {
			stale++
			t.Errorf("round %d: root summary %d after 10s, want %d", round, got, want)
		}
	}
	t.Logf("%d/%d rounds left a stale root", stale, rounds)
}
