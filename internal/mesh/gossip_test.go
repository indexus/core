package mesh

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// gossipShape builds n distinct vnode IDs. "spike" stacks most of them under
// one hot prefix, the deep, lopsided trie a burst of splits leaves behind.
func gossipShape(n int, shape string, rng *rand.Rand) []GossipMember {
	seen := map[tree.ID]bool{}
	var out []GossipMember
	for len(out) < n {
		var id tree.ID
		for i := range id {
			id[i] = byte(rng.IntN(256))
		}
		if shape == "spike" && len(out) >= n/8 {
			// Splits of one shelf: IDs share a prefix whose length grows.
			depth := 8 + rng.IntN(1+len(out)%40)
			for b := 0; b < depth; b++ {
				id[b/8] &^= 1 << (7 - b%8) // all-zero prefix of that depth
			}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, GossipMember{ID: id, Host: fmt.Sprintf("h%d", len(out))})
		}
	}
	return out
}

// trieDepth is the GOS-1 bound: 1 + the longest prefix any two IDs share.
func trieDepth(members []GossipMember) int {
	depth := 0
	for i := range members {
		for j := i + 1; j < len(members); j++ {
			if d := int(tree.CommonPrefixLen(members[i].ID, members[j].ID)) + 1; d > depth {
				depth = d
			}
		}
	}
	return depth
}

// spread runs synchronous push-pull rounds from one origin. A pair exchanges
// what both held at the start of the round, the conservative reading of GOS-1.
func spread(members []GossipMember, origin int, start uint64) (rounds, messages int) {
	index := map[string]int{}
	for i, m := range members {
		index[m.Host] = i
	}
	has := make([]bool, len(members))
	has[origin] = true
	informed := 1
	for round := start; informed < len(members); round++ {
		rounds++
		before := append([]bool(nil), has...)
		for i, m := range members {
			for _, host := range GossipPeers(m.ID, m.Host, members, nil, round) {
				messages++
				j := index[host]
				if before[i] || before[j] {
					for _, k := range []int{i, j} {
						if !has[k] {
							has[k] = true
							informed++
						}
					}
				}
			}
		}
		if rounds > 4*len(members) {
			return -1, messages
		}
	}
	return rounds, messages
}

// GOS-1: a fact reaches every member within depth(trie) rounds, with about
// N·log2(N) messages per round instead of N·(N−1).
func TestGOS1_BucketGossipCrossesNetworkWithinTrieDepth(t *testing.T) {
	t.Parallel()
	sizes := []int{8, 64, 256, 1024}
	if testing.Short() {
		sizes = sizes[:3]
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, shape := range []string{"random", "spike"} {
		for _, n := range sizes {
			members := gossipShape(n, shape, rng)
			depth := trieDepth(members)
			worst, perRound := 0, 0
			for trial := 0; trial < 6; trial++ {
				rounds, messages := spread(members, rng.IntN(n), rng.Uint64())
				if rounds < 0 || rounds > depth {
					t.Fatalf("%s n=%d: %d rounds, bound %d", shape, n, rounds, depth)
				}
				worst, perRound = max(worst, rounds), messages/max(rounds, 1)
			}
			full := n * (n - 1)
			t.Logf("%-6s n=%4d depth=%2d worst=%2d rounds msgs/round=%6d (full mesh %7d, %.1f%%)", shape, n, depth, worst, perRound, full, 100*float64(perRound)/float64(full))
			if n >= 256 && perRound*10 > full {
				t.Fatalf("%s n=%d: %d messages per round is not bounded", shape, n, perRound)
			}
		}
	}
}

// Rotation reaches every member of every bucket, so a slow or stale peer is
// never starved: within the largest bucket's size, all pairs have talked.
func TestGossipRotationCoversEveryMember(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 4))
	members := gossipShape(200, "spike", rng)
	self := members[0]
	largest := map[uint8]int{}
	for _, m := range members[1:] {
		largest[tree.CommonPrefixLen(self.ID, m.ID)]++
	}
	window := 0
	for _, size := range largest {
		window = max(window, size)
	}
	contacted := map[string]bool{}
	for round := uint64(0); round < uint64(window); round++ {
		for _, host := range GossipPeers(self.ID, self.Host, members, []string{"former-a", "former-b"}, round) {
			contacted[host] = true
		}
	}
	for _, m := range members[1:] {
		if !contacted[m.Host] {
			t.Fatalf("%s never contacted within %d rounds", m.Host, window)
		}
	}
	if !contacted["former-a"] || !contacted["former-b"] {
		t.Fatal("former holders starved")
	}
	if contacted[self.Host] {
		t.Fatal("contacted self")
	}
}

// GOS-1 structure, checked round by round: for every source s and target t,
// s contacts t itself or a member sharing a strictly longer prefix with t.
// Spread timing alone cannot see a missing bucket; detours hide it.
func TestGOS1_EveryRoundContactsCloserToEveryTarget(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(5, 6))
	for _, shape := range []string{"random", "spike"} {
		members := gossipShape(256, shape, rng)
		ids := map[string]tree.ID{}
		for _, m := range members {
			ids[m.Host] = m.ID
		}
		for round := uint64(0); round < 4; round++ {
			for _, s := range members {
				peers := GossipPeers(s.ID, s.Host, members, nil, round)
				for _, target := range members {
					if target.Host == s.Host {
						continue
					}
					need := tree.CommonPrefixLen(s.ID, target.ID)
					ok := false
					for _, host := range peers {
						if host == target.Host || tree.CommonPrefixLen(ids[host], target.ID) > need {
							ok = true
							break
						}
					}
					if !ok {
						t.Fatalf("%s round %d: %s has no partner closer to %s", shape, round, s.Host, target.Host)
					}
				}
			}
		}
	}
}

// Real stores and joins: bucket gossip alone (no all-to-all Sync) converges
// every replica and keeps exactly one writer per zone.
func TestGOS1_RealJoinsConvergeByBucketGossip(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 6)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		record := storage.Record{Collection: fmt.Sprintf("c%015d", i), Location: "aa", ID: "row"}
		if _, err := nodes[0].Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprint(i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range nodes[1:] {
		if err := node.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	digests := func() map[causal.Digest]int {
		out := map[causal.Digest]int{}
		for _, node := range nodes {
			state, err := node.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			replica, err := state.MembershipReplica()
			if err != nil {
				t.Fatal(err)
			}
			out[replica.Digest()]++
		}
		return out
	}
	net.mu.Lock()
	net.hosts = nil
	net.mu.Unlock()
	rounds := 0
	for ; len(digests()) > 1; rounds++ {
		if rounds == 8 {
			t.Fatalf("no convergence after %d gossip rounds: %v", rounds, digests())
		}
		for _, node := range nodes {
			peers, err := node.GossipTargets(uint64(rounds))
			if err != nil {
				t.Fatal(err)
			}
			if len(peers) >= len(nodes) {
				t.Fatalf("%d partners for %d nodes", len(peers), len(nodes))
			}
			if err := node.Sync(ctx, peers); err != nil {
				t.Fatal(err)
			}
			// A stale view may ask for a region a better-informed donor
			// refuses; the refusal carries the donor's knowledge back.
			if err := node.Rebalance(ctx); err != nil && !errors.Is(err, causal.ErrConflict) {
				t.Fatal(err)
			}
		}
	}
	net.mu.Lock()
	calls := len(net.hosts)
	net.mu.Unlock()
	for _, node := range nodes {
		if err := node.Rebalance(ctx); err != nil {
			t.Fatalf("converged %s still conflicts: %v", node.Self.Host, err)
		}
	}
	t.Logf("converged in %d gossip rounds, %d exchanges (all-to-all: %d per round)", rounds, calls, len(nodes)*(len(nodes)-1))
	for key := 0; key < 256; key++ {
		record := storage.Record{Collection: fmt.Sprintf("Probe%011d", key), Location: "aa", ID: "probe"}
		acks := 0
		for _, node := range nodes {
			if _, _, err := node.Store.Apply(fmt.Sprintf("probe-%d", key), record); err == nil {
				acks++
			} else if !errors.Is(err, storage.ErrNotOwner) {
				t.Fatal(err)
			}
		}
		if acks != 1 {
			t.Fatalf("%d writers for zone %d", acks, key)
		}
	}
}
