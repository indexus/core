// Package mesh exchanges local rights and bytes without a metadata quorum.
package mesh

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

const (
	mirrorBatchZones     = 512
	freezeBacklogFloor   = 256
	freezeBacklogDivisor = 2
	handoffGrainZones    = 512
	maxChildRebuilds     = 32
)

type Service struct {
	controlMu     sync.Mutex
	controlClosed bool
	membership    *membershipWire
	Store         *storage.Store
	Self          causal.Endpoint
	VNode         tree.VNodeDescriptor
	Peers         Transport
	// Activated, when set, runs right after this vnode publishes its own
	// activation. A join keeps acquiring regions after that point, so a node
	// caching a routing view refreshes it here rather than when Join returns.
	Activated func()
	// Prevent overlapping retries of this receiver's join, without serializing
	// other machines or independent donor authority tokens.
	join sync.Mutex
	move sync.Mutex
	// Join and concurrent install-zone handlers can both publish this disk's
	// first activation. Publish that local membership announcement once.
	activationMu sync.Mutex
	// One successor per token; independent certified regions share only the
	// store's short transactional writer lock, not a network-length mutex.
	handoffRights rightGates
	// grain sizes the next handoff grain, in KiB, from how long the last
	// regional freezes lasted (GRAIN-1). Nil until the first grain.
	grainOnce sync.Once
	grain     *adaptiveUnit
	coldMu    sync.Mutex
	// jobs runs each donor handoff once, in the background (HANDOFF-STREAM).
	jobs handoffJobs
	// WriteRate, when positive, caps the writes this vnode applies per second
	// (CAP-2, experiments only).
	WriteRate float64
	writeCap  writeCap
	// routes are the entry hints of WRITE-HINT-1, under routeMu (a leaf:
	// never held across a call).
	routeMu sync.Mutex
	routes  zoneRoutes
	// regions are the regional hints of ROUTE-REGION-1, under routeMu.
	regions regionRoutes
	// Demand counts the writes this vnode applied or refused at its cap: its
	// write load, the scale-up signal that does not depend on the host.
	Demand atomic.Int64
	// Writes measures write demand on arrival at this vnode, before service:
	// arrivals as holder, writes in flight and how they ended (WriteDemand).
	Writes WriteDemand
	// Wanted, when set, reports that this vnode still wants a peer (its
	// autoscale was under pressure recently). A new joiner started for an
	// autoscale target (Request.Spawned) is refused otherwise (ErrNotNeeded).
	Wanted func() bool
	// Heat, when set, is how written a zone has been lately: a granular handoff
	// copies and flips its most written units first (FLIP-UNIT).
	Heat func(tree.Key) int64
	// FlipUnitZones and FlipUnitsInFlight bound each unit of a handoff: zones
	// per unit and units side by side. Zero adapts unit size smoothly around
	// flipUnitHoldGoal (4..2048 zones, starting at 16) with 1 unit in flight.
	FlipUnitZones, FlipUnitsInFlight int
	// Spawned marks this node's own join requests (see Request.Spawned).
	Spawned bool
	// ReplicaMachine names a holder's machine for R(z); nil uses the host's
	// address without its port (machineOf).
	ReplicaMachine func(causal.Endpoint) string
	// ReplicateWrites makes a write wait, at most ackTimeout, for one other
	// member of its R(z) to hold it, and say so in Durability (ACK-1).
	ReplicateWrites bool
	// SuspectTimeout is the silence after which a replica enters fallback for
	// an owner (T_suspect, default 3s matching convergent.DefaultConfig).
	SuspectTimeout time.Duration
	// Now, when set, overrides time.Now for deterministic suspicion tests.
	Now           func() time.Time
	ownerHeardMu  sync.Mutex
	ownerHeard    map[string]time.Time
	shadowAckedMu sync.Mutex
	shadowAcked   map[string]map[string]uint64
	// cutoverMu guards active per-zone freeze barriers so writes hitting
	// ErrFrozen during a local flipUnit wait briefly and forward cleanly
	// instead of returning an immediate 503.
	cutoverMu      sync.Mutex
	cutovers       map[tree.Key]chan struct{}
	regionCutovers map[causal.TransferID]regionCutover
	// installedMu tracks recently installed zones until their first local
	// write ACK (metrics.Handoffs.InstallToFirstWrite). installedCount keeps
	// steady-state writes lock-free when no zones are pending observation.
	installedCount atomic.Int64
	installedMu    sync.Mutex
	installedAt    map[tree.Key]time.Time
}

// Close cancels workers before the durable store is closed. Fences survive it.
func (s *Service) Close() {
	s.jobs.close()
	s.controlMu.Lock()
	s.controlClosed = true
	w := s.membership
	s.controlMu.Unlock()
	if w != nil {
		w.close()
	}
}
func (s *Service) call(ctx context.Context, host string, r Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if s.Peers == nil {
		return Response{}, storage.ErrNotReady
	}
	out, err := s.Peers.Exchange(ctx, host, r)
	out.AppliedLocally = false
	return out, err
}

// endpointCall follows address updates of the same durable disk. A different
// owner cannot answer another disk's ticket namespace; only an explicit drain
// receipt can make its old issued transfer obsolete.
func (s *Service) endpointCall(ctx context.Context, to causal.Endpoint, r Request) (Response, error) {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return Response{}, err
	}
	if resolved, e := routing.Resolve(to); e == nil && resolved.Owner == to.Owner {
		to = resolved
	}
	return s.call(ctx, to.Host, r)
}
func (s *Service) Exchange(ctx context.Context, host string, r Request) (Response, error) {
	return s.call(ctx, host, r)
}
func (s *Service) Write(ctx context.Context, id string, record storage.Record) (Response, error) {
	return s.Handle(ctx, Request{Kind: "write", RequestID: id, Record: &record})
}
func (s *Service) Query(ctx context.Context) ([]storage.Snapshot, error) {
	return s.query(ctx, Request{Kind: "inventory"})
}
func (s *Service) Reserve(plan protocol.TopologyPlan) error { return s.Store.ReserveSpawns(plan.Add) }
func (s *Service) PendingSpawnNames() ([]string, error)     { return s.Store.PendingSpawnNames() }
