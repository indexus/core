package mesh

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/replication/convergent"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

const (
	KindFallbackWrite  = "fallback-write"
	KindFallbackRepair = "fallback-repair"
	KindOwnerHeartbeat = "owner-heartbeat"
	KindDegradedRead   = "degraded-read"
)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) suspectTimeout() time.Duration {
	if s.SuspectTimeout > 0 {
		return s.SuspectTimeout
	}
	return time.Duration(convergent.DefaultConfig().Suspect) * time.Millisecond
}

// RecordOwnerHeard notes that owner was observed alive at the current local
// clock (heartbeat or shadow replication message).
func (s *Service) RecordOwnerHeard(owner causal.Endpoint) {
	if owner.Storage == "" {
		return
	}
	s.ownerHeardMu.Lock()
	if s.ownerHeard == nil {
		s.ownerHeard = map[string]time.Time{}
	}
	s.ownerHeard[owner.Storage] = s.now()
	s.ownerHeardMu.Unlock()
}

// OwnerSuspected reports whether owner has been silent for at least
// SuspectTimeout since its last observed heartbeat or shadow sync.
func (s *Service) OwnerSuspected(owner causal.Endpoint) bool {
	if owner.Storage == "" {
		return false
	}
	s.ownerHeardMu.Lock()
	heard, ok := s.ownerHeard[owner.Storage]
	s.ownerHeardMu.Unlock()
	if !ok {
		return true
	}
	return s.now().Sub(heard) >= s.suspectTimeout()
}

// SendHeartbeats sends an owner-heartbeat to every other member of R(z) for
// the zones this disk currently owns.
func (s *Service) SendHeartbeats(ctx context.Context) error {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return err
	}
	owned, err := s.Store.OwnedSeqs()
	if err != nil {
		return err
	}
	machine := s.ReplicaMachine
	if machine == nil {
		machine = machineOf
	}
	seen := map[causal.Endpoint]bool{}
	var failures []error
	for _, zs := range owned {
		for _, member := range replicas(routing, zs.Zone, machine) {
			if member.Owner == s.Self.Owner || seen[member] {
				continue
			}
			seen[member] = true
			if _, err := s.call(ctx, member.Host, Request{Kind: KindOwnerHeartbeat, Owner: s.Self}); err != nil {
				failures = append(failures, fmt.Errorf("heartbeat to %s: %w", member.VNode, err))
			}
		}
	}
	return errors.Join(failures...)
}

func toConvergentVersion(e storage.FallbackEntry) convergent.Version {
	return convergent.Version{
		ID:      e.Record.ID,
		Ver:     e.Ver,
		Deleted: e.Record.Deleted,
		Digest:  e.Digest,
		Dot: convergent.Dot{
			Incarnation: convergent.Incarnation(e.DotIncarnation),
			Counter:     e.DotCounter,
		},
	}
}

func fallbackWins(a, b storage.FallbackEntry) bool {
	return convergent.Wins(toConvergentVersion(a), toConvergentVersion(b))
}

func (s *Service) handleFallback(request Request) (Response, error) {
	var out Response
	switch request.Kind {
	case KindOwnerHeartbeat:
		if request.Owner.Owner == s.Self.Owner {
			return out, causal.ErrConflict
		}
		s.RecordOwnerHeard(request.Owner)
		return out, nil
	case KindFallbackWrite:
		if request.Owner.Valid() && !s.OwnerSuspected(request.Owner) {
			return out, convergent.ErrRedirect
		}
		z, err := tree.NewZoneKey(request.Record.Collection, request.Record.Location)
		if err != nil {
			return out, err
		}
		var baseSeq uint64
		if request.Owner.Valid() && request.Owner.Owner != s.Self.Owner {
			if seq, ok, err := s.Store.ShadowSeq(request.Owner, z); err == nil && ok {
				baseSeq = seq
			}
		}
		entry, err := s.Store.ApplyFallback(request.RequestID, *request.Record, baseSeq, 0)
		if err != nil {
			return out, err
		}
		metrics.Replication.FallbackWrites.Add(1)
		out.FallbackEntry = &entry
		return out, nil
	case KindDegradedRead:
		rec, ok, err := s.Store.DegradedRead(request.Owner, request.Zone, request.RecordID)
		if err != nil {
			return out, err
		}
		metrics.Replication.DegradedReads.Add(1)
		out.Degraded = true
		if ok {
			out.Record = &rec
		}
		return out, nil
	case KindFallbackRepair:
		applied, err := s.Store.MergeFallback(request.Zone, request.FallbackEntries, fallbackWins)
		if applied > 0 {
			metrics.Replication.FallbackRepairs.Add(int64(applied))
		}
		out.AppliedCount = applied
		return out, err
	}
	return out, causal.ErrConflict
}

// RepairFallback ships pending fallback writes for z to its owner and clears
// them locally once the owner has durably merged them.
func (s *Service) RepairFallback(ctx context.Context, owner causal.Endpoint, z tree.ZoneKey) error {
	entries, err := s.Store.FallbackEntries(z)
	if err != nil || len(entries) == 0 {
		return err
	}
	if _, err := s.call(ctx, owner.Host, Request{Kind: KindFallbackRepair, Zone: z, FallbackEntries: entries}); err != nil {
		return err
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.RequestID
	}
	return s.Store.AckFallback(z, ids)
}

// RepairAllFallback ships every pending fallback zone on this disk to its
// current owner from the local P2P routing view. One unreachable owner does
// not hold back the others; their errors are joined.
func (s *Service) RepairAllFallback(ctx context.Context) error {
	zones, err := s.Store.FallbackZones()
	if err != nil || len(zones) == 0 {
		return err
	}
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return err
	}
	var failures []error
	for _, z := range zones {
		owner, err := routing.Route(z.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("route fallback %s/%s: %w", z.Collection, z.Location, err))
			continue
		}
		if owner.Owner == s.Self.Owner {
			continue
		}
		if err := s.RepairFallback(ctx, owner, z); err != nil {
			failures = append(failures, fmt.Errorf("repair fallback %s/%s to %s: %w", z.Collection, z.Location, owner.VNode, err))
		}
	}
	return errors.Join(failures...)
}
