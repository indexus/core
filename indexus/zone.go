package indexus

// zoneKeyID is id(z) = location || collection. Ownership of that id is
// topology.View.Owner / Owner, never a capacity exception (D1).
func zoneKeyID(collection, location string) ([]byte, error) {
	id, err := ZoneID(collection, location)
	if err != nil {
		return nil, err
	}
	return id.Bytes(), nil
}
