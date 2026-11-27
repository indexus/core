package mesh

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"log/slog"
	"time"
)

func cutoverRefusal(err error) bool {
	return errors.Is(err, storage.ErrFrozen) || errors.Is(err, storage.ErrNotOwner) || errors.Is(err, storage.ErrNotReady)
}

// handleWrite retries once after a local cutover actually completes. Successful
// relays to an already installed lot never wait for regional consolidation.
func (s *Service) handleWrite(ctx context.Context, r Request) (Response, error) {
	if r.Record == nil || r.Hops < 0 {
		return Response{}, storage.ErrNotReady
	}
	for _, owner := range r.Visited {
		if owner == s.Self.Owner {
			return Response{}, storage.ErrNotOwner
		}
	}
	zone, err := tree.NewZoneKey(r.Record.Collection, r.Record.Location)
	if err != nil {
		return Response{}, err
	}
	r.Visited = append(append([]causal.Owner(nil), r.Visited...), s.Self.Owner)
	// Demand is counted here, on arrival, and not after the store served it.
	visit := s.Writes.arrive(r.RequestID, time.Now().UnixNano())
	out, err := s.serveWrite(ctx, r, zone, &visit)
	visit.finish(err)
	return out, err
}

func (s *Service) serveWrite(ctx context.Context, r Request, zone tree.ZoneKey, visit *writeVisit) (Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			visit.comeBack(time.Now().UnixNano())
		}
		out, err := s.writeAttempt(ctx, r, zone, attempt > 0, visit)
		if err == nil || attempt != 0 || !cutoverRefusal(err) || !s.waitCutover(ctx, zone) {
			return out, err
		}
	}
	return Response{}, storage.ErrNotReady
}

func (s *Service) writeAttempt(ctx context.Context, r Request, zone tree.ZoneKey, afterCutover bool, visit *writeVisit) (out Response, err error) {
	// Quiesce only this lot before preparing its MVCC fence. Writers already
	// inside Apply drain normally; new arrivals cannot invalidate every CAS.
	// Once ceded, forward immediately even if the install ACK is still lost.
	if done := s.lotCutoverSignal(zone); done != nil {
		holder, e := s.Store.ZoneHolder(zone)
		if e != nil {
			return out, e
		}
		if holder.Valid() && holder.Owner != s.Self.Owner {
			return s.forwardWrite(ctx, zone, holder, r, afterCutover, visit)
		}
		if !awaitCutover(ctx, done) {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, storage.ErrFrozen
		}
	}
	capped := s.WriteRate > 0
	if capped && !s.writeCap.take(s.WriteRate, time.Now()) {
		holder, e := s.Store.ZoneHolder(zone)
		if e != nil {
			return out, e
		}
		if holder.Valid() && holder.Owner != s.Self.Owner {
			return s.forwardWrite(ctx, zone, holder, r, afterCutover, visit)
		}
		s.Demand.Add(1)
		return out, storage.ErrWriteOverload
	}
	release := func() {
		if capped {
			capped = false
			s.writeCap.refund()
		}
	}
	defer release()
	out.Mutation, out.Previous, out.Applied, err = s.Store.ApplyRecordContext(ctx, r.RequestID, *r.Record)
	if cutoverRefusal(err) {
		holder, e := s.Store.ZoneHolder(zone)
		if e != nil {
			return out, e
		}
		if holder.Owner == s.Self.Owner && errors.Is(err, storage.ErrFrozen) {
			return out, err
		}
		release()
		return s.forwardWrite(ctx, zone, holder, r, afterCutover, visit)
	}
	if err != nil {
		return out, err
	}
	capped = false
	if out.Applied {
		if age := metrics.FirstAppliedWrite.Mark(); age > 0 {
			slog.Info("first write applied", "after_seconds", age.Seconds())
		}
		metrics.Writes.Applied.Add(1)
		s.Writes.Applied.Add(1)
		s.observeInstalledFirstWrite(*r.Record)
	}
	s.Demand.Add(1)
	metrics.Writes.Hops[min(r.Hops, len(metrics.Writes.Hops)-1)].Add(1)
	out.AppliedBy, err = s.Store.Endpoint(s.Self.Host)
	if err != nil {
		return out, err
	}
	// The applier advertises its installed piece, never a cell guessed by an
	// entry from membership. This also teaches routes on locally applied ACKs.
	region := s.Store.WriteRegionHint(zone.ID)
	out.AppliedRegion = &region
	out.AppliedLocally = out.Applied
	if s.ReplicateWrites {
		out.Durability = s.replicateWrite(ctx, zone, out.Mutation.Seq)
	}
	return out, nil
}

func (s *Service) forwardWrite(ctx context.Context, zone tree.ZoneKey, holder causal.Endpoint, r Request, afterCutover bool, visit *writeVisit) (Response, error) {
	visit.forward()
	forwarded := r
	if afterCutover && holder.Owner != s.Self.Owner {
		// The destination was visited before its permission became installed.
		// Revisit only that destination after this disk's barrier completed; keep
		// this disk in Visited so an unsuccessful retry cannot bounce back here.
		forwarded.Visited = make([]causal.Owner, 0, len(r.Visited))
		for _, owner := range r.Visited {
			if owner != holder.Owner {
				forwarded.Visited = append(forwarded.Visited, owner)
			}
		}
	}
	forwarded.Hops++
	metrics.Writes.Relayed.Add(1)
	return s.relayWrite(ctx, zone, holder, r.Hops == 0, forwarded)
}

// relayWrite uses current successor pointers and recent ACKs as hints. Every
// endpoint still checks its local installed right; a finite visited set makes
// stale routing terminate without a cluster-size protocol ceiling.
//
// The candidates are tried in order, each owner once; the first ACK wins and
// teaches the entry its routes. A refusal that another endpoint cannot lift
// (frozen, overload, capacity) ends the relay.
func (s *Service) relayWrite(ctx context.Context, z tree.ZoneKey, holder causal.Endpoint, entry bool, request Request) (Response, error) {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return Response{}, err
	}
	candidates, hint := s.writeCandidates(routing, z, holder, entry, request.Iterative)
	seen := map[causal.Owner]bool{}
	for _, owner := range request.Visited {
		seen[owner] = true
	}
	var out Response
	var failures []error
	for _, target := range candidates {
		if !target.Valid() || seen[target.Owner] {
			continue
		}
		seen[target.Owner] = true
		if out, err = s.callWrite(ctx, target, request); err == nil {
			if entry {
				s.learnWriteRoute(z, hint, out)
			}
			return out, nil
		}
		s.routeMu.Lock()
		s.routes.drop(z.Key(), target)
		s.routeMu.Unlock()
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, storage.ErrFrozen) || errors.Is(err, storage.ErrWriteOverload) || errors.Is(err, storage.ErrDataCapacity) {
			return out, err
		}
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		return out, storage.ErrNotOwner
	}
	return out, errors.Join(failures...)
}

// regionHint is the regional hint a relay used, to drop it if it was wrong.
type regionHint struct {
	region tree.Region
	holder causal.Endpoint
}

// writeCandidates lists where a write may be applied, most likely first:
//
//  1. entry only: the last applier of this exact zone;
//  2. entry only: the disk last seen serving the most specific region around
//     it (ROUTE-REGION-1);
//  3. the holder the caller resolved;
//  4. entry only, not iterative: the genesis holder, the durable predecessor
//     of the initial address space, whose successor directory bridges a
//     placement member that has not acquired the address yet.
//
// Each is a hint checked by the target's own installed right. An endpoint
// the routing cannot resolve is left out.
func (s *Service) writeCandidates(routing storage.RoutingSnapshot, z tree.ZoneKey, holder causal.Endpoint, entry, iterative bool) (candidates []causal.Endpoint, hint regionHint) {
	add := func(endpoint causal.Endpoint) bool {
		resolved, err := routing.Resolve(endpoint)
		if err == nil {
			candidates = append(candidates, resolved)
		}
		return err == nil
	}
	if entry {
		s.routeMu.Lock()
		exact, known := s.routes.get(z.Key())
		region, regional, wide := s.regions.get(z.ID)
		s.routeMu.Unlock()
		if known {
			add(exact)
		}
		if wide && add(regional) {
			hint = regionHint{region: region, holder: regional}
		}
	}
	if holder.Valid() {
		add(holder)
	}
	if entry && !iterative {
		_, genesis := routing.Identity()
		add(genesis.Holder)
	}
	return candidates, hint
}

// learnWriteRoute keeps what an ACK teaches the entry: the zone's applier,
// its region, and that a regional hint naming another disk was wrong.
func (s *Service) learnWriteRoute(z tree.ZoneKey, hint regionHint, out Response) {
	if !out.AppliedBy.Valid() {
		return
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.routes.put(z.Key(), out.AppliedBy)
	if hint.holder.Valid() && hint.holder.Owner != out.AppliedBy.Owner {
		s.regions.drop(hint.region, hint.holder)
	}
	if out.AppliedRegion != nil {
		s.regions.put(*out.AppliedRegion, out.AppliedBy)
	}
}

// A known successor may be reached between its donor's fence and its install.
// Retry that same endpoint, not unrelated members. Request IDs and installed
// rights make replay safe; transport failures and overload do not trigger it.
func (s *Service) callWrite(ctx context.Context, target causal.Endpoint, request Request) (Response, error) {
	out, err := s.call(ctx, target.Host, request)
	if request.Hops == 1 && !request.Iterative {
		for _, delay := range []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond} {
			if !cutoverRefusal(err) {
				break
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-timer.C:
			}
			out, err = s.call(ctx, target.Host, request)
		}
	}
	return out, err
}
