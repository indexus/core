package tree

import "sync"

// Buckets is one k-bucket per shared-prefix length: slot i holds contacts that
// agree with the target on exactly i leading bits.
const Buckets = Size * 8

// Trie is the peer registry: a mutable binary trie over identifier bits, safe
// for concurrent use. Unlike the View's immutable XOR index it holds any
// payload and changes in place as peers come and go.
type Trie[N any] struct {
	mu   sync.RWMutex
	root *trieNode[N]
}

type trieNode[N any] struct {
	children [2]*trieNode[N]
	node     N
}

func NewTrie[N any]() *Trie[N] {
	return &Trie[N]{root: &trieNode[N]{}}
}

func trieBit(idx int, value []byte) byte { return value[idx/8] >> (7 - idx%8) & 1 }

func (t *trieNode[N]) insert(idx int, id []byte, node N) {
	if idx/8 == len(id) {
		t.node = node
		return
	}
	bit := trieBit(idx, id)
	if t.children[bit] == nil {
		t.children[bit] = &trieNode[N]{}
	}
	t.children[bit].insert(idx+1, id, node)
}

func (t *trieNode[N]) traverse(idx int, value []byte, process func(int, []byte, N)) {
	// Mutate/restore the path buffer in place. Callbacks must not retain it.
	if child := t.children[0]; child != nil {
		child.traverse(idx+1, value, process)
	}
	if child := t.children[1]; child != nil {
		bit := byte(1 << (7 - uint(idx%8)))
		value[idx/8] |= bit
		child.traverse(idx+1, value, process)
		value[idx/8] &^= bit
	}
	if idx > 0 && t.children[0] == nil && t.children[1] == nil {
		process(idx, value, t.node)
	}
}

func (t *trieNode[N]) get(idx int, value []byte) (N, bool) {
	if idx/8 == len(value) {
		return t.node, true
	}
	if child := t.children[trieBit(idx, value)]; child != nil {
		return child.get(idx+1, value)
	}
	return t.node, false
}

func (t *trieNode[N]) nearest(idx int, value []byte) N {
	for idx/8 != len(value) {
		bit := trieBit(idx, value)
		child := t.children[bit]
		if child == nil {
			child = t.children[bit^1]
		}
		if child == nil {
			break
		}
		t = child
		idx++
	}
	return t.node
}

func (t *trieNode[N]) remove(idx int, value []byte) bool {
	if idx/8 == len(value) {
		return true
	}
	bit := trieBit(idx, value)
	if child := t.children[bit]; child != nil && child.remove(idx+1, value) {
		t.children[bit] = nil
		return t.children[bit^1] == nil
	}
	return false
}

// nearestK visits the matching branch first: leaves arrive in XOR distance
// order without sorting or inspecting any ownership rule.
func (t *trieNode[N]) nearestK(idx int, value []byte, k int, out *[]N) {
	if len(*out) >= k {
		return
	}
	if idx/8 == len(value) {
		*out = append(*out, t.node)
		return
	}
	first := trieBit(idx, value)
	for _, side := range [2]byte{first, first ^ 1} {
		if child := t.children[side]; child != nil {
			child.nearestK(idx+1, value, k, out)
		}
	}
}

// An identifier longer than Size bytes would branch past the last bucket;
// such a peer is simply not bucketed beyond it.
func (t *trieNode[N]) extractK(idx int, value []byte, k int, routing *[Buckets][]N) {
	if idx/8 == len(value) || idx == Buckets {
		return
	}
	bit := trieBit(idx, value)
	if child := t.children[bit]; child != nil {
		child.extractK(idx+1, value, k, routing)
	}
	if sibling := t.children[bit^1]; sibling != nil {
		out := make([]N, 0, k)
		sibling.nearestK(idx+1, value, k, &out)
		routing[idx] = out
	}
}

func (t *trieNode[N]) extract(idx int, value []byte, routing *[Buckets]N) {
	if idx/8 == len(value) || idx == Buckets {
		return
	}
	bit := trieBit(idx, value)
	if child := t.children[bit]; child != nil {
		child.extract(idx+1, value, routing)
	}
	if sibling := t.children[bit^1]; sibling != nil {
		routing[idx] = sibling.nearest(idx+1, value)
	}
}

func (t *Trie[N]) Insert(id []byte, node N) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.root.insert(0, id, node)
}

func (t *Trie[N]) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.root = &trieNode[N]{}
}

// Traverse visits every leaf. value is the path buffer, copied first: the walk
// mutates it in place and concurrent callers may share one.
func (t *Trie[N]) Traverse(value []byte, process func(int, []byte, N)) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	path := make([]byte, len(value))
	copy(path, value)
	t.root.traverse(0, path, process)
}

func (t *Trie[N]) Get(value []byte) (N, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.root.get(0, value)
}

func (t *Trie[N]) Remove(value []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.root.remove(0, value)
}

// Extract fills each bucket with its XOR-nearest contact.
func (t *Trie[N]) Extract(value []byte, routing *[Buckets]N) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.root.extract(0, value, routing)
}

// ExtractK fills each bucket with up to k nearest contacts.
func (t *Trie[N]) ExtractK(value []byte, k int, routing *[Buckets][]N) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.root.extractK(0, value, k, routing)
}
