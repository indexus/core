package tree

import "fmt"

// Region is a cube of the 96-bit key space. Unmasked bits are unconstrained.
// Unlike an inventory of existing rows it also covers future, empty zones.
type Region struct{ Mask, Value ID }

// FullMask pins every bit of a 96-bit ID. Point(id) is the cube that contains
// only that identifier, used as a per-zone activation proof.
var FullMask = ID{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255}

func Point(id ID) Region { return Region{Mask: FullMask, Value: id} }

func (r Region) Valid() bool {
	for i := range r.Mask {
		if r.Value[i] & ^r.Mask[i] != 0 {
			return false
		}
	}
	return true
}
func (r Region) Contains(id ID) bool {
	for i := range r.Mask {
		if id[i]&r.Mask[i] != r.Value[i] {
			return false
		}
	}
	return true
}
func (r Region) Intersect(other Region) (Region, bool) {
	var out Region
	for i := range r.Mask {
		if (r.Value[i]^other.Value[i])&r.Mask[i]&other.Mask[i] != 0 {
			return Region{}, false
		}
		out.Mask[i] = r.Mask[i] | other.Mask[i]
		out.Value[i] = r.Value[i] | other.Value[i]
	}
	return out, true
}

// Subtract returns disjoint cubes covering r minus other, without enumerating
// keys. Each new constraint contributes at most one residual cube.
func (r Region) Subtract(other Region) []Region {
	overlap, ok := r.Intersect(other)
	if !ok {
		return []Region{r}
	}
	var out []Region
	for i := range r.Mask {
		for bit := byte(128); bit != 0; bit >>= 1 {
			if overlap.Mask[i]&bit == 0 || r.Mask[i]&bit != 0 {
				continue
			}
			residual := r
			residual.Mask[i] |= bit
			residual.Value[i] |= (^overlap.Value[i]) & bit
			out = append(out, residual)
			r.Mask[i] |= bit
			r.Value[i] |= overlap.Value[i] & bit
		}
	}
	return out
}

// Cell gives the exact XOR cell, using the first differing bit of each pair.
// Beating every competitor is equivalent to View.Owner(key)==vnode.ID.
func Cell(view *View, name string) (Region, error) {
	owner, found := view.byName[name]
	if !found {
		for _, vnode := range view.Active {
			if vnode.Name == name {
				owner, found = vnode.ID, true
				break
			}
		}
	}
	if !found {
		return Region{}, fmt.Errorf("vnode absent from view")
	}
	var out Region
	// Each branching bit on the owner's path is exactly a first differing
	// bit against a competitor. Reuse the placement trie instead of visiting
	// every competitor again for every cell in a regional query.
	if len(view.xorIndex) > 0 {
		at := int32(0)
		for view.xorIndex[at].bit < Size*8 {
			node := view.xorIndex[at]
			i, bit := node.bit/8, byte(128)>>(node.bit%8)
			out.Mask[i] |= bit
			out.Value[i] |= owner[i] & bit
			at = node.child[Bit(owner, node.bit)]
		}
		return out, nil
	}
	for _, other := range view.Active {
		if other.Name == name {
			continue
		}
		for i := range owner {
			different := owner[i] ^ other.ID[i]
			if different == 0 {
				continue
			}
			bit := byte(128)
			for different&bit == 0 {
				bit >>= 1
			}
			out.Mask[i] |= bit
			out.Value[i] |= owner[i] & bit
			break
		}
	}
	return out, nil
}
func CoversRegions(region Region, pieces []Region) bool {
	pending := []Region{region}
	for _, piece := range pieces {
		var next []Region
		for _, part := range pending {
			next = append(next, part.Subtract(piece)...)
		}
		pending = next
	}
	return len(pending) == 0
}
