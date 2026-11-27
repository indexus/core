package storage

import (
	"errors"
	"fmt"
	"sort"

	"github.com/indexus/go-indexus-core/internal/algebra"
)

// The collection catalog: the verified description (IXC1, docs/protocol/
// collection-description.md) of every collection this disk has admitted,
// keyed by its name. A description is immutable and is the hash of its
// name, so a node keeps it the same way whoever sent it; a different
// description for a held name is refused, never merged.
//
// A collection without a description is a legacy collection: every column
// is a sum, read through the zonelog sums as before.

var collectionsBucket = []byte("collections-v1")

// ErrDescription reports a write that does not fit its collection's
// description (wrong column count).
var ErrDescription = errors.New("storage: record does not fit the collection description")

// Collection is an admitted description.
type Collection struct {
	Name   string
	Digest [32]byte
	Bytes  []byte
	Desc   *algebra.Descriptor
}

// Typed reports whether the collection needs the typed fold: a schema with
// a column that is not a sum. An all-sum schema reads through the zonelog
// sums, exactly as a legacy collection.
func (c *Collection) Typed() bool {
	if c == nil {
		return false
	}
	for _, k := range c.Desc.Schema {
		if k != algebra.Sum {
			return true
		}
	}
	return false
}

// AdmitCollection verifies a description against its name and keeps it.
// It is idempotent; another description for a held name is refused
// (algebra.ErrCollision).
func (s *Store) AdmitCollection(name string, ixc1 []byte) (*Collection, error) {
	// Verify first: bytes that are not the name's hash are wrong bytes, not a
	// collision; only a verified description can collide.
	c, err := verifyCollection(name, ixc1)
	if err != nil {
		return nil, err
	}
	if held, ok := s.cachedCollection(name); ok {
		if held.Digest != c.Digest {
			return nil, fmt.Errorf("%w: %s", algebra.ErrCollision, name)
		}
		return held, nil
	}
	if err := s.update(func(tx *Tx) error { return admitTx(tx, c) }); err != nil {
		return nil, err
	}
	return s.remember(c), nil
}

// AdmitDescriptions admits descriptions named by their own hash: what a
// disk sends another before handing it zones (DESC-3).
func (s *Store) AdmitDescriptions(descriptions [][]byte) error {
	for _, b := range descriptions {
		if _, err := s.AdmitCollection(algebra.NameOf(algebra.Digest(b)), b); err != nil {
			return err
		}
	}
	return nil
}

// Descriptions lists the IXC1 bytes of every admitted description, by name.
func (s *Store) Descriptions() [][]byte {
	var names []string
	s.collections.Range(func(k, v any) bool {
		if v.(*Collection) != nil {
			names = append(names, k.(string))
		}
		return true
	})
	sort.Strings(names)
	out := make([][]byte, len(names))
	for i, name := range names {
		c, _ := s.cachedCollection(name)
		out[i] = c.Bytes
	}
	return out
}

func verifyCollection(name string, ixc1 []byte) (*Collection, error) {
	d, digest, err := algebra.Verify(name, ixc1)
	if err != nil {
		return nil, err
	}
	return &Collection{Name: name, Digest: digest, Bytes: append([]byte(nil), ixc1...), Desc: d}, nil
}

// admitTx keeps a verified description in an update; another update may
// carry the records of the same collection (DESC-2).
func admitTx(tx *Tx, c *Collection) error {
	path := key(collectionsBucket, []byte(c.Name))
	if held := tx.value(path); held != nil {
		if algebra.Digest(held) != c.Digest {
			return fmt.Errorf("%w: %s", algebra.ErrCollision, c.Name)
		}
		return nil
	}
	return tx.put(path, c.Bytes)
}

// CollectionOf returns the admitted description of a collection, or nil for
// a legacy collection. The catalog is read into memory when the store opens
// (loadCollections) and kept there by AdmitCollection: a write checks its
// collection without reading the disk or waiting for an update in flight.
func (s *Store) CollectionOf(name string) (*Collection, error) {
	c, _ := s.cachedCollection(name)
	return c, nil
}

// loadCollections reads the whole catalog into the cache: a few hundred
// bytes per collection, verified again (hash = name) as it is read.
func (s *Store) loadCollections() error {
	return s.view(func(tx *Tx) error {
		var bad error
		err := tx.each(key(collectionsBucket), "", func(path string, value []byte) bool {
			elems := split(path)
			name := string(elems[len(elems)-1])
			c, err := verifyCollection(name, value)
			if err != nil {
				bad = fmt.Errorf("storage: held description of %q: %w", name, err)
				return false
			}
			s.remember(c)
			return true
		})
		if err != nil {
			return err
		}
		return bad
	})
}

// FitsCollection checks a record against its collection's description: one
// metric per column, at a record depth. Legacy collections take any record.
func FitsCollection(c *Collection, r Record) error {
	if c == nil || r.Deleted {
		return nil
	}
	if len(r.Metrics) != len(c.Desc.Schema) {
		return fmt.Errorf("%w: %s has %d columns, the record %d metrics", ErrDescription, c.Name, len(c.Desc.Schema), len(r.Metrics))
	}
	depth := len(r.Location)
	if r.Location == "@" {
		depth = 0
	}
	if lo, hi := c.Desc.RecordDepths[0], c.Desc.RecordDepths[1]; depth < lo || depth > hi {
		return fmt.Errorf("%w: %s places records at depths %d to %d, not %d", ErrDescription, c.Name, lo, hi, depth)
	}
	return nil
}

func (s *Store) cachedCollection(name string) (*Collection, bool) {
	v, ok := s.collections.Load(name)
	if !ok {
		return nil, false
	}
	return v.(*Collection), true
}

// remember caches an admitted description; the first one cached for a name
// wins (any other with the same digest is the same bytes).
func (s *Store) remember(c *Collection) *Collection {
	v, _ := s.collections.LoadOrStore(c.Name, c)
	return v.(*Collection)
}
