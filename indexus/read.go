package indexus

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func (n *Node) remote(c Contact) bool {
	return c != nil && c.Name() != n.Name() && contactDialable(c)
}

func (n *Node) RoutingHint(clientKey []byte) Contact {
	if len(clientKey) == 0 {
		return nil
	}
	best := FromSlice(n.ID())
	key := FromSlice(clientKey)
	var hint Contact
	for _, c := range n.traverseRegistered(false) {
		if !n.remote(c) || !contactDialable(c) {
			continue
		}
		cand := FromSlice(c.ID())
		if hint == nil || tree.XORCompare(key, cand, best) < 0 {
			best = cand
			hint = c
		}
	}
	return hint
}

func (n *Node) Get(collection, location string, deep bool, via Visited, refresh bool) (Contact, *Set, error) {
	return n.GetSetContext(context.Background(), collection, location, deep, via, refresh)
}

// GetSetContext shares one admission budget with every other read on this node.
func (n *Node) GetSetContext(ctx context.Context, collection, location string, deep bool, via Visited, refresh bool) (Contact, *Set, error) {
	if n.mesh == nil {
		return nil, nil, ErrNotReady
	}
	ctx, done := n.mesh.clientActivity.Begin(ctx)
	defer done()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case n.mesh.readSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-n.mesh.readSlots }()
	contact, set, err := n.meshGet(ctx, collection, location, deep || refresh, refresh)
	if err == nil && set != nil {
		n.mesh.reads.put(Key{Collection: collection, Location: location}, set, time.Now())
	}
	return contact, set, err
}

func (n *Node) GetAggregates(collection string, locations []string) (map[string]*Abelian, error) {
	if n.mesh == nil {
		return nil, ErrNotReady
	}
	ctx, done := n.mesh.clientActivity.Begin(context.Background())
	defer done()
	out := make(map[string]*Abelian, len(locations))
	snapshot, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return nil, err
	}
	view := snapshot.Current()
	if !view.HasName(n.Name()) {
		return out, nil
	}
	placement, err := snapshot.Placement(n.Name())
	if err != nil {
		return nil, err
	}
	if placement.Storage != n.mesh.service.Self.Storage {
		return out, nil
	}
	for _, location := range locations {
		location = strings.TrimSpace(location)
		if location == "" {
			continue
		}
		zone, err := NewZoneKey(collection, location)
		if err != nil {
			return nil, err
		}
		owner, _ := view.OwnerName(zone.ID)
		if owner != n.Name() {
			continue
		}
		if n.mesh.summary.ReadyFor(collection, location) {
			out[location] = n.mesh.summary.Owned(collection, location)
		} else {
			_, set, err := n.meshGet(ctx, collection, location, true, true)
			if err != nil {
				return nil, err
			}
			if set == nil {
				return nil, ErrNotReady
			}
			out[location] = set.Abelian()
		}
	}
	return out, nil
}

const batchParallelism = 8

// GetSetsBlockContext serves a read of /sets as one IXB2 body, the only
// /sets encoding, to a client or a peer: with options set, one explicit read
// policy for every location (a failed location fails the batch). Its metrics
// are as wide as width says; columns, when set, selects which (wire.EncodeSetsBlockColumns).
func (n *Node) GetSetsBlockContext(parent context.Context, collection string, locations []string, deep bool, via Visited, refresh bool, options *ReadOptions, width wire.SetsBlockMetrics, columns []int) ([]byte, error) {
	if options != nil {
		if err := options.validate(); err != nil {
			return nil, err
		}
		deep, via, refresh = true, nil, true
	}
	if n.mesh == nil {
		return nil, ErrNotReady
	}
	parent, done := n.mesh.clientActivity.Begin(parent)
	defer done()
	resolved, locations, redirects, err := n.resolveSets(parent, collection, locations, deep, via, refresh, true, options)
	if err != nil {
		return nil, err
	}
	body, err := wire.EncodeSetsBlockColumns(resolved, locations, wire.SetsPrecision, redirects, width, columns)
	if err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

// resolveSets reads every unique location through the mesh, merging what
// peers answer, and collects owner redirects when the client navigates itself.
// The locations are read by up to batchParallelism workers; the first failure
// cancels the others and fails the batch.
func (n *Node) resolveSets(parent context.Context, collection string, locations []string, deep bool, via Visited, refresh bool, withRedirects bool, options *ReadOptions) (map[string]*Set, []string, []wire.SetsRedirect, error) {
	if n.mesh == nil {
		return nil, nil, nil, ErrNotReady
	}
	if err := parent.Err(); err != nil {
		return nil, nil, nil, err
	}
	if len(locations) > wire.MaxReadLocations {
		return nil, nil, nil, fmt.Errorf("read batch exceeds %d locations", wire.MaxReadLocations)
	}
	locations = uniqueLocations(locations)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	batch := setsBatch{n: n, collection: collection, deep: deep, via: via, refresh: refresh, withRedirects: withRedirects, options: options,
		resolved: make(map[string]*Set, len(locations))}
	var firstErr error
	var fail sync.Once
	var wg sync.WaitGroup
	jobs := make(chan string)
	for range min(batchParallelism, len(locations)) {
		wg.Go(func() {
			for loc := range jobs {
				if err := batch.resolve(ctx, loc); err != nil {
					fail.Do(func() { firstErr = err; cancel() })
					return
				}
			}
		})
	}
	dispatchLocations(ctx, jobs, locations)
	wg.Wait()
	if firstErr != nil {
		return nil, nil, nil, firstErr
	}
	if err := parent.Err(); err != nil {
		return nil, nil, nil, err
	}
	return batch.resolved, locations, batch.redirects, nil
}

// uniqueLocations trims the locations and keeps the first occurrence of each
// non-empty one, in order.
func uniqueLocations(locations []string) []string {
	unique := make([]string, 0, len(locations))
	seen := make(map[string]bool, len(locations))
	for _, loc := range locations {
		loc = strings.TrimSpace(loc)
		if loc != "" && !seen[loc] {
			seen[loc] = true
			unique = append(unique, loc)
		}
	}
	return unique
}

// dispatchLocations hands the locations to the workers until ctx ends, then
// closes jobs.
func dispatchLocations(ctx context.Context, jobs chan<- string, locations []string) {
	defer close(jobs)
	for _, loc := range locations {
		select {
		case jobs <- loc:
		case <-ctx.Done():
			return
		}
	}
}

// setsBatch is one resolveSets: the read policy shared by its workers and,
// under mu, what they have resolved.
type setsBatch struct {
	n             *Node
	collection    string
	deep          bool
	via           Visited
	refresh       bool
	withRedirects bool
	options       *ReadOptions

	mu        sync.Mutex
	resolved  map[string]*Set
	redirects []wire.SetsRedirect
}

// resolve reads one location, with the explicit read policy when the batch
// has one, and keeps its set or, without a set, the redirect to its owner.
func (b *setsBatch) resolve(ctx context.Context, loc string) error {
	var contact Contact
	var set *Set
	var err error
	if b.options == nil {
		contact, set, err = b.n.GetSetContext(ctx, b.collection, loc, b.deep, b.via, b.refresh)
	} else {
		var result ReadResult
		result, err = b.n.ReadContext(ctx, b.collection, loc, *b.options)
		contact, set = result.Contact, result.Set
	}
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if set != nil {
		b.resolved[loc] = set
		return nil
	}
	remote := b.n.remote(contact)
	if contact != nil && contact != b.n && contactDialable(contact) {
		remote = true
	}
	if b.withRedirects && remote && contact != nil {
		b.redirects = append(b.redirects, wire.SetsRedirect{Location: loc, Name: contact.Name(), IP: contact.IP(), Port: contact.Port()})
	}
	return nil
}
