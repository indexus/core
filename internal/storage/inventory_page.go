package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

const DefaultInventoryPageBytes = 1 << 20
const MaxInventoryRPCBytes = 32 << 20

var (
	ErrReadChanged           = fmt.Errorf("%w: inventory changed during paginated read", ErrNotReady)
	ErrInventoryItemTooLarge = errors.New("inventory item exceeds RPC response limit")
)

// InventoryCursor addresses canonical stored keys, never their geometric ID.
// EndOfZone distinguishes an empty zone from a record whose ID is empty.
// Watermark tracks collection data; Scope binds current coverage and referrals.
// Unrelated maintenance cannot invalidate a cursor. No transaction is retained.
type InventoryCursor struct {
	Collection string
	Location   string
	RecordID   string
	Watermark  uint64
	Scope      [32]byte
	EndOfZone  bool
}
type InventoryPageRequest struct {
	After       *InventoryCursor
	ByteLimit   int
	MetricsOnly bool // Project metrics before retaining or transmitting opaque content.

}
type InventoryPage struct {
	Next      *InventoryCursor
	Watermark uint64
	Scope     [32]byte
	Done      bool
	// Referrals belongs to the same snapshot as the rows. The mesh carries
	// it in Response.Referrals, not inside the cursor/page wire metadata.
	Referrals []RegionReferral `json:",omitempty"`
	Coverage  []tree.Region    `json:",omitempty"`
}

func inventoryQueryRegion(query *tree.ZoneKey) tree.Region {
	if query == nil {
		return tree.Region{}
	}
	return tree.PrefixRegion(*query)
}

func containsInventoryQuery(query *tree.ZoneKey, z tree.ZoneKey) bool {
	return query == nil || query.Collection == z.Collection && (query.Location == "@" || strings.HasPrefix(z.Location, query.Location))
}

// CausalInventoryPage reads one bounded page after rechecking all completeness
// guards. A mutation between pages invalidates the cursor and the entire read.
// A single row may exceed the soft page budget, but never the hard RPC bound.
func (s *Store) CausalInventoryPage(ctx context.Context, query *tree.ZoneKey, request InventoryPageRequest) ([]Snapshot, InventoryPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, InventoryPage{}, err
	}
	if query != nil {
		canonical, err := tree.NewZoneKey(query.Collection, query.Location)
		if err != nil {
			return nil, InventoryPage{}, err
		}
		query = &canonical
	}
	p := inventoryPager{ctx: ctx, store: s, query: query, after: request.After, metricsOnly: request.MetricsOnly, limit: request.ByteLimit, first: true}
	if p.limit <= 0 || p.limit > DefaultInventoryPageBytes {
		p.limit = DefaultInventoryPageBytes
	}
	if err := s.view(p.read); err != nil {
		return nil, InventoryPage{}, err
	}
	return p.out, p.page, nil
}

// inventoryPager reads one inventory page inside one transaction. The walk
// calls openZone at each zone and add at each of its rows; closeZone lists
// the zone being read. stopped means the byte budget ended the page.
type inventoryPager struct {
	ctx         context.Context
	store       *Store
	query       *tree.ZoneKey
	after       *InventoryCursor
	metricsOnly bool
	limit       int

	tx      *Tx
	control *causalSnapshot
	from    *tree.ZoneKey // the cursor's zone, nil on a first page
	behind  string        // the cursor's record

	out  []Snapshot
	page InventoryPage
	used int
	last *InventoryCursor // the last entry listed: where the next page resumes

	snapshot Snapshot // the zone being read
	overhead int      // bytes of its empty snapshot
	open     bool     // a zone is being read
	resumed  bool     // it was listed on the previous page already
	first    bool     // no zone was visited yet
	stopped  bool
}

func (p *inventoryPager) read(tx *Tx) error {
	p.tx = tx
	if err := p.guards(); err != nil {
		return err
	}
	p.page.Watermark, p.page.Scope = tx.inventoryRevision(p.query), inventoryScope(p.page)
	if err := p.resume(); err != nil {
		return err
	}
	err := tx.walk(p.query, p.from, p.behind, !p.metricsOnly, func(zone tree.ZoneKey, r *row) (bool, error) {
		if err := p.ctx.Err(); err != nil {
			return false, err
		}
		if r == nil {
			return p.openZone(zone)
		}
		return p.add(zone, r)
	})
	if err != nil {
		return err
	}
	if p.first && p.after != nil {
		return ErrReadChanged // the cursor's zone is gone
	}
	if p.closeZone(); p.stopped {
		p.page.Next = p.last
		return nil
	}
	p.page.Done = true
	return nil
}

// guards reads what makes the page's answer complete: the control snapshot,
// the coverage of the query and the referrals to other disks.
func (p *inventoryPager) guards() (err error) {
	if err = p.ctx.Err(); err != nil {
		return err
	}
	if p.control, err = p.store.causalSnapshot(p.tx); err != nil {
		return err
	}
	region := inventoryQueryRegion(p.query)
	if p.page.Coverage, err = inventoryCoverage(p.tx, p.control, region, p.query); err != nil {
		return err
	}
	if p.page.Referrals, err = inventoryReferrals(p.tx, p.control, region, p.query); err != nil {
		return err
	}
	return p.ctx.Err()
}

// resume checks the cursor of the previous page against this transaction:
// same watermark, a zone inside the query, a record that still exists.
func (p *inventoryPager) resume() error {
	after := p.after
	if after == nil {
		return nil
	}
	if after.Watermark != p.page.Watermark || after.Scope != p.page.Scope {
		return ErrReadChanged
	}
	zone, err := tree.NewZoneKey(after.Collection, after.Location)
	if err != nil || zone.Collection != after.Collection || zone.Location != after.Location || !containsInventoryQuery(p.query, zone) || after.EndOfZone && after.RecordID != "" {
		return ErrReadChanged
	}
	p.from, p.behind = &zone, after.RecordID
	if after.EndOfZone {
		return nil
	}
	if r, err := p.tx.record(zone, p.behind); err != nil || r == nil {
		return errors.Join(err, ErrReadChanged)
	}
	return nil
}

// closeZone lists the zone being read. A zone with no row on this page is
// still listed once, so an empty zone is not lost; a zone resumed from the
// previous page was listed there.
func (p *inventoryPager) closeZone() {
	if !p.open {
		return
	}
	p.open = false
	if len(p.snapshot.Records) > 0 {
		p.out = append(p.out, p.snapshot)
		return
	}
	if p.resumed {
		return
	}
	if p.used+p.overhead > p.limit && p.last != nil {
		p.stopped = true
		return
	}
	p.out = append(p.out, p.snapshot)
	p.used += p.overhead
	p.last = &InventoryCursor{Collection: p.snapshot.Zone.Collection, Location: p.snapshot.Zone.Location, Watermark: p.page.Watermark, Scope: p.page.Scope, EndOfZone: true}
}

// openZone closes the previous zone and starts zone, unless this disk may
// not read it (skipped) or the cursor had finished it already.
func (p *inventoryPager) openZone(zone tree.ZoneKey) (bool, error) {
	if p.closeZone(); p.stopped {
		return false, nil
	}
	p.resumed = false
	wasFirst := p.first
	p.first = false
	if wasFirst && p.after != nil {
		if zone != *p.from {
			return false, ErrReadChanged
		}
		if p.after.EndOfZone {
			return true, errSkipZone
		}
		p.resumed = true
	}
	if err := p.store.readableIn(p.tx, p.control, zone); err != nil {
		if errors.Is(err, ErrNotOwner) || errors.Is(err, ErrNotReady) {
			return true, errSkipZone
		}
		return false, err
	}
	p.snapshot = Snapshot{Zone: zone, Seq: liveZone(zone).seq(p.tx), Records: []Record{}}
	metadata, err := json.Marshal(p.snapshot)
	if err != nil {
		return false, err
	}
	p.overhead, p.open = len(metadata)+1, true
	return true, nil
}

// add lists one row of the open zone, or stops the page when the row does
// not fit and the page already lists something.
func (p *inventoryPager) add(zone tree.ZoneKey, r *row) (bool, error) {
	if r.size > MaxInventoryRPCBytes {
		return false, ErrInventoryItemTooLarge
	}
	record, cost := *r.record, r.size+1
	if p.metricsOnly {
		record.Payload = nil
		projected, err := json.Marshal(record)
		if err != nil {
			return false, err
		}
		cost = len(projected) + 1
	}
	if len(p.snapshot.Records) == 0 {
		cost += p.overhead
	}
	if p.used+cost > p.limit && p.last != nil {
		if len(p.snapshot.Records) > 0 {
			p.out = append(p.out, p.snapshot)
		}
		p.open, p.stopped = false, true
		return false, nil
	}
	p.snapshot.Records = append(p.snapshot.Records, record)
	p.used += cost
	p.last = &InventoryCursor{Collection: zone.Collection, Location: zone.Location, RecordID: r.id, Watermark: p.page.Watermark, Scope: p.page.Scope}
	return true, nil
}

// Revision is the last commit visible to a new reader (zonelog's seq).
// It is a conservative invalidation key: unrelated metadata commits can cause
// extra work, but unchanged storage cannot hide a committed data mutation.
func (s *Store) Revision() (uint64, error) {
	return read(s, func(tx *Tx) (uint64, error) { return uint64(tx.ID()), nil })
}

// CompareLocations orders locations as a paged read visits them (the store's
// key order), for checking that a cursor moves forward.
func CompareLocations(a, b string) int { return zonelog.CompareLocations(a, b) }
