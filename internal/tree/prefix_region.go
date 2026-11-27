package tree

// PrefixRegion is the XOR subtree holding a location prefix and every
// descendant of it, in any collection: zone ids start with the location's bits
// (6 per character), so a k-character prefix fixes the first 6k bits (PREFIX-1).
// The root fixes nothing; a full-length location fixes every bit, one point.
func PrefixRegion(zone ZoneKey) Region {
	var out Region
	location := zone.Location
	if location == "" || location == BASE64.Root() {
		return out
	}
	bits := min(6*len(location), 8*Size)
	for i := 0; i < bits; i++ {
		out.Mask[i/8] |= 1 << (7 - i%8)
	}
	for i := range out.Mask {
		out.Value[i] = zone.ID[i] & out.Mask[i]
	}
	return out
}
