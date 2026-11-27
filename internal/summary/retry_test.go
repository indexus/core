package summary

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestFailedFirstEdgeValueRemainsQueuedForRetry(t *testing.T) {
	tr := &Tree{}
	key := tree.Key{Collection: "aaaaaaaaaaaaaaaa", Location: "AB"}
	edge := &Edge{}
	tr.Edges = map[tree.Key]*Edge{key: edge}
	tr.pendingEdges = map[tree.Key]struct{}{key: {}}
	if _, send := tr.storeEdgeValue(key, time.Now(), edgeClaim{stale: true}, nil, errors.New("injected read failure")); send {
		t.Fatal("a failed value was sent")
	}
	tr.Mu.Lock()
	defer tr.Mu.Unlock()
	defer tr.StopFlushLocked()
	_, queued := tr.pendingEdges[key]
	if !queued || !edge.Stale || edge.Pending != nil || edge.failures != 1 || edge.retryAt.IsZero() || tr.flushTimer == nil {
		t.Fatalf("failed first value lost its retry: queued=%v edge=%+v", queued, edge)
	}
}

// An edge waiting for its retry keeps a flush armed for it, even when an
// earlier flush replaced the timer its failure armed: one timer serves every
// edge, and nothing else would wake an edge once writes stop.
func TestAnEdgeWaitingForItsRetryKeepsAFlushArmed(t *testing.T) {
	tr := &Tree{}
	key := tree.Key{Collection: "aaaaaaaaaaaaaaaa", Location: "AB"}
	retry := time.Now().Add(time.Hour)
	tr.Edges = map[tree.Key]*Edge{key: {Pending: &mesh.Contribution{}, retryAt: retry}}
	tr.pendingEdges = map[tree.Key]struct{}{key: {}}
	tr.FlushPendingEdges() // the timer armed at its failure was replaced, then fired
	tr.Mu.Lock()
	defer tr.Mu.Unlock()
	armed := tr.flushTimer != nil && !tr.flushAt.After(retry)
	tr.StopFlushLocked()
	if !armed {
		t.Fatal("no flush armed for the edge's retry: it stays pending for good")
	}
}

// When more than summaryFlushChunk ready edges are queued, FlushPendingEdges
// processes one bounded chunk and immediately re-arms the flush timer for the
// remaining edges so pushMu is not held across tens of thousands of edges.
func TestFlushPendingEdgesReArmsWhenChunked(t *testing.T) {
	tr := &Tree{}
	tr.Edges = make(map[tree.Key]*Edge, summaryFlushChunk+5)
	tr.pendingEdges = make(map[tree.Key]struct{}, summaryFlushChunk+5)
	for i := range summaryFlushChunk + 5 {
		k := tree.Key{Collection: "aaaaaaaaaaaaaaaa", Location: fmt.Sprintf("A%04d", i)}
		// Nil Pending so each edge in the first chunk is dropped without RPC,
		// leaving the overflow 5 edges in pendingEdges.
		tr.Edges[k] = &Edge{}
		tr.pendingEdges[k] = struct{}{}
	}
	tr.FlushPendingEdges()
	tr.Mu.Lock()
	defer tr.Mu.Unlock()
	left := len(tr.pendingEdges)
	armed := tr.flushTimer != nil
	tr.StopFlushLocked()
	if left != 5 {
		t.Fatalf("pendingEdges after 1 chunk = %d, want 5", left)
	}
	if !armed {
		t.Fatal("flush timer not re-armed when ready edges exceeded summaryFlushChunk")
	}
}
