package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

const KindInventoryPrefix = "inventory-prefix"

// QueryPrefix asks each disk to filter by collection and canonical path before
// decoding or sending rows. The owner of a parent does not necessarily own its
// descendants, so the read is not narrowed to the parent's route; it is
// narrowed to the prefix's XOR subtree, which holds every descendant
// (PREFIX-1).
func (s *Service) QueryPrefix(ctx context.Context, collection, prefix string) ([]storage.Snapshot, error) {
	zone, err := tree.NewZoneKey(collection, prefix)
	if err != nil {
		return nil, err
	}
	return s.query(ctx, Request{Kind: KindInventoryPrefix, Zone: zone})
}

// QueryPrefixMetrics preserves authoritative coverage without opaque payloads.
func (s *Service) QueryPrefixMetrics(ctx context.Context, collection, prefix string) ([]storage.Snapshot, error) {
	zone, err := tree.NewZoneKey(collection, prefix)
	if err != nil {
		return nil, err
	}
	return s.query(ctx, Request{Kind: KindInventoryPrefix, Zone: zone, Page: &storage.InventoryPageRequest{MetricsOnly: true}})
}

// QueryPrefixLimited bounds retained user records across all holders and pages.
// The budget is shared by the fan-out; overflow discards the whole read.
func (s *Service) QueryPrefixLimited(ctx context.Context, collection, prefix string, records, bytes int) ([]storage.Snapshot, error) {
	zone, err := tree.NewZoneKey(collection, prefix)
	if err != nil {
		return nil, err
	}
	if records < 1 || bytes < 1 {
		return nil, fmt.Errorf("invalid inventory budget")
	}
	return s.queryLimited(ctx, Request{Kind: KindInventoryPrefix, Zone: zone}, records, bytes)
}

var ErrQueryBudget = errors.New("inventory record or byte budget exceeded")

type queryBudget struct {
	mu                                   sync.Mutex
	records, bytes, maxRecords, maxBytes int
}

func (b *queryBudget) accept(rows []storage.Snapshot) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, snap := range rows {
		b.bytes += len(snap.Zone.Collection) + len(snap.Zone.Location) + 128
		for _, row := range snap.Records {
			if !row.Deleted {
				b.records++
			}
			b.bytes += len(row.Payload) + len(row.Collection) + len(row.Location) + len(row.ID) + len(row.Metrics)*8 + 128
			if b.records > b.maxRecords || b.bytes > b.maxBytes {
				return ErrQueryBudget
			}
		}
	}
	if b.bytes > b.maxBytes {
		return ErrQueryBudget
	}
	return nil
}

func (s *Service) query(ctx context.Context, request Request) ([]storage.Snapshot, error) {
	return s.queryLimited(ctx, request, 0, 0)
}
func (s *Service) queryLimited(ctx context.Context, request Request, maxRecords, maxBytes int) ([]storage.Snapshot, error) {
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := s.Membership()
		if err != nil {
			return nil, err
		}
		region := tree.Region{}
		if request.Kind == KindInventoryPrefix {
			region = tree.PrefixRegion(request.Zone)
		}
		view := m.Current()
		var initial, other []causal.Endpoint
		local, e := s.Store.Rights(region)
		if e != nil {
			return nil, e
		}
		if len(local) > 0 {
			initial = append(initial, s.Self)
		}
		for _, member := range m.Export().Members {
			if member.Retired {
				continue
			}
			cell, e := tree.Cell(view, member.VNode.Name)
			if e != nil {
				return nil, e
			}
			if _, ok := cell.Intersect(region); ok {
				initial = append(initial, member.Endpoint)
			} else {
				other = append(other, member.Endpoint)
			}
		}
		var budget *queryBudget
		if maxRecords > 0 {
			budget = &queryBudget{maxRecords: maxRecords, maxBytes: maxBytes}
		}
		rows, err := s.queryCoverage(ctx, initial, other, region, request, budget)
		if errors.Is(err, storage.ErrReadChanged) {
			continue
		}
		return rows, err
	}
	return nil, storage.ErrReadChanged
}

type inventoryResult struct {
	rows     []storage.Snapshot
	refs     []storage.RegionReferral
	coverage []tree.Region
	err      error
}

// Expected XOR cells choose the first probes. Actual durable coverage and all
// mandatory successor/source referrals decide completeness, including empty
// address space. Unknown scope is never silently interpreted as an empty set.
//
// Steps, in order:
//  1. probe the holders expected to cover the region, four at a time;
//  2. follow every mandatory referral into the region, one at a time;
//  3. while the region is not covered, probe one more of the other holders
//     and go back to 2: running out of holders is a failure;
//  4. keep the newest snapshot of each zone.
func (s *Service) queryCoverage(ctx context.Context, initial, other []causal.Endpoint, region tree.Region, r Request, b *queryBudget) ([]storage.Snapshot, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := coverageWalk{s: s, ctx: ctx, region: region, r: r, b: b, asked: map[causal.Endpoint]bool{}, failed: map[causal.Endpoint]error{}}
	if err := w.probeExpected(initial); err != nil {
		return nil, err
	}
	for {
		if err := w.followReferrals(); err != nil {
			return nil, err
		}
		if tree.CoversRegions(region, w.coverage) {
			break
		}
		var next causal.Endpoint
		for len(other) > 0 {
			next, other = other[0], other[1:]
			if !w.asked[next] {
				break
			}
			next = causal.Endpoint{}
		}
		if !next.Valid() {
			return nil, errors.Join(append(w.hintFailures, storage.ErrNotReady)...)
		}
		if err := w.probeOther(next); err != nil {
			return nil, err
		}
	}
	return newestPerZone(w.parts), nil
}

// coverageWalk is the state of one queryCoverage: which holders were asked,
// what they answered and which referrals are still to follow.
type coverageWalk struct {
	s      *Service
	ctx    context.Context
	region tree.Region
	r      Request
	b      *queryBudget

	asked  map[causal.Endpoint]bool
	failed map[causal.Endpoint]error // failed probes of expected and other holders

	parts     [][]storage.Snapshot
	coverage  []tree.Region
	mandatory []storage.RegionReferral
	// hintFailures: failures of holders probed on a guess. They do not end the
	// read; they explain it when the region ends up not covered.
	hintFailures []error
}

// claim marks a holder as asked. A holder asked before is not asked again
// (fresh is false), and the failure of its earlier probe, if any, is returned.
func (w *coverageWalk) claim(to causal.Endpoint) (fresh bool, err error) {
	if w.asked[to] {
		return false, w.failed[to]
	}
	w.asked[to] = true
	return true, nil
}

// absorb keeps the answer of one holder.
func (w *coverageWalk) absorb(rows []storage.Snapshot, refs []storage.RegionReferral, covered []tree.Region) {
	w.parts = append(w.parts, rows)
	w.coverage = append(w.coverage, covered...)
	w.mandatory = append(w.mandatory, refs...)
}

// guessFailed records the failure of a holder probed on a guess. A changed
// read or an exhausted budget ends the whole read and is returned; any other
// failure is only kept.
func (w *coverageWalk) guessFailed(to causal.Endpoint, err error) error {
	w.failed[to] = err
	if errors.Is(err, storage.ErrReadChanged) || errors.Is(err, ErrQueryBudget) {
		return err
	}
	w.hintFailures = append(w.hintFailures, err)
	return nil
}

// probeExpected asks the holders expected to cover the region, four at a
// time, each at most once.
func (w *coverageWalk) probeExpected(initial []causal.Endpoint) error {
	for len(initial) > 0 {
		batch := initial[:min(4, len(initial))]
		initial = initial[len(batch):]
		results := make([]inventoryResult, len(batch))
		var wg sync.WaitGroup
		for i, to := range batch {
			fresh, err := w.claim(to)
			if err != nil {
				return err
			}
			if !fresh {
				continue
			}
			wg.Go(func() {
				results[i].rows, results[i].refs, results[i].coverage, results[i].err = w.s.queryPages(w.ctx, to, w.r, w.b, false)
			})
		}
		wg.Wait()
		for i, result := range results {
			if result.err != nil {
				if err := w.guessFailed(batch[i], result.err); err != nil {
					return err
				}
				continue
			}
			w.absorb(result.rows, result.refs, result.coverage)
		}
	}
	return nil
}

// followReferrals asks every holder a referral names for a part of the
// region, including the referrals their answers add. A referral is
// mandatory: its failure ends the read.
func (w *coverageWalk) followReferrals() error {
	for len(w.mandatory) > 0 {
		ref := w.mandatory[0]
		w.mandatory = w.mandatory[1:]
		if _, intersects := ref.Region.Intersect(w.region); !intersects {
			continue
		}
		to := ref.Endpoint
		if !to.Valid() {
			return causal.ErrConflict
		}
		fresh, err := w.claim(to)
		if err != nil {
			return err
		}
		if !fresh {
			continue
		}
		rows, refs, covered, err := w.s.queryPages(w.ctx, to, w.r, w.b, true)
		if err != nil {
			return err
		}
		w.absorb(rows, refs, covered)
	}
	return nil
}

// probeOther asks one holder that was not expected to cover the region.
func (w *coverageWalk) probeOther(to causal.Endpoint) error {
	w.asked[to] = true
	rows, refs, covered, err := w.s.queryPages(w.ctx, to, w.r, w.b, false)
	if err != nil {
		return w.guessFailed(to, err)
	}
	w.absorb(rows, refs, covered)
	return nil
}

// newestPerZone keeps one snapshot per zone, the one of highest sequence
// (the first met among equals), in the order the zones were first met.
func newestPerZone(parts [][]storage.Snapshot) []storage.Snapshot {
	var out []storage.Snapshot
	at := map[string]int{}
	for _, rows := range parts {
		for _, snap := range rows {
			key := zoneKey(snap)
			if i, ok := at[key]; ok {
				if snap.Seq > out[i].Seq {
					out[i] = snap
				}
				continue
			}
			at[key] = len(out)
			out = append(out, snap)
		}
	}
	return out
}

func (s *Service) queryPages(ctx context.Context, holder causal.Endpoint, r Request, b *queryBudget, _ bool) ([]storage.Snapshot, []storage.RegionReferral, []tree.Region, error) {
	pageRequest := storage.InventoryPageRequest{}
	if r.Page != nil {
		pageRequest.ByteLimit = r.Page.ByteLimit
		pageRequest.MetricsOnly = r.Page.MetricsOnly
	}
	var refs []storage.RegionReferral
	var coverage []tree.Region
	var rows []storage.Snapshot
	var watermark uint64
	var scope [32]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		r.Page = &pageRequest
		r.Target = holder
		var response Response
		var err error
		if holder.Owner == s.Self.Owner {
			response, err = s.Handle(ctx, r)
		} else {
			response, err = s.call(ctx, holder.Host, r)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if err = b.accept(response.Inventory); err != nil {
			return nil, nil, nil, err
		}

		page := response.Page
		if page == nil || page.Watermark == 0 || !page.Done && page.Next == nil || page.Done && page.Next != nil {
			return nil, nil, nil, storage.ErrReadChanged
		}
		if pageRequest.After != nil && (page.Watermark != watermark || page.Scope != scope) {
			return nil, nil, nil, storage.ErrReadChanged
		}
		watermark = page.Watermark
		scope = page.Scope
		refs = append(refs, page.Referrals...)
		coverage = append(coverage, page.Coverage...)
		for _, snap := range response.Inventory {
			if len(rows) > 0 {
				last := &rows[len(rows)-1]
				if last.Zone == snap.Zone && last.Seq == snap.Seq {
					last.Records = append(last.Records, snap.Records...)
					continue
				}
			}
			rows = append(rows, snap)
		}
		if page.Done {
			return rows, refs, coverage, nil
		}
		if page.Next.Watermark != watermark || page.Next.Scope != scope || pageRequest.After != nil && !inventoryCursorAdvances(*pageRequest.After, *page.Next) {
			return nil, nil, nil, storage.ErrReadChanged
		}
		pageRequest.After = page.Next
	}
}

func inventoryCursorAdvances(previous, next storage.InventoryCursor) bool {
	if previous.Collection != next.Collection {
		return next.Collection > previous.Collection
	}
	if previous.Location != next.Location {
		// the store's key order (routing alphabet), not Go's string order
		return storage.CompareLocations(next.Location, previous.Location) > 0
	}
	if previous.EndOfZone {
		return false
	}
	if next.EndOfZone {
		return true
	}
	return next.RecordID > previous.RecordID
}
