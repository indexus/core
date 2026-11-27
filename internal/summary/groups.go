package summary

import (
	"strings"

	"github.com/indexus/go-indexus-core/internal/algebra"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// zoneKeyDepth is how far below a zone a key may reach (query.md, "Keys of a
// zone"): 4 characters, 24 bits, cover the 4 binary levels the SDK walk
// splits a cell into (MAX_VIRTUAL_CHILD_DEPTH_DELTA) for up to 4 dimensions.
const zoneKeyDepth = 4

// groupKey is the key of the records below child, one character under path:
// their one location when they have one, cut zoneKeyDepth characters below
// path, else child itself.
func groupKey(path, child, exact string) string {
	if len(exact) <= len(child) || !strings.HasPrefix(exact, child) {
		return child
	}
	return exact[:min(len(exact), len(path)+zoneKeyDepth)]
}

// Groups builds the keys of one zone from its records, as the owner's
// summary would: the zone itself for the records placed exactly there, and
// one key per child character.
type Groups struct {
	Path   string // the zone as a prefix of its descendants: "" for the root
	self   *wire.Abelian
	groups map[byte]*zoneGroup
	order  []byte
	// typed is set for a collection with a column that is not a sum: the
	// records are kept and folded in cell order (typed.go).
	typed      *storage.Collection
	collection string
	elems      []typedElem
}

type zoneGroup struct {
	exact string
	value *wire.Abelian
}

// NewGroups starts the keys of the zone at location.
func NewGroups(location string) *Groups {
	return &Groups{Path: summaryPath(location), groups: map[byte]*zoneGroup{}}
}

// NewGroupsOf starts the keys of a zone of a collection, with its
// description (nil for a legacy collection).
func NewGroupsOf(collection, location string, c *storage.Collection) *Groups {
	g := NewGroups(location)
	if c.Typed() {
		g.typed, g.collection = c, collection
	}
	return g
}

// AddRecord counts a record; its id orders records at one location.
func (g *Groups) AddRecord(location, id string, weight float64, metrics []float64) {
	if g.typed != nil {
		d := g.typed.Desc
		if len(metrics) == len(d.Schema) {
			g.elems = append(g.elems, typedElem{
				key: algebra.CellOrder(d.BitsPerChar, g.collection, location), id: id, loc: location, exact: location,
				value: recordValue(d.Schema, weight, metrics),
			})
		}
		return
	}
	value := wire.NewAbelian(itemCount(weight), metrics)
	if location == g.Path {
		if g.self == nil {
			g.self = wire.NewAbelian(0, nil)
		}
		g.self.Sum(value)
		return
	}
	c := location[len(g.Path)]
	group := g.groups[c]
	if group == nil {
		g.groups[c] = &zoneGroup{exact: location, value: value}
		g.order = append(g.order, c)
		return
	}
	if group.exact != location {
		group.exact = ""
	}
	group.value.Sum(value)
}

// Set is the keys of the zone.
func (g *Groups) Set() *wire.Set {
	if g.typed != nil {
		elems := append([]typedElem(nil), g.elems...)
		sortTyped(elems)
		return typedSet(g.typed.Desc.Schema, g.Path, elems)
	}
	set := wire.NewSet()
	if g.self != nil {
		set.Put(summaryLocation(g.Path), g.self)
	}
	for _, c := range g.order {
		group := g.groups[c]
		set.Put(groupKey(g.Path, g.Path+string(c), group.exact), group.value)
	}
	return set
}
