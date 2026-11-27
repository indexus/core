package tree

import (
	"cmp"
	"encoding/hex"
	"fmt"
)

// Size is the 96-bit identifier width (docs/protocol/02-id-xor-model.md).
const Size = 12

// Width is the length, in BASE64 characters, of every 96-bit name: a vnode
// name, a collection name, and the deepest location. 16 characters of 6 bits
// fill the 12 bytes of an ID exactly, so a zone key is location bits followed
// by collection bits with nothing left over or missing.
const Width = Size * 8 / 6

// CheckName reports whether s is a full-width name of the alphabet: a vnode
// name or a collection name.
func CheckName(s string) error {
	if len(s) != Width {
		return fmt.Errorf("name %q: %d characters, want %d", s, len(s), Width)
	}
	return checkChars(s)
}

// CheckLocation reports whether s is a location: the root, or 1 to Width
// characters of the alphabet.
func CheckLocation(s string) error {
	if s == BASE64.Root() {
		return nil
	}
	if len(s) == 0 || len(s) > Width {
		return fmt.Errorf("location %q: %d characters, want 1 to %d", s, len(s), Width)
	}
	return checkChars(s)
}

func checkChars(s string) error {
	for i := range len(s) {
		if BASE64.IndexOf(rune(s[i])) < 0 {
			return fmt.Errorf("%q: %q is not in the alphabet", s, s[i])
		}
	}
	return nil
}

// ID is a 96-bit point in the XOR space. It is not a hash of an item.
type ID [Size]byte

// Zero is the all-zero ID.
var Zero ID

func (id ID) Bytes() []byte {
	out := make([]byte, Size)
	copy(out, id[:])
	return out
}

func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

func (id ID) IsZero() bool {
	return id == Zero
}

// FromSlice copies the first Size bytes. Shorter input is zero-padded.
func FromSlice(b []byte) ID {
	var id ID
	copy(id[:], b)
	return id
}

// Key names a zone: a location range inside a collection. It is the unit every
// phase agrees on — what a node owns, transfers, snapshots and delegates.
type Key struct {
	Collection string
	Location   string
}

// Compare orders zone identities lexically by collection, then location.
// This is not XOR order or the storage engine's routing-alphabet order.
func (k Key) Compare(other Key) int {
	if c := cmp.Compare(k.Collection, other.Collection); c != 0 {
		return c
	}
	return cmp.Compare(k.Location, other.Location)
}

// XORCompare compares key⊕a vs key⊕b as big-endian integers without
// allocating the two distance slices.
//
//	<0 : a is strictly closer to key
//	>0 : b is strictly closer to key
//	 0 : equal distance
func XORCompare(key, a, b ID) int {
	for i := 0; i < Size; i++ {
		da := key[i] ^ a[i]
		db := key[i] ^ b[i]
		if da < db {
			return -1
		}
		if da > db {
			return 1
		}
	}
	return 0
}

// Closer reports whether candidate is strictly XOR-closer to key than incumbent.
func Closer(key, candidate, incumbent ID) bool {
	return XORCompare(key, candidate, incumbent) < 0
}

// CommonPrefixLen is the number of leading equal bits of a and b.
func CommonPrefixLen(a, b ID) uint8 {
	var shared uint8
	for i := 0; i < Size; i++ {
		diff := a[i] ^ b[i]
		if diff == 0 {
			shared += 8
			continue
		}
		for bit := 7; bit >= 0; bit-- {
			if diff>>uint(bit)&1 == 0 {
				shared++
				continue
			}
			return shared
		}
	}
	return shared
}

// Bit returns bit (0 = MSB of byte 0) of id.
func Bit(id ID, bit uint8) uint8 {
	if int(bit) >= Size*8 {
		return 0
	}
	by := bit / 8
	mask := uint8(1 << (7 - bit%8))
	if id[by]&mask != 0 {
		return 1
	}
	return 0
}

// SetBit returns a copy of id with bit set to side (0 or 1).
func SetBit(id ID, bit uint8, side uint8) ID {
	out := id
	if int(bit) >= Size*8 {
		return out
	}
	by := bit / 8
	mask := byte(1 << (7 - bit%8))
	if side&1 == 1 {
		out[by] |= mask
	} else {
		out[by] &^= mask
	}
	return out
}

// HotSplitAddress returns the canonical donor-only hot-split address:
// donor with exactly forkBit flipped and every other bit unchanged.
func HotSplitAddress(donor ID, forkBit uint8) (ID, error) {
	if int(forkBit) >= Size*8 {
		return Zero, fmt.Errorf("fork bit %d out of range", forkBit)
	}
	out := SetBit(donor, forkBit, 1-Bit(donor, forkBit))
	if out.IsZero() {
		return Zero, fmt.Errorf("hot split address is the reserved zero ID")
	}
	return out, nil
}

// Prefix is a closed XOR cell p* of Bits leading bits of ID.
// It is a control region, not an ownership range.
type Prefix struct {
	ID   ID
	Bits uint8
}

func (p Prefix) Contains(id ID) bool {
	if p.Bits == 0 {
		return true
	}
	if int(p.Bits) > Size*8 {
		return p.ID == id
	}
	return CommonPrefixLen(p.ID, id) >= p.Bits
}

// GuardBit is g(D)=1+max_{U≠D} LCP(D,U). Forks at b≥g(D) are donor-only.
// With no other vnode, g(D)=0 (any fork is vacuously donor-only).
func GuardBit(donor ID, others []ID) uint8 {
	var max uint8
	found := false
	for _, u := range others {
		if u == donor {
			continue
		}
		l := CommonPrefixLen(donor, u)
		if !found || l > max {
			max = l
			found = true
		}
	}
	if !found {
		return 0
	}
	// max LCP of 95 ⇒ g(D)=96 ⇒ no legal fork bit (ForkID requires bit < 96).
	if int(max)+1 > Size*8 {
		return uint8(Size * 8)
	}
	return max + 1
}

// FirstDiffFrom is the first bit ≥ from where a and b differ.
func FirstDiffFrom(a, b ID, from uint8) (uint8, bool) {
	total := uint8(Size * 8)
	for i := from; i < total; i++ {
		if Bit(a, i) != Bit(b, i) {
			return i, true
		}
	}
	return 0, false
}

// SingleDiffBit reports the sole bit where a and b differ. Hot split
// addresses are canonical only when this succeeds.
func SingleDiffBit(a, b ID) (uint8, bool) {
	var found uint8
	differences := 0
	for bit := uint8(0); bit < Size*8; bit++ {
		if Bit(a, bit) == Bit(b, bit) {
			continue
		}
		found = bit
		differences++
		if differences > 1 {
			return 0, false
		}
	}
	return found, differences == 1
}

// ForkID builds a vnode ID that matches donor until bit, takes side, then
// follows suffix. Safe donor-only ADD requires bit ≥ GuardBit(donor, others).
func ForkID(donor ID, bit uint8, side uint8, suffix ID) (ID, error) {
	total := uint8(Size * 8)
	if bit >= total {
		return Zero, fmt.Errorf("fork bit %d out of range", bit)
	}
	out := donor
	out = SetBit(out, bit, side)
	for b := bit + 1; b < total; b++ {
		out = SetBit(out, b, Bit(suffix, b))
	}
	return out, nil
}

// DecodeName maps a 16-character BASE64 vnode name to an ID.
func DecodeName(name string) (ID, error) {
	raw, err := DecodeVnodeID(name)
	if err != nil {
		return Zero, err
	}
	return FromSlice(raw), nil
}

// EncodeName maps an ID to a BASE64 vnode name.
func EncodeName(id ID) string {
	return BASE64.Encode(id.Bytes())
}

// ZoneKeyID is id(z) = bits(location) || suffix(collection).
func ZoneKeyID(collection, location string) ([]byte, error) {
	if location == "" {
		location = BASE64.Root()
	}
	if err := CheckName(collection); err != nil {
		return nil, fmt.Errorf("zone key: collection %w", err)
	}
	if err := CheckLocation(location); err != nil {
		return nil, fmt.Errorf("zone key: %w", err)
	}
	return MergeEncodings(BASE64, BASE64, location, collection)
}

// DecodeVnodeID maps a 16-character BASE64 name to 96 bits.
func DecodeVnodeID(name string) ([]byte, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	return BASE64.Decode(name)
}

// ZoneKey is (collection, location) plus its 96-bit id
// id(z) = location bits || collection suffix.
type ZoneKey struct {
	Collection string
	Location   string
	ID         ID
}

// Compare preserves the raw lexical identity order, independently of ID.
func (z ZoneKey) Compare(other ZoneKey) int {
	return (Key{z.Collection, z.Location}).Compare(Key{other.Collection, other.Location})
}

// Key is the zone's identity. ID is only its XOR address: an ancestor and
// descendant (or another collection) may have the same address.
func (z ZoneKey) Key() Key {
	location := z.Location
	if location == "" {
		location = BASE64.Root()
	}
	return Key{Collection: z.Collection, Location: location}
}

// ZoneID is id(z) = bits(location) || suffix(collection).
func ZoneID(collection, location string) (ID, error) {
	raw, err := ZoneKeyID(collection, location)
	if err != nil {
		return Zero, err
	}
	if len(raw) == 0 {
		return Zero, fmt.Errorf("empty zone id")
	}
	return FromSlice(raw), nil
}

// NewZoneKey builds a ZoneKey with its XOR id filled in.
func NewZoneKey(collection, location string) (ZoneKey, error) {
	id, err := ZoneID(collection, location)
	if err != nil {
		return ZoneKey{}, err
	}
	if location == "" {
		location = BASE64.Root()
	}
	return ZoneKey{Collection: collection, Location: location, ID: id}, nil
}

// IsAncestorOf reports whether z is a strict GPS ancestor of other
// in the same collection.
func (z ZoneKey) IsAncestorOf(other ZoneKey) bool {
	return Covers(z.Collection, z.Location, other.Collection, other.Location)
}

// Covers reports whether parent location covers child in the GPS trie.
func Covers(parentCol, parentLoc, childCol, childLoc string) bool {
	if parentCol != childCol || parentLoc == childLoc {
		return false
	}
	root := BASE64.Root()
	if parentLoc == root || parentLoc == "" {
		return true
	}
	if childLoc == root || childLoc == "" {
		return false
	}
	return len(childLoc) > len(parentLoc) && childLoc[:len(parentLoc)] == parentLoc
}

// Encoder interface defines the methods that any encoder should implement.
type Encoder interface {
	Packing() int
	Encode(data []byte) string
	Decode(encoded string) ([]byte, error)
	Root() string
	Length() int
	CharAt(idx int) string
	Parent(hash string) string
	NewID() []byte
	RandomName() (string, error)
}

// ChildCandidates returns the direct child location ids under parent
// (one alphabet extension each). Used for local XOR targeting only —
// not as network probes.
func ChildCandidates(base Encoder, parent string) []string {
	if base == nil {
		return nil
	}
	n := base.Length()
	out := make([]string, 0, n)
	root := base.Root()
	for i := 0; i < n; i++ {
		ch := base.CharAt(i)
		if ch == "" {
			continue
		}
		if parent == root {
			out = append(out, ch)
		} else {
			out = append(out, parent+ch)
		}
	}
	return out
}

// IsDirectChild reports whether child is a one-hop encoding child of parent.
// Rejects empty keys, self-keys (child == parent), and deeper/unrelated hashes.
func IsDirectChild(base Encoder, parent, child string) bool {
	if base == nil || child == "" || child == parent {
		return false
	}
	return base.Parent(child) == parent
}
