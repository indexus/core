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

// Writer routes from membership and recent ACKs. Neither grants authority;
// only the destination's installed local rights can acknowledge a write.
type Writer struct {
	peers                              Transport
	seeds                              []string
	control                            *membershipWire
	mu                                 sync.Mutex
	mergeMu                            sync.Mutex
	membership                         *causal.Membership
	root                               tree.VNodeDescriptor
	genesis                            causal.Genesis
	direct, forward, refresh, requests uint64
	routes                             zoneRoutes
}
type WriterStats struct {
	Requests, Direct, Forwarded, Refreshes uint64
	Active                                 int
	Digest                                 causal.Digest
}

func NewWriter(ctx context.Context, peers Transport, seeds []string) (*Writer, error) {
	if peers == nil || len(seeds) == 0 {
		return nil, fmt.Errorf("writer requires transport and at least one seed")
	}
	w := &Writer{peers: peers, seeds: append([]string(nil), seeds...)}
	w.control = newMembershipWire(w.snapshot, w.mergeMembership)
	if err := w.refreshView(ctx); err != nil {
		w.control.close()
		return nil, err
	}
	return w, nil
}
func (w *Writer) Close() { w.control.close() }
func (w *Writer) snapshot() (localMembership, error) {
	w.mu.Lock()
	m, root, genesis := w.membership, w.root, w.genesis
	w.mu.Unlock()
	if m == nil {
		return localMembership{}, nil
	}
	return localMembership{membershipKey{root, genesis, m.Digest()}, m.Export}, nil
}
func (w *Writer) mergeMembership(e MembershipEnvelope) error {
	w.mergeMu.Lock()
	defer w.mergeMu.Unlock()
	w.mu.Lock()
	base, root, genesis := w.membership, w.root, w.genesis
	w.mu.Unlock()
	var m *causal.Membership
	if base == nil {
		var err error
		m, err = causal.NewMembership(e.Root, e.Genesis)
		if err != nil {
			return err
		}
	} else {
		if root != e.Root || genesis != e.Genesis {
			return storage.ErrWrongCluster
		}
		m = base.Clone()
	}
	if err := m.Merge(e.Delta); err != nil {
		return err
	}
	w.mu.Lock()
	w.membership, w.root, w.genesis = m, e.Root, e.Genesis
	w.mu.Unlock()
	return nil
}
func (w *Writer) sendMembership(ctx context.Context, host string, p MembershipPacket) (MembershipReply, error) {
	out, err := w.peers.Exchange(ctx, host, Request{Kind: KindMembership, Membership: &p})
	if err != nil {
		return MembershipReply{}, err
	}
	if out.Membership == nil {
		return MembershipReply{}, causal.ErrDependency
	}
	return *out.Membership, nil
}
func (w *Writer) refreshView(ctx context.Context) error {
	var failures []error
	for _, host := range w.seeds {
		if err := w.control.exchange(ctx, host, w.sendMembership); err != nil {
			if errors.Is(err, storage.ErrWrongCluster) {
				return err
			}
			failures = append(failures, err)
			continue
		}
		w.mu.Lock()
		ready := w.membership != nil
		if ready {
			w.refresh++
		}
		w.mu.Unlock()
		if ready {
			return nil
		}
	}
	if len(failures) == 0 {
		return causal.ErrDependency
	}
	return errors.Join(failures...)
}
func (w *Writer) targetLocked(record storage.Record) (causal.Endpoint, error) {
	if w.membership == nil {
		return causal.Endpoint{}, causal.ErrDependency
	}
	zone, err := tree.NewZoneKey(record.Collection, record.Location)
	if err != nil {
		return causal.Endpoint{}, err
	}
	if hint, ok := w.routes.get(zone.Key()); ok {
		return w.membership.Resolve(hint)
	}
	return w.membership.Route(zone.ID)
}
func (w *Writer) rememberRoute(record storage.Record, endpoint causal.Endpoint) {
	zone, err := tree.NewZoneKey(record.Collection, record.Location)
	if err == nil {
		w.routes.put(zone.Key(), endpoint)
	}
}

// Write never exchanges membership inline. On stale routing it tries each known
// endpoint at most once; background anti-entropy repairs hints independently.
func (w *Writer) Write(ctx context.Context, id string, record storage.Record) (_ Response, writeErr error) {
	w.mu.Lock()
	w.requests++
	target, err := w.targetLocked(record)
	m := w.membership
	w.mu.Unlock()
	if err != nil {
		return Response{}, err
	}
	refreshPeer := ""
	defer func() {
		if writeErr != nil && refreshPeer != "" {
			w.control.schedule(refreshPeer, w.sendMembership)
		}
	}()
	candidates := []causal.Endpoint{target}
	seen := map[causal.Owner]bool{}
	var last Response
	var failures []error
	discovered := false
	for {
		if len(candidates) == 0 {
			if discovered {
				break
			}
			discovered = true
			for _, member := range m.Export().Members {
				if !member.Retired {
					candidates = append(candidates, member.Endpoint)
				}
			}
			if len(candidates) == 0 {
				break
			}
		}
		to := candidates[0]
		candidates = candidates[1:]
		if seen[to.Owner] {
			continue
		}
		seen[to.Owner] = true
		if err = ctx.Err(); err != nil {
			return last, err
		}
		last, err = w.peers.Exchange(ctx, to.Host, Request{Kind: "write", RequestID: id, Record: &record, Iterative: true})
		if err == nil {
			w.mu.Lock()
			w.rememberRoute(record, last.AppliedBy)
			if last.AppliedBy.Owner == to.Owner {
				w.direct++
			} else {
				w.forward++
			}
			w.mu.Unlock()
			return last, nil
		}
		zone, e := tree.NewZoneKey(record.Collection, record.Location)
		if e == nil {
			w.mu.Lock()
			w.routes.drop(zone.Key(), to)
			w.mu.Unlock()
		}
		if !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, causal.ErrDependency) {
			return last, err
		}
		if refreshPeer == "" {
			refreshPeer = to.Host
		}
		failures = append(failures, err)
	}
	return last, errors.Join(failures...)
}
func (w *Writer) Stats() WriterStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := WriterStats{Requests: w.requests, Direct: w.direct, Forwarded: w.forward, Refreshes: w.refresh}
	if w.membership != nil {
		out.Active = w.membership.Current().Len()
		out.Digest = w.membership.Digest()
	}
	return out
}
