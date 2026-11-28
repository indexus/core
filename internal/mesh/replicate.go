package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Convergent replication, step 3, sync half (docs/design/
// convergent-replication-wiring.md): an owner brings the shadow copies of its
// zones up to date on the other members of each zone's R(z). The same pieces
// as a handoff copy: a first snapshot, then the zone's mutations after the
// sequence the member holds, batched across zones. It changes no
// acknowledgement yet, and grants nothing: shadow copies are never read.
const (
	KindShadowSeqs    = "shadow-seqs"
	KindShadowPreload = "shadow-preload"
	KindShadowMirror  = "shadow-mirror"
)

// machineOf names the advertised host independently of scheme and slot port.
// Tests and local agents may advertise a bare host. Unknown URI forms cannot
// establish an independent replication domain.
func machineOf(h causal.Endpoint) string {
	raw := h.Host
	if ip, err := netip.ParseAddr(raw); err == nil {
		return ip.Unmap().String()
	}
	if !strings.Contains(raw, "://") {
		if strings.ContainsAny(raw, "/?#@") {
			return ""
		}
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return ""
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// shadowBatchZones bounds one shadow preload/mirror round to match the live
// write batch size (256), balancing fsync count against transaction duration.
const shadowBatchZones = 256

func (s *Service) shadowKnownSeq(member causal.Endpoint, zoneKey string) uint64 {
	s.shadowAckedMu.Lock()
	defer s.shadowAckedMu.Unlock()
	if s.shadowAcked == nil {
		return 0
	}
	return s.shadowAcked[member.Storage][zoneKey]
}

func (s *Service) markShadowSynced(member causal.Endpoint, progress map[string]uint64) {
	if len(progress) == 0 || member.Storage == "" {
		return
	}
	s.shadowAckedMu.Lock()
	defer s.shadowAckedMu.Unlock()
	if s.shadowAcked == nil {
		s.shadowAcked = map[string]map[string]uint64{}
	}
	m := s.shadowAcked[member.Storage]
	if m == nil {
		m = map[string]uint64{}
		s.shadowAcked[member.Storage] = m
	}
	for k, seq := range progress {
		if seq > m[k] {
			m[k] = seq
		}
	}
}

func (s *Service) resetShadowCache(member causal.Endpoint) {
	s.shadowAckedMu.Lock()
	delete(s.shadowAcked, member.Storage)
	s.shadowAckedMu.Unlock()
}

// Replicate syncs every zone this disk may write to the other members of its
// R(z). One failing member does not hold back the others; their errors are
// joined.
func (s *Service) Replicate(ctx context.Context) error {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return err
	}
	owned, err := s.Store.OwnedSeqs()
	if err != nil {
		return err
	}
	byMember := map[causal.Endpoint][]storage.ZoneSeq{}
	var maxLag uint64
	for _, zs := range owned {
		zk := zoneKeyOf(zs.Zone)
		for _, member := range s.replicaPeers(routing, zs.Zone) {
			known := s.shadowKnownSeq(member, zk)
			if zs.Seq > known {
				if lag := zs.Seq - known; lag > maxLag {
					maxLag = lag
				}
				byMember[member] = append(byMember[member], zs)
			}
		}
	}
	metrics.Replication.LagSeq.Store(int64(maxLag))
	metrics.Replication.ShadowBytes.Store(s.Store.ShadowBytes())
	var failures []error
	for member, zones := range byMember {
		catalogShipped := false
		for start := 0; start < len(zones); start += shadowBatchZones {
			if err := s.syncShadowsBatch(ctx, member, zones[start:min(start+shadowBatchZones, len(zones))], &catalogShipped); err != nil {
				failures = append(failures, fmt.Errorf("replicate to %s: %w", member.VNode, err))
				break
			}
		}
	}
	return errors.Join(failures...)
}

// syncShadows asks member which of zones it holds and through which
// sequence, sends a first copy of the others and the mutations it lacks.
func (s *Service) syncShadows(ctx context.Context, member causal.Endpoint, zones []storage.ZoneSeq) error {
	catalogShipped := false
	return s.syncShadowsBatch(ctx, member, zones, &catalogShipped)
}

func (s *Service) syncShadowsBatch(ctx context.Context, member causal.Endpoint, zones []storage.ZoneSeq, catalogShipped *bool) error {
	var unknown []storage.ZoneSeq
	var needSnap []tree.ZoneKey
	var ranges []storage.MutationRange
	for _, zs := range zones {
		if known := s.shadowKnownSeq(member, zoneKeyOf(zs.Zone)); known > 0 {
			if known < zs.Seq {
				ranges = append(ranges, storage.MutationRange{Zone: zs.Zone, After: known, Through: zs.Seq})
			}
			continue
		}
		// PreloadShadow is idempotent (stageOnce preserves existing progress),
		// so a brand-new single-mutation zone skips the shadow-seqs round-trip.
		if zs.Seq == 1 {
			needSnap = append(needSnap, zs.Zone)
			continue
		}
		unknown = append(unknown, zs)
	}
	if len(unknown) > 0 {
		keys := make([]tree.ZoneKey, len(unknown))
		for i, zs := range unknown {
			keys[i] = zs.Zone
		}
		held, err := s.call(ctx, member.Host, Request{Kind: KindShadowSeqs, Owner: s.Self, Zones: keys})
		if err != nil {
			return err
		}
		s.markShadowSynced(member, held.Progress)
		for _, zs := range unknown {
			seq, ok := held.Progress[zoneKeyOf(zs.Zone)]
			switch {
			case !ok:
				needSnap = append(needSnap, zs.Zone)
			case seq < zs.Seq:
				ranges = append(ranges, storage.MutationRange{Zone: zs.Zone, After: seq, Through: zs.Seq})
			}
		}
	}
	if len(needSnap) > 0 {
		snaps, err := s.Store.Snapshots(ctx, needSnap, 0)
		if err != nil {
			return err
		}
		if !*catalogShipped {
			if err := s.shipCatalog(ctx, member); err != nil {
				return err
			}
			*catalogShipped = true
		}
		res, err := s.call(ctx, member.Host, Request{Kind: KindShadowPreload, Owner: s.Self, Transfer: &storage.CausalTransfer{Snapshots: snaps}})
		if err != nil {
			return err
		}
		s.markShadowSynced(member, res.Progress)
	}
	if len(ranges) == 0 {
		return nil
	}
	batch, err := s.Store.MutationsBatch(ranges)
	if err != nil {
		return err
	}
	if _, err := s.call(ctx, member.Host, Request{Kind: KindShadowMirror, Owner: s.Self, Batch: batch}); err != nil {
		if errors.Is(err, storage.ErrSequence) || errors.Is(err, storage.ErrNotReady) {
			s.resetShadowCache(member)
		}
		return err
	}
	synced := make(map[string]uint64, len(ranges))
	for _, r := range ranges {
		synced[zoneKeyOf(r.Zone)] = r.Through
	}
	s.markShadowSynced(member, synced)
	return nil
}

// handleShadow serves the member side of Replicate.
func (s *Service) handleShadow(request Request) (Response, error) {
	var out Response
	owner := request.Owner
	if owner.Owner == s.Self.Owner {
		return out, causal.ErrConflict
	}
	s.RecordOwnerHeard(owner)
	var err error
	switch request.Kind {
	case KindShadowSeqs:
		out.Progress, err = s.Store.ShadowSeqs(owner, request.Zones)
	case KindShadowPreload:
		if request.Transfer == nil {
			return out, causal.ErrConflict
		}
		out.Progress, err = s.Store.PreloadShadow(owner, request.Transfer.Snapshots)
	case KindShadowMirror:
		err = s.Store.MirrorShadow(owner, request.Batch)
	}
	return out, err
}

// Durability of an acknowledged write (ACK-1).
const (
	DurabilityReplicated = "replicated" // on the owner and one other member of R(z)
	DurabilityLocal      = "local"      // on the owner only: no member answered in time
)

// ackTimeout bounds the wait for one member's copy of a write (T_ack).
var ackTimeout = time.Second

// replicateWrite brings the other members of zone's R(z) to the write's
// sequence and returns DurabilityReplicated as soon as one holds it, or
// DurabilityLocal when none did within ackTimeout or when R(z) has no other
// machine. A member that answered later, or not at all, is caught up by
// Replicate.
func (s *Service) replicateWrite(ctx context.Context, zone tree.ZoneKey, seq uint64) string {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return DurabilityLocal
	}
	members := s.replicaPeers(routing, zone)
	if len(members) == 0 {
		return DurabilityLocal
	}
	ctx, cancel := context.WithTimeout(ctx, ackTimeout)
	defer cancel()
	held := make(chan bool, len(members))
	for _, member := range members {
		go func() { held <- s.syncShadows(ctx, member, []storage.ZoneSeq{{Zone: zone, Seq: seq}}) == nil }()
	}
	for range members {
		select {
		case ok := <-held:
			if ok {
				metrics.Replication.AcksReplicated.Add(1)
				return DurabilityReplicated
			}
		case <-ctx.Done():
			metrics.Replication.AcksLocal.Add(1)
			return DurabilityLocal
		}
	}
	metrics.Replication.AcksLocal.Add(1)
	return DurabilityLocal
}
