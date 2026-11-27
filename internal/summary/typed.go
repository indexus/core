package summary

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/algebra"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// The typed fold: the value of a zone of a collection whose description has
// a column that is not a sum (docs/protocol/collection-description.md,
// section 5). Sums are read from the zonelog segments and corrected by
// subtracting what a remote child covers (store.go); min, max, first, last
// and affine columns have no inverse, so here the value of a zone is the
// fold, in cell order, of the local records at or below it that no remote
// child covers and of those remote children's values. Exact, and O(n log n)
// in the records of the zone.

// typedElem is one operand of a typed fold.
type typedElem struct {
	key   []byte // the location in cell order (algebra.CellOrder)
	id    string // the record id; "" for a remote child
	loc   string // the record's location, or the remote child's
	exact string // the one location of everything it holds, or ""
	value algebra.Value
}

// sortTyped puts elements in cell order: by location with the rotation
// removed, a prefix before its extensions, then by record id.
func sortTyped(elems []typedElem) {
	sort.SliceStable(elems, func(i, j int) bool {
		if c := bytes.Compare(elems[i].key, elems[j].key); c != 0 {
			return c < 0
		}
		return elems[i].id < elems[j].id
	})
}

// typedCollection is the description of a collection that takes the typed
// fold, or nil (a legacy or all-sum collection).
func (t *Tree) typedCollection(collection string) *storage.Collection {
	c, err := t.service.Store.CollectionOf(collection)
	if err != nil || !c.Typed() {
		return nil
	}
	return c
}

// recordValue is the value of one record: its metrics as columns, counted
// for its weight (at least once).
func recordValue(s algebra.Schema, weight float64, metrics []float64) algebra.Value {
	v := s.Lift(metrics)
	v.Count = int64(max(1, itemCount(weight)))
	return v
}

// coveredBy reports whether location lies in one of remotes, sorted and
// none below another (incomingBelowLocked).
func coveredBy(remotes []string, location string) bool {
	i := sort.SearchStrings(remotes, location)
	if i < len(remotes) && remotes[i] == location {
		return true
	}
	return i > 0 && strings.HasPrefix(location, remotes[i-1])
}

// typedElemsLocked lists, in cell order, what the value of a zone folds.
// Records and contributions whose width is not the schema's are left out:
// a record written before its collection was described, or a peer's bug.
func (t *Tree) typedElemsWithIncoming(c *storage.Collection, collection, location string, incoming []incomingEntry) ([]typedElem, error) {
	d := c.Desc
	remotes := make([]string, len(incoming))
	var elems []typedElem
	for i, in := range incoming {
		remotes[i] = in.key.Location
		if in.value == nil || in.value.Count() == 0 || len(in.value.Metrics()) != len(d.Schema) {
			continue
		}
		elems = append(elems, typedElem{
			key: algebra.CellOrder(d.BitsPerChar, collection, in.key.Location), loc: in.key.Location, exact: in.exact,
			value: algebra.Value{Count: int64(in.value.Count()), Cols: in.value.Metrics()},
		})
	}
	err := t.service.Store.ZoneRecords(collection, location, func(r storage.ZoneRecord) bool {
		if coveredBy(remotes, r.Location) || len(r.Metrics) != len(d.Schema) {
			return true
		}
		elems = append(elems, typedElem{
			key: algebra.CellOrder(d.BitsPerChar, collection, r.Location), id: r.ID, loc: r.Location, exact: r.Location,
			value: recordValue(d.Schema, r.Weight, r.Metrics),
		})
		return true
	})
	sortTyped(elems)
	return elems, err
}

// foldTyped folds elements already in cell order, with the one location
// they all sit at, or "".
func foldTyped(s algebra.Schema, elems []typedElem) (*wire.Abelian, string) {
	acc, exact := s.Zero(), ""
	for i, e := range elems {
		acc = s.Combine(acc, e.value)
		if i == 0 {
			exact = e.exact
		} else if e.exact != exact {
			exact = ""
		}
	}
	if acc.Count == 0 {
		return wire.NewAbelian(0, nil), ""
	}
	return wire.NewAbelian(int(acc.Count), acc.Cols), exact
}

func (t *Tree) typedValueWithIncoming(c *storage.Collection, collection, location string, incoming []incomingEntry) (*wire.Abelian, string, error) {
	elems, err := t.typedElemsWithIncoming(c, collection, location, incoming)
	if err != nil {
		return nil, "", err
	}
	value, exact := foldTyped(c.Desc.Schema, elems)
	return value, exact, nil
}

// typedSet is the keys of a zone (query.md): the records placed exactly at
// it, then one key per child, each folded in cell order; the set's total is
// the fold of all of them, not their sum.
func typedSet(s algebra.Schema, path string, elems []typedElem) *wire.Set {
	var parts [65][]typedElem
	for _, e := range elems {
		slot := 0
		if len(e.loc) > len(path) && e.loc != summaryLocation(path) {
			slot = 1 + strings.IndexByte(routingAlphabet, e.loc[len(path)])
		}
		if slot < 0 {
			continue
		}
		parts[slot] = append(parts[slot], e)
	}
	set := wire.NewSet()
	if value, _ := foldTyped(s, parts[0]); value.Count() > 0 {
		set.Put(summaryLocation(path), value)
	}
	for c := range 64 {
		if value, exact := foldTyped(s, parts[1+c]); value.Count() > 0 {
			child := path + routingAlphabet[c:c+1]
			set.Put(groupKey(path, child, exact), value)
		}
	}
	total, _ := foldTyped(s, elems)
	set.SetTotal(total)
	return set
}

func (t *Tree) typedSetWithIncoming(c *storage.Collection, collection, location string, incoming []incomingEntry) (*wire.Set, error) {
	elems, err := t.typedElemsWithIncoming(c, collection, location, incoming)
	if err != nil {
		return nil, err
	}
	return typedSet(c.Desc.Schema, summaryPath(location), elems), nil
}

// descriptionDigest is the hex digest a contribution of a described
// collection carries (DESC-4), or "".
func (t *Tree) descriptionDigest(collection string) string {
	c, err := t.service.Store.CollectionOf(collection)
	if err != nil || c == nil {
		return ""
	}
	return hex.EncodeToString(c.Digest[:])
}

// checkDescription is DESC-4 for an incoming contribution: one that carries
// a digest is folded only with that description. A description this disk
// lacks is fetched from the sender and verified; a different one is a
// forged collision or a bug, and the contribution is dropped, never merged.
// A contribution without a digest comes from a disk that reads the
// collection as legacy: right for a legacy or all-sum collection, wrong for a
// typed one (a sum of maxima is not a maximum). Then the contribution is
// dropped and the child is sent the description; its next push, or the
// repair poll, brings the typed value.
func (t *Tree) checkDescription(in mesh.Contribution) (bool, error) {
	c, err := t.service.Store.CollectionOf(in.Collection)
	if err != nil {
		return false, err
	}
	if in.Digest == "" {
		if !c.Typed() {
			return true, nil
		}
		t.stats.droppedDescription.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := t.service.Exchange(ctx, in.Holder.Host, mesh.Request{Kind: mesh.KindAdmitCollection, Collection: in.Collection, Description: c.Bytes}); err != nil {
			slog.Warn("typed contribution without its description; sending it failed", "collection", in.Collection, "to", in.Holder.Host, "error", err)
		}
		return false, nil
	}
	if c == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if c, err = t.service.FetchCollection(ctx, in.Holder.Host, in.Collection); err != nil {
			return false, fmt.Errorf("summary: description of %s from %s: %w", in.Collection, in.Holder.Host, err)
		}
	}
	if hex.EncodeToString(c.Digest[:]) != in.Digest {
		t.stats.droppedDescription.Add(1)
		slog.Warn("contribution dropped: description digest differs", "collection", in.Collection, "location", in.Location, "from", in.Holder.Host)
		return false, nil
	}
	return true, nil
}
