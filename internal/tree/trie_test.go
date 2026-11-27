package tree

import (
	"bytes"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

// Kademlia properties of the peer registry trie (docs/protocol.md §P/§R).
//
// Gossip and routing rest on two trie operations:
//   - nearest is the true XOR minimum, so every bucket names the closest peer
//     of its distance class.
//   - Extract fills k-bucket slots — routing[i] is the best contact sharing
//     exactly i prefix bits, so gossip reaches every distance class.
//
// These tests check the trie against brute-force XOR arithmetic on random
// datasets, seeds fixed for reproducibility.

const kadIDLen = 8 // bytes; 64 bits < the 96-slot routing array

func kadIDs(seed int64, count int) [][]byte {
	rnd := rand.New(rand.NewSource(seed))
	seen := make(map[string]struct{}, count)
	ids := make([][]byte, 0, count)
	for len(ids) < count {
		id := make([]byte, kadIDLen)
		rnd.Read(id)
		if _, dup := seen[string(id)]; dup {
			continue
		}
		seen[string(id)] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func xorOf(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// prefixBits is the index of the first differing bit (== shared prefix
// length); len*8 when equal.
func prefixBits(a, b []byte) int {
	for i := 0; i < len(a); i++ {
		if x := a[i] ^ b[i]; x != 0 {
			n := i * 8
			for mask := byte(0x80); mask != 0 && x&mask == 0; mask >>= 1 {
				n++
			}
			return n
		}
	}
	return len(a) * 8
}

// nearest must return the XOR minimum over the whole trie: it names the
// contact Extract puts in each bucket. Distinct ids never tie
// (XOR against a fixed target is a bijection), so the answer is unique.
func TestNearestIsXorMinimum(t *testing.T) {
	ids := kadIDs(1, 256)
	bst := NewTrie[[]byte]()
	for _, id := range ids {
		bst.Insert(id, id)
	}

	targets := kadIDs(2, 64)
	targets = append(targets, ids[:32]...) // members: distance 0 must win

	for _, target := range targets {
		want := ids[0]
		for _, id := range ids[1:] {
			if bytes.Compare(xorOf(id, target), xorOf(want, target)) < 0 {
				want = id
			}
		}
		got := bst.root.nearest(0, target)
		if !bytes.Equal(got, want) {
			t.Fatalf("nearest(%x) = %x, brute-force XOR minimum is %x", target, got, want)
		}
	}
}

// Extract must fill routing[i] with the XOR-nearest id sharing exactly i
// prefix bits with the target — one best contact per k-bucket — and leave
// the slot empty iff that distance class has no member.
func TestExtractFillsEachBucketWithItsNearest(t *testing.T) {
	ids := kadIDs(3, 256)
	bst := NewTrie[[]byte]()
	for _, id := range ids {
		bst.Insert(id, id)
	}

	for _, target := range kadIDs(4, 16) {
		var routing [Buckets][]byte
		bst.Extract(target, &routing)

		buckets := make(map[int][][]byte)
		for _, id := range ids {
			p := prefixBits(id, target)
			buckets[p] = append(buckets[p], id)
		}

		for i := 0; i < kadIDLen*8; i++ {
			members := buckets[i]
			if routing[i] == nil {
				if len(members) > 0 {
					t.Fatalf("bucket %d has %d members but Extract left it empty", i, len(members))
				}
				continue
			}
			if got := prefixBits(routing[i], target); got != i {
				t.Fatalf("routing[%d] shares %d prefix bits with the target, want exactly %d", i, got, i)
			}
			best := members[0]
			for _, id := range members[1:] {
				if bytes.Compare(xorOf(id, target), xorOf(best, target)) < 0 {
					best = id
				}
			}
			if !bytes.Equal(routing[i], best) {
				t.Fatalf("routing[%d] = %x, bucket's XOR-nearest is %x", i, routing[i], best)
			}
		}
	}
}

// ExtractK is what a peer gossips back. Each bucket must carry its k nearest
// members in distance order, not an arbitrary k: a node learning the mesh from
// these answers gets the near half of every band it cannot see, and a lookup
// reading only the head of each bucket still gets the same contact Extract
// would have given it.
func TestExtractKFillsEachBucketWithItsKNearest(t *testing.T) {
	ids := kadIDs(7, 256)
	bst := NewTrie[[]byte]()
	for _, id := range ids {
		bst.Insert(id, id)
	}

	for _, k := range []int{1, 2, 8, 20} {
		for _, target := range kadIDs(9, 8) {
			var routing [Buckets][][]byte
			bst.ExtractK(target, k, &routing)

			buckets := make(map[int][][]byte)
			for _, id := range ids {
				p := prefixBits(id, target)
				buckets[p] = append(buckets[p], id)
			}

			for i := 0; i < kadIDLen*8; i++ {
				members := buckets[i]
				got := routing[i]

				want := len(members)
				if want > k {
					want = k
				}
				if len(got) != want {
					t.Fatalf("k=%d bucket %d holds %d contacts, want %d (bucket has %d members)",
						k, i, len(got), want, len(members))
				}

				sort.Slice(members, func(a, b int) bool {
					return bytes.Compare(xorOf(members[a], target), xorOf(members[b], target)) < 0
				})
				for rank, id := range got {
					if prefixBits(id, target) != i {
						t.Fatalf("k=%d routing[%d][%d] shares %d prefix bits, want exactly %d",
							k, i, rank, prefixBits(id, target), i)
					}
					if !bytes.Equal(id, members[rank]) {
						t.Fatalf("k=%d routing[%d][%d] = %x, the %d-th nearest is %x",
							k, i, rank, id, rank, members[rank])
					}
				}
			}
		}
	}
}

// The head of each widened bucket has to be exactly what the narrow Extract
// returns, or a lookup reading ExtractK would route differently from one
// reading Extract.
func TestExtractKAgreesWithExtractOnTheFirstContact(t *testing.T) {
	ids := kadIDs(11, 512)
	bst := NewTrie[[]byte]()
	for _, id := range ids {
		bst.Insert(id, id)
	}

	for _, target := range kadIDs(13, 16) {
		var narrow [Buckets][]byte
		bst.Extract(target, &narrow)

		var wide [Buckets][][]byte
		bst.ExtractK(target, 20, &wide)

		for i := range narrow {
			if narrow[i] == nil {
				if len(wide[i]) != 0 {
					t.Fatalf("bucket %d is empty for Extract and holds %d for ExtractK", i, len(wide[i]))
				}
				continue
			}
			if len(wide[i]) == 0 || !bytes.Equal(wide[i][0], narrow[i]) {
				t.Fatalf("bucket %d head disagrees: Extract %x, ExtractK %v", i, narrow[i], wide[i])
			}
		}
	}
}

func TestTrieConcurrentAccess(t *testing.T) {
	const (
		writers = 8
		readers = 8
		ops     = 200
	)

	bst := NewTrie[int]()

	keys := make([][]byte, writers*ops)
	for i := range keys {
		keys[i] = []byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}
	}

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				bst.Insert(keys[w*ops+i], w*ops+i)
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := make([]byte, 4)
			var routing [Buckets]int
			for i := 0; i < ops; i++ {
				_, _ = bst.Get(keys[i])
				bst.Extract(keys[i], &routing)
				bst.Traverse(id, func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}

// Reset must be observed atomically: readers may see a full or an empty tree,
// never a half-replaced one.
func TestTrieConcurrentReset(t *testing.T) {
	const ops = 500

	bst := NewTrie[int]()
	id := make([]byte, 4)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			bst.Insert([]byte{byte(i), 0, 0, 0}, i)
			bst.Reset()
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var routing [Buckets]int
			for i := 0; i < ops; i++ {
				bst.Extract(id, &routing)
				bst.Traverse(id, func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}
