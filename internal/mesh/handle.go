package mesh

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Handle serves one mesh request. It is a table: one line per kind, the work
// in the handler it names. A request without a field its kind requires is
// refused first (causal.ErrConflict, see required).
func (s *Service) Handle(ctx context.Context, r Request) (out Response, err error) {
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = r.validate(); err != nil {
		return out, err
	}
	switch r.Kind {
	// membership and routing
	case KindMembership:
		return s.handleMembership(ctx, r)
	case "directory":
		return out, s.Store.MergeDirectory(r.Directory)
	case KindRights:
		return s.handleRights(r)
	case "move-host":
		return out, s.MoveTo(ctx, r.Target)

	// data
	case "write":
		return s.handleWrite(ctx, r)
	case KindInventoryPrefix, "inventory":
		return s.handleInventory(ctx, r)
	case KindSummaryContribute, KindSummaryGet:
		return s.handleSummaryRouting(r)
	case KindAdmitCollection, KindGetCollection:
		return s.handleCollection(r, out)

	// transfer, in protocol order
	case "handoff":
		return s.handleHandoff(ctx, r)
	case "admit":
		return s.handleAdmit(r)
	case "cancel":
		return out, s.Store.CancelCausal(*r.Cession)
	case "preload":
		out.Progress, err = s.preload(ctx, *r.Transfer, r.WarmPage)
		return out, err
	case "mirror":
		if len(r.Batch) > 0 {
			return out, s.Store.MirrorBatch(r.Operation.String(), r.Batch)
		}
		return out, s.Store.Mirror(r.Operation.String(), r.Zone, r.Mutations)
	case "stage":
		return out, s.Store.StageCausal(*r.Transfer)
	case "stage-zone":
		return out, s.handleStageZones(r)
	case "install":
		return out, s.handleInstall(r)
	case "install-zone":
		return out, s.handleInstallZones(r)

	// cold storage, shadows, fallback
	case KindColdUsage, KindColdPin, KindColdCheck, KindColdRelease, KindColdWitness:
		return s.handleCold(r)
	case KindShadowSeqs, KindShadowPreload, KindShadowMirror:
		return s.handleShadow(r)
	case KindFallbackWrite, KindFallbackRepair, KindOwnerHeartbeat, KindDegradedRead:
		return s.handleFallback(r)
	default:
		return out, fmt.Errorf("unknown mesh operation %q", r.Kind)
	}
}

func (s *Service) handleMembership(ctx context.Context, r Request) (out Response, err error) {
	reply, err := s.membershipTransport().receive(ctx, r.Owner.Host, *r.Membership)
	out.Membership = &reply
	return out, err
}

// handleRights answers with this disk's rights in a region, its endpoint and
// the part of its read directory that intersects the region.
func (s *Service) handleRights(r Request) (out Response, err error) {
	if out.Rights, err = s.Store.Rights(r.Region); err != nil {
		return out, err
	}
	if out.Endpoint, err = s.Store.Endpoint(s.Self.Host); err != nil {
		return out, err
	}
	refs, err := s.Store.ReadDirectory()
	if err != nil {
		return out, err
	}
	for _, ref := range refs {
		if part, ok := ref.Region.Intersect(r.Region); ok {
			ref.Region = part
			out.Directory = append(out.Directory, ref)
		}
	}
	return out, nil
}

// handleSummaryRouting is the mesh half of a summary request: it refuses a
// caller whose routing digest differs from this disk's.
func (s *Service) handleSummaryRouting(r Request) (out Response, err error) {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return out, err
	}
	out.RoutingDigest = routing.RoutingDigest()
	if r.RoutingDigest != (causal.Digest{}) && r.RoutingDigest != out.RoutingDigest {
		return out, causal.ErrDependency
	}
	return out, nil
}

func (s *Service) handleInventory(ctx context.Context, r Request) (out Response, err error) {
	if r.Target.Valid() && r.Target.Owner != s.Self.Owner {
		return out, causal.ErrConflict
	}
	var query *tree.ZoneKey
	if r.Kind == KindInventoryPrefix {
		query = &r.Zone
	}
	var page storage.InventoryPage
	if out.Inventory, page, err = s.Store.CausalInventoryPage(ctx, query, *r.Page); err == nil {
		out.Page = &page
	}
	return out, err
}

// handleHandoff starts or follows a handoff. A spawned join nobody wants any
// more is refused, unless it already started or its target is placed.
func (s *Service) handleHandoff(ctx context.Context, r Request) (out Response, err error) {
	if r.Spawned && s.Wanted != nil && !s.Wanted() && !r.Operation.Valid() {
		s.jobs.mu.Lock()
		started := s.jobs.jobs[handoffKey(r)] != nil
		s.jobs.mu.Unlock()
		routing, err := s.Store.CausalRouting()
		if err != nil {
			return out, err
		}
		current, err := routing.Placement(r.Target.VNode)
		active := err == nil && current.VNode == r.Target.VNode
		if !started && !active {
			return out, ErrNotNeeded
		}
	}
	return out, s.handoffAsync(ctx, r, handoffWait)
}

// handleAdmit reserves an incoming cession. For a physical move of this
// vnode, the existing logical contact is preserved before the admission is
// acknowledged: a constant-size fact that survives receipt GC and restart.
func (s *Service) handleAdmit(r Request) (out Response, err error) {
	c, err := s.Store.ReserveCausal(*r.Cession)
	if err != nil {
		return out, err
	}
	if c.Source.VNode == c.Target.VNode && c.Target.Owner == s.Self.Owner {
		if err = s.Store.MergeMembership(causal.MembershipDelta{Members: []causal.Member{{VNode: s.VNode, Endpoint: c.Source}}}); err != nil {
			return out, err
		}
	}
	out.Cession = &c
	return out, nil
}

func (s *Service) handleStageZones(r Request) error {
	if len(r.Seals) > 0 {
		return s.Store.CertifyZones(r.Operation, r.Owner.VNode, r.Seals)
	}
	return s.Store.StageZones(r.Operation, r.Transfer.Cession.Source.VNode, r.Transfer.Snapshots)
}

// physicalMove is a transfer between two disks of the same vnode: it changes
// no placement, so no activation is published for it.
func physicalMove(from, to causal.Endpoint) bool {
	return from.VNode == to.VNode && from.Owner != to.Owner
}

func (s *Service) handleInstall(r Request) error {
	if err := s.Store.InstallCausal(*r.Cession); err != nil {
		return err
	}
	metrics.Handoffs.Taken.Add(1)
	return s.activateInstalled(r.Cession.Source, r.Cession.Target)
}

// handleInstallZones certifies the final delta and mounts the lot in one
// durable commit, in whichever form the source sent it.
func (s *Service) handleInstallZones(r Request) (err error) {
	switch {
	case len(r.Seals) > 0:
		err = s.Store.MountZones(r.Operation, r.Owner.VNode, r.Batch, r.Seals, nil, r.ZoneTransfers)
	case r.Transfer != nil && len(r.Transfer.Snapshots) > 0:
		err = s.Store.MountZones(r.Operation, r.Transfer.Cession.Source.VNode, nil, nil, r.Transfer.Snapshots, r.ZoneTransfers)
	default:
		err = s.Store.InstallZones(r.Operation, r.ZoneTransfers)
	}
	if err != nil {
		return err
	}
	s.noteZonesInstalled(r.ZoneTransfers, time.Now())
	return s.activateInstalled(r.ZoneTransfers[0].From, r.ZoneTransfers[0].To)
}

// Both granular and regional rights can make a newly joined vnode useful.
// A physical relocation of the same identity has its own lifecycle.
func (s *Service) activateInstalled(from, to causal.Endpoint) error {
	if physicalMove(from, to) {
		return nil
	}
	if age := metrics.FirstInboundInstall.Mark(); age > 0 {
		slog.Info("first inbound right installed", "after_seconds", age.Seconds())
	}
	return s.publishActivation()
}
