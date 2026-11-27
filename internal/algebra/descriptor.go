package algebra

import "fmt"

// A Descriptor is everything a node or a client needs to compute on a
// collection: turn points into locations and back, and aggregate zones.
// Nothing about what the data means.
type Descriptor struct {
	// Geometry of the addresses: bits per character and characters per
	// address (base 2^BitsPerChar, Width characters).
	BitsPerChar, Width int
	// Nonce makes two collections with the same mathematics distinct. It
	// carries no meaning.
	Nonce [16]byte
	// Space: how points become locations, and back (see space.go).
	Space Space
	// RecordDepths: records are placed in zones at depths in [lo, hi].
	RecordDepths [2]int
	// Schema: the aggregation of every column, the same at every depth.
	Schema Schema
}

// Validate checks the geometry, the space and the schema.
func (d *Descriptor) Validate() error {
	if d.BitsPerChar < 1 || d.BitsPerChar > 8 || d.Width < 1 || d.BitsPerChar*d.Width > 255 {
		return fmt.Errorf("%w: geometry %d bits x %d characters", ErrSchema, d.BitsPerChar, d.Width)
	}
	if d.RecordDepths[0] < 0 || d.RecordDepths[1] < d.RecordDepths[0] || d.RecordDepths[1] > d.Width {
		return fmt.Errorf("%w: record depths %v", ErrSchema, d.RecordDepths)
	}
	if err := d.Space.ValidateSpace(d.BitsPerChar, d.Width, d.RecordDepths[1]); err != nil {
		return err
	}
	if len(d.Schema) > MaxColumns {
		return fmt.Errorf("%w: more than %d columns", ErrSchema, MaxColumns)
	}
	return d.Schema.Validate()
}
