package tree

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"sort"
)

// VNodeID is a 96-bit vnode identifier. Host is not part of ownership.
type VNodeID = ID

// VNodeDescriptor is the membership record that participates in XOR.
// State/host live elsewhere; only ID is used by Owner().
type VNodeDescriptor struct {
	ID   VNodeID
	Name string
}

// View is the unique source of XOR ownership at a generation.
// Owner(zone) has no capacity, heat, or host argument.
type View struct {
	Generation uint64
	Active     []VNodeDescriptor
	Digest     [32]byte

	byName     map[string]VNodeID
	byID       map[ID]string
	retiredIDs map[ID]bool
	xorOrder   []VNodeDescriptor // immutable rank order of trie leaves
	xorIndex   []xorNode         // immutable; clones can share it until membership changes
}

func NewView(generation uint64) *View {
	return &View{
		Generation: generation,
		byName:     make(map[string]VNodeID),
		byID:       make(map[ID]string),
		retiredIDs: make(map[ID]bool),
	}
}

// NewViewFrom validates and builds a complete membership in one pass, with a
// single digest/index build. The input slice is detached from the returned view.
func NewViewFrom(generation uint64, active []VNodeDescriptor) (*View, error) {
	v := &View{
		Generation: generation,
		Active:     make([]VNodeDescriptor, 0, len(active)),
		byName:     make(map[string]VNodeID, len(active)),
		byID:       make(map[ID]string, len(active)),
		retiredIDs: make(map[ID]bool),
	}
	for _, d := range active {
		if err := v.addMember(d.Name, d.ID); err != nil {
			return nil, err
		}
	}
	v.finish()
	return v, nil
}

// Clone returns a detached mutable view. Its private XOR index is immutable.
func (v *View) Clone() *View {
	if v == nil {
		return nil
	}
	return v.cloneMeta()
}

func (v *View) finish() {
	active := canonicalActive(v.Active)
	v.Digest = digestCanonical(v.Generation, active)
	v.xorOrder = active
	v.xorIndex = buildXORIndex(active)
}

func (v *View) cloneMeta() *View {
	return &View{
		Generation: v.Generation,
		Active:     append([]VNodeDescriptor(nil), v.Active...),
		Digest:     v.Digest,
		byName:     maps.Clone(v.byName),
		byID:       maps.Clone(v.byID),
		retiredIDs: maps.Clone(v.retiredIDs),
		xorIndex:   v.xorIndex,
		xorOrder:   v.xorOrder,
	}
}

// Add publishes a vnode into V_active. Duplicate name or ID is refused.
func (v *View) Add(name string, id VNodeID) error {
	if err := v.addMember(name, id); err != nil {
		return err
	}
	v.Digest = digestActive(v.Generation, v.Active)
	return nil
}

func (v *View) addMember(name string, id VNodeID) error {
	if v == nil {
		return fmt.Errorf("nil view")
	}
	if v.retiredIDs[id] {
		return fmt.Errorf("retired vnode id cannot be reused")
	}
	if name == "" || id.IsZero() {
		return fmt.Errorf("vnode name and id are required")
	}
	if existing, ok := v.byName[name]; ok {
		if existing == id {
			return nil
		}
		return fmt.Errorf("duplicate vnode name %s", name)
	}
	if other, ok := v.byID[id]; ok {
		return fmt.Errorf("duplicate vnode id already used by %s", other)
	}
	v.byName[name] = id
	v.byID[id] = name
	v.Active = append(v.Active, VNodeDescriptor{ID: id, Name: name})
	v.xorIndex = nil
	v.xorOrder = nil
	return nil
}

// Remove drops a vnode from V_active. Unknown names are ignored.
func (v *View) Remove(name string) {
	if v.removeMember(name) {
		v.Digest = digestActive(v.Generation, v.Active)
	}
}

func (v *View) removeMember(name string) bool {
	if v == nil {
		return false
	}
	id, ok := v.byName[name]
	if !ok {
		return false
	}
	delete(v.byName, name)
	delete(v.byID, id)
	kept := make([]VNodeDescriptor, 0, len(v.Active))
	for _, d := range v.Active {
		if d.Name == name {
			continue
		}
		kept = append(kept, d)
	}
	v.Active = kept
	v.xorIndex = nil
	v.xorOrder = nil
	return true
}

// Owner is owner(z) = argmin XOR among Active. Nothing else. Bulk-built
// views use an immutable compressed trie; small/mutated views scan without
// allocating a temporary ID slice.
func (v *View) Owner(zone ID) VNodeID {
	if v == nil || len(v.Active) == 0 {
		return Zero
	}
	if len(v.xorIndex) != 0 {
		return indexedOwner(v.xorIndex, zone)
	}
	best := v.Active[0].ID
	for _, d := range v.Active[1:] {
		if Closer(zone, d.ID, best) {
			best = d.ID
		}
	}
	return best
}

func (v *View) OwnerName(zone ID) (string, VNodeID) {
	id := v.Owner(zone)
	return v.byID[id], id
}

func (v *View) NameOf(id VNodeID) string {
	if v == nil {
		return ""
	}
	return v.byID[id]
}

func (v *View) IDOf(name string) (VNodeID, bool) {
	if v == nil {
		return Zero, false
	}
	id, ok := v.byName[name]
	return id, ok
}

func (v *View) Len() int {
	if v == nil {
		return 0
	}
	return len(v.Active)
}

func (v *View) HasID(id VNodeID) bool {
	if v == nil {
		return false
	}
	_, ok := v.byID[id]
	return ok
}

func (v *View) HasName(name string) bool {
	if v == nil {
		return false
	}
	_, ok := v.byName[name]
	return ok
}

// AddedSince returns vnodes in v that are not in old (by ID).
func (v *View) AddedSince(old *View) []VNodeDescriptor {
	if v == nil {
		return nil
	}
	var out []VNodeDescriptor
	for _, d := range v.Active {
		if old != nil && old.HasID(d.ID) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func canonicalActive(active []VNodeDescriptor) []VNodeDescriptor {
	out := append([]VNodeDescriptor(nil), active...)
	sort.Slice(out, func(i, j int) bool {
		if cmp := bytes.Compare(out[i].ID[:], out[j].ID[:]); cmp != 0 {
			return cmp < 0
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func digestActive(generation uint64, active []VNodeDescriptor) [32]byte {
	return digestCanonical(generation, canonicalActive(active))
}

func digestCanonical(generation uint64, active []VNodeDescriptor) [32]byte {
	h := sha256.New()
	var gen [8]byte
	for i := 0; i < 8; i++ {
		gen[7-i] = byte(generation >> (8 * i))
	}
	_, _ = h.Write(gen[:])
	for _, d := range active {
		_, _ = h.Write(d.ID[:])
		_, _ = h.Write([]byte(d.Name))
		_, _ = h.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ErrStaleTopology means a delta was built on a different BaseDigest than the view.
var ErrStaleTopology = fmt.Errorf("stale topology")

// Delta is an explicit membership mutation. Apply produces a new View
// without touching zone storage.
type Delta struct {
	BaseDigest [32]byte
	Add        []VNodeDescriptor
	Remove     []string
}

// Apply returns a new view. The old view is not mutated.
func Apply(old *View, delta Delta) (*View, error) {
	if old == nil {
		return nil, fmt.Errorf("nil view")
	}
	if old.Digest != delta.BaseDigest && delta.BaseDigest != [32]byte{} {
		return nil, fmt.Errorf("%w: digest mismatch", ErrStaleTopology)
	}
	next := old.cloneMeta()
	next.Generation = old.Generation + 1
	for _, name := range delta.Remove {
		next.removeMember(name)
	}
	for _, d := range delta.Add {
		if err := next.addMember(d.Name, d.ID); err != nil {
			return nil, err
		}
	}
	next.finish()
	return next, nil
}

// Extend is Apply of an ADD-only delta. old is not mutated.
func Extend(old *View, add ...VNodeDescriptor) (*View, error) {
	if old == nil {
		return nil, fmt.Errorf("nil view")
	}
	return Apply(old, Delta{BaseDigest: old.Digest, Add: add})
}

// ReserveRetired retains a lifetime namespace exclusion without changing XOR
// ownership. Causal membership invokes this only for a validated retirement.
func (v *View) ReserveRetired(id ID) {
	if v.retiredIDs == nil {
		v.retiredIDs = map[ID]bool{}
	}
	v.retiredIDs[id] = true
}
func (v *View) IsRetiredID(id ID) bool { return v != nil && v.retiredIDs[id] }
