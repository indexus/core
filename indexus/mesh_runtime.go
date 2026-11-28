package indexus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/summary"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type meshRuntime struct {
	readSlots         chan struct{}
	reads             readCache
	compaction        atomic.Pointer[CausalCompactionConfig]
	replicate         atomic.Bool // convergent replication sync (EnableReplication)
	compactionRunning atomic.Bool
	lastWrite         atomic.Int64
	demandAt          time.Time // last autoscale tick: write demand rate
	demandSeen        int64
	installedLots     atomic.Int64 // successful inbound lot installs since the last resource sample
	lastColdAttempt   atomic.Int64
	gossipRound       atomic.Uint64
	writes            atomic.Int64 // client writes seen, for the COLD-2 cool meter
	cool              coolMeter
	lastColdResult    atomic.Pointer[causalCompactionResult]
	service           *mesh.Service
	summary           *summary.Tree
}

// DefaultCopySlots is the per-host source-copy concurrency used by nodes
// sharing a copy-budget directory when no positive override is configured.
func DefaultCopySlots() int { return mesh.DefaultCopySlots() }

var foldTrace = os.Getenv("INDEXUS_FOLD_TRACE") == "1"

// SummaryMetrics reports summary pushes and repair polls for GET /metrics.
func (n *Node) SummaryMetrics() map[string]any {
	s := n.SummaryStats()
	return map[string]any{"flushes": s.Flushes, "flush_max_ms": s.FlushMaxMS, "flush_last_ms": s.FlushLastMS,
		"export_keys_built": s.ExportKeysBuilt, "export_keys_extended": s.ExportKeysExtended, "export_keys_reused": s.ExportKeysReused, "export_keys_continued": s.ExportKeysContinued, "export_keys_oversized": s.ExportKeysOversized, "pending_edges": s.PendingEdges, "push_batches": s.PushBatches, "push_failures": s.PushFailures,
		// A repair response can change several contributions or their availability.
		"poll_calls": s.PollCalls, "poll_failures": s.PollFailures, "poll_changes": s.PollChanged,
		"value_ms": s.ValueMS, "push_ms": s.PushMS, "valued": s.Valued, "unchanged": s.Unchanged}
}

// SummaryStats is a snapshot of the derived-summary protocol counters.
type SummaryStats = summary.Stats

func (n *Node) SummaryStats() SummaryStats {
	if n.mesh == nil {
		return SummaryStats{}
	}
	return n.mesh.summary.Stats()
}

func (n *Node) EnableCausalCluster(service *mesh.Service) error {
	if service == nil || service.Store == nil || service.Peers == nil || service.VNode.Name != n.Name() || n.mesh != nil {
		return fmt.Errorf("invalid causal runtime configuration")
	}

	snapshot, err := service.Store.CausalRouting()
	if err != nil {
		return err
	}
	epoch, err := service.Store.NextSummaryEpoch()
	if err != nil {
		return err
	}
	runtime := &meshRuntime{readSlots: make(chan struct{}, batchParallelism), service: service}
	runtime.summary = summary.New(service, epoch, summary.Hooks{
		LastWrite:   runtime.lastWrite.Load,
		Quiet:       n.inventoryQuiet,
		RefreshView: n.meshRefreshViewIfChanged,
		Reconciled:  n.meshScheduleCompaction,
	})
	n.mesh = runtime
	// A donor refuses a new joiner once its autoscale has cooled down.
	service.Wanted = func() bool { return n.autoscale == nil || n.autoscale.Wanted() }
	service.Heat = n.zoneWriteHeat
	// Join publishes the activation before it finishes acquiring the cell; the
	// writes applied meanwhile must see this vnode as owner in the view, or
	// their summary propagation is routed by a view that predates the join.
	// The regions installed before that publication were not writable yet
	// (no active membership), so the install-time fold saw an empty
	// inventory: fold again now that they are.
	service.Activated = func() {
		_ = n.meshRefreshView()
		if foldTrace {
			fmt.Fprintf(os.Stderr, "fold-trigger node=%s activation\n", n.Name())
		}
		_, _ = n.mesh.summary.Fold(context.Background(), true)
	}
	return n.meshApplyView(snapshot, true)
}

// MeshData serves one mesh request on this node. Steps, in order:
//  1. count a write;
//  2. let the mesh service handle the request;
//  3. refresh the advisory view, as the kind of request needs;
//  4. serve the summary part of a summary request;
//  5. fold a zone that was just installed;
//  6. account an applied record: heat, autoscale and, when it was applied
//     here, the summary;
//  7. stamp a summary answer with the routing digest.
//
// The first error wins and later steps that only make sense on success are
// skipped; the response built so far is returned with it.
func (n *Node) MeshData(ctx context.Context, request mesh.Request) (mesh.Response, error) {
	if n.mesh == nil {
		return mesh.Response{}, storage.ErrNotReady
	}
	if request.Kind == "write" {
		n.mesh.lastWrite.Store(time.Now().UnixNano())
		n.mesh.writes.Add(1)
	}
	// Writes go straight to Handle: it applies when this disk holds durable
	// writable authority and otherwise forwards along the authority chain.
	// Pre-routing by the XOR view here would, during a handoff, send a write to
	// the new owner before its grant is installed; that node forwards it back
	// along authority, the view forwards it again, and the write bounces for up
	// to 64 hops while the region moves. appliedHere marks the one invocation
	// that applied the record; a relayed response that names this vnode as the
	// applier must not fold the delta again.
	response, err := n.mesh.service.Handle(ctx, request)
	if err == nil && request.Kind == "install-zone" {
		n.mesh.installedLots.Add(1)
	}
	appliedHere := err == nil && request.Record != nil && response.AppliedLocally
	refreshErr := n.meshRefreshViewFor(request.Kind)
	if err == nil {
		err = refreshErr
	}
	if err == nil {
		err = n.meshServeSummary(request, &response)
	}
	if err == nil && (request.Kind == "install" || request.Kind == mesh.KindColdWitness) {
		n.meshFoldInstalled()
	}
	if response.Applied && request.Record != nil {
		if e := n.meshAccountWrite(*request.Record, response.Previous, appliedHere); err == nil {
			err = e
		}
	}
	if request.Kind == mesh.KindSummaryContribute || request.Kind == mesh.KindSummaryGet {
		err = n.meshStampRoutingDigest(request, &response, err)
	}
	return response, err
}

// meshRefreshViewFor refreshes the advisory view before a derived request's
// values are interpreted.
//
// Derived requests refresh their advisory view before interpreting values.
// A write ACK never refreshes all contacts or repushes the inventory: the
// local installed right already decided the commit. Its notification
// (meshAccountWrite) uses the current cell directly from the immutable
// placement index.
func (n *Node) meshRefreshViewFor(kind string) error {
	switch kind {
	case "write":
		return nil
	case mesh.KindSummaryContribute, mesh.KindSummaryGet:
		return n.meshSyncViewIfChanged()
	default:
		return n.meshRefreshViewIfChanged()
	}
}

// meshServeSummary accepts the contributions of a summary push, or fills the
// answer to a summary poll. Other requests are left as they are.
func (n *Node) meshServeSummary(request mesh.Request, response *mesh.Response) error {
	switch request.Kind {
	case mesh.KindSummaryContribute:
		if len(request.Contributions) > 0 {
			return n.mesh.summary.AcceptBatch(request.Contributions)
		}
	case mesh.KindSummaryGet:
		response.SummaryStatus = n.mesh.summary.PollStatus()
		response.Contributions, response.SummaryMore = n.mesh.summary.ExportPage(request.Zone.Collection, request.Locations, request.TargetVNode.Name, request.SummaryPage)
	}
	return nil
}

// meshFoldInstalled folds what an install or a cold witness just made local.
//
// The certified bytes are now local and writable: fold them like any
// other durable commit instead of waiting for the maintenance tick. The
// install itself is durable; a fold refused by a still-frozen sibling
// transfer is simply retried by the next Update.
func (n *Node) meshFoldInstalled() {
	if foldTrace {
		fmt.Fprintf(os.Stderr, "fold-trigger node=%s install\n", n.Name())
	}
	if _, foldErr := n.mesh.summary.Fold(context.Background(), true); foldErr != nil && foldTrace {
		fmt.Fprintf(os.Stderr, "fold-error node=%s install: %v\n", n.Name(), foldErr)
	}
}

// meshAccountWrite counts an applied record in its zone's heat and in the
// autoscale window. When this invocation applied it (appliedHere), the
// summary also folds its delta, under the vnode's current cell; the error is
// that of reading the cell.
func (n *Node) meshAccountWrite(record storage.Record, previous *storage.Record, appliedHere bool) error {
	if !appliedHere {
		return nil
	}
	key := Key{Collection: record.Collection, Location: record.Location}
	n.heatZone(key)
	if n.autoscale != nil {
		n.autoscale.RecordInsertAt(key.Collection, key.Location)
	}
	routing, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return err
	}
	cell, err := routing.Cell(n.Name())
	if err != nil {
		return err
	}
	n.mesh.summary.OnLocalWrite(record, previous, cell)
	return nil
}

func (n *Node) heatZone(key Key) {
	n.heatMu.Lock()
	defer n.heatMu.Unlock()
	n.zoneHeat.observe(key, time.Now())
}

// meshStampRoutingDigest stamps a summary answer with this node's routing
// digest and returns the error the request ends with. A requester on another
// routing gets no contributions and, when err is nil, ErrDependency; a failure
// to read the routing replaces err.
func (n *Node) meshStampRoutingDigest(request mesh.Request, response *mesh.Response, err error) error {
	routing, e := n.mesh.service.Store.CausalRouting()
	if e != nil {
		return e
	}
	response.RoutingDigest = routing.RoutingDigest()
	if request.RoutingDigest == (causal.Digest{}) || request.RoutingDigest == response.RoutingDigest {
		return err
	}
	response.Contributions = nil
	if err == nil {
		err = causal.ErrDependency
	}
	return err
}

// meshRefreshViewIfChanged refreshes membership or local physical placement
// from one durable snapshot. Unchanged topology needs no view reconstruction.
func (n *Node) meshRefreshViewIfChanged() error {
	return n.meshApplyViewIfChanged(true)
}

// meshSyncViewIfChanged updates membership and placement without re-arming
// every remote-parent edge. Summary RPC
// handlers use this so piggybacked topology does not amplify flush work.
func (n *Node) meshSyncViewIfChanged() error {
	return n.meshApplyViewIfChanged(false)
}

func (n *Node) meshApplyViewIfChanged(repushBoundaries bool) error {
	snapshot, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return err
	}
	holder, placementErr := snapshot.Placement(n.Name())
	local := placementErr == nil && holder.Storage == n.mesh.service.Self.Storage
	if n.mesh.summary.ViewIs(snapshot.Digest(), local) {
		return nil
	}
	return n.meshApplyView(snapshot, repushBoundaries)
}

func (n *Node) meshRefreshView() error {
	snapshot, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return err
	}
	return n.meshApplyView(snapshot, true)
}

func (n *Node) meshApplyView(snapshot storage.RoutingSnapshot, repushBoundaries bool) error {
	current := snapshot.Current()
	holder, placementErr := snapshot.Placement(n.Name())
	n.mesh.summary.SetView(current, placementErr == nil && holder.Storage == n.mesh.service.Self.Storage, repushBoundaries)
	for _, vnode := range current.Active {
		if vnode.Name == n.Name() {
			continue
		}
		holder, err := snapshot.Placement(vnode.Name)
		if err != nil {
			return err
		}
		contact, err := n.contactForPlacement(holder.VNode, holder.Host)
		if err != nil {
			return err
		}
		n.register([]Contact{contact})
	}
	return nil
}
func (n *Node) JoinCausalCluster(ctx context.Context) error {
	if n.mesh == nil {
		return storage.ErrNotReady
	}
	if err := n.mesh.service.Join(ctx); err != nil {
		return err
	}
	return n.meshRefreshView()
}

// Each maintenance phase has its own deadline. With one 30 s budget for the
// whole round, a slow gossip partner spent it in Sync, and Resume and
// Rebalance then ran with an expired context every round: a frozen handoff
// never moved while any partner was slow (REM-88).
const (
	maintainSyncBudget  = 10 * time.Second
	maintainPhaseBudget = 30 * time.Second
)

// maintainPhases runs sync, then every phase, each under its own deadline
// derived from parent, and joins their errors: one failing phase does not
// skip the next.
func maintainPhases(parent context.Context, sync func(context.Context) error, phases ...func(context.Context) error) error {
	return maintainPhasesWithin(parent, maintainSyncBudget, maintainPhaseBudget, sync, phases...)
}

// maintainPhasesWithin is maintainPhases with the given budgets.
func maintainPhasesWithin(parent context.Context, syncBudget, phaseBudget time.Duration, sync func(context.Context) error, phases ...func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, syncBudget)
	err := sync(ctx)
	cancel()
	for _, phase := range phases {
		ctx, cancel := context.WithTimeout(parent, phaseBudget)
		err = errors.Join(err, phase(ctx))
		cancel()
	}
	return err
}

func (n *Node) meshMaintainContext(parent context.Context) error {
	// Bounded anti-entropy: one partner per XOR bucket, rotating (GOS-1).
	peers, err := n.mesh.service.GossipTargets(n.mesh.gossipRound.Add(1))
	if err != nil {
		return err
	}
	roundStarted := time.Now()
	defer func() {
		metrics.Gossip.Rounds.Add(1)
		metrics.Gossip.Partners.Store(int64(len(peers)))
		metrics.Gossip.RoundNanos.Store(int64(time.Since(roundStarted)))
	}()
	service := n.mesh.service
	phases := []func(context.Context) error{service.Resume, service.Rebalance}
	if n.mesh.replicate.Load() {
		phases = append(phases, service.SendHeartbeats, service.Replicate, service.RepairAllFallback)
	}
	err = maintainPhases(parent, func(ctx context.Context) error { return service.Sync(ctx, peers) }, phases...)
	err = errors.Join(err, n.meshRefreshView())
	// Resume and Rebalance may have installed or released regions locally.
	if _, foldErr := n.mesh.summary.Fold(parent, false); foldErr != nil {
		err = errors.Join(err, foldErr)
	}
	if errors.Is(err, causal.ErrConflict) {
		return errors.Join(storage.ErrConflict, err)
	}
	return err
}
func (n *Node) meshWrite(item *Item, root string, deleted bool) (WriteRoute, error) {
	if err := checkKey(item, root); err != nil {
		return WriteRoute{}, err
	}
	if n.full.Load() || n.autoscale.ClientWritesBlocked() {
		return WriteRoute{}, ErrNodeFull
	}
	request := item.RequestID
	if request == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return WriteRoute{}, err
		}
		request = hex.EncodeToString(id[:])
	}
	record := storage.Record{Collection: item.Collection, Location: item.Location, ID: item.Id, Metrics: append([]float64(nil), item.Metrics...), Deleted: deleted, Payload: append([]byte(nil), item.Payload...)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A local API write carries no membership state. In particular, do
	// not rebuild a bundle here before the mesh's compact write path starts.
	metrics.Writes.Received.Add(1)
	started := time.Now()
	response, err := n.MeshData(ctx, mesh.Request{Kind: "write", RequestID: request, Record: &record})
	if err != nil {
		metrics.Writes.Refused.Add(1)
		return WriteRoute{}, err
	}
	metrics.Writes.Acknowledged.Add(1)
	metrics.Writes.Latency.Observe(time.Since(started))
	route := WriteRoute{Owner: response.AppliedBy.Host}
	if response.AppliedRegion != nil {
		route.Cell = EncodeCell(*response.AppliedRegion)
	}
	return route, nil
}

// EncodeCell writes a region as its mask and its value in hexadecimal,
// "mask/value": a key's zone ID is in the region when ID & mask == value.
func EncodeCell(r tree.Region) string {
	return hex.EncodeToString(r.Mask[:]) + "/" + hex.EncodeToString(r.Value[:])
}

// DecodeCell reads EncodeCell.
func DecodeCell(s string) (tree.Region, error) {
	var r tree.Region
	mask, value, ok := strings.Cut(s, "/")
	m, e1 := hex.DecodeString(mask)
	v, e2 := hex.DecodeString(value)
	if !ok || e1 != nil || e2 != nil || len(m) != len(r.Mask) || len(v) != len(r.Value) {
		return r, fmt.Errorf("cell %q: want mask/value, %d bytes each in hexadecimal", s, len(r.Mask))
	}
	copy(r.Mask[:], m)
	copy(r.Value[:], v)
	if !r.Valid() {
		return r, fmt.Errorf("cell %q: value outside its mask", s)
	}
	return r, nil
}
func (n *Node) meshGet(ctx context.Context, collection, location string, deep, refresh bool) (Contact, *Set, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	zone, err := NewZoneKey(collection, location)
	if err != nil {
		return nil, nil, err
	}
	// A map drill asks for one prefix at a time. The derived summary already
	// holds that prefix's direct children, so answer from memory. Falling
	// through scans every durable record on the holder — once per location.
	contact, local, err := n.meshReadRoute(zone)
	if err != nil {
		return nil, nil, err
	}
	if !deep && !local {
		return contact, nil, nil
	}
	// Only a local derived read needs to publish pending ancestry first.
	// Strict/remote reads use durable QueryPrefix below: draining unrelated
	// ingress writes adds work without improving that authoritative result.
	if local && !refresh {
		if set, ok := n.mesh.summary.Set(zone.Collection, zone.Location); ok {
			return contact, set, nil
		}
		if n.mesh.summary.Ensure(ctx) {
			if set, ok := n.mesh.summary.Set(zone.Collection, zone.Location); ok {
				return contact, set, nil
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	inventory, err := n.mesh.service.QueryPrefixMetrics(ctx, collection, location)
	if err != nil {
		return nil, nil, err
	}
	// Query may have learned a newer placement. Return a current route hint
	// through the same resolver used for shallow and cached reads.
	contact, _, err = n.meshReadRoute(zone)
	if err != nil {
		return nil, nil, err
	}
	described, err := n.mesh.service.Store.CollectionOf(collection)
	if err != nil {
		return nil, nil, err
	}
	groups := summary.NewGroupsOf(collection, location, described)
	for _, snap := range inventory {
		if snap.Zone.Collection != collection || !strings.HasPrefix(snap.Zone.Location, groups.Path) {
			continue
		}
		for _, record := range snap.Records {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if record.Deleted || record.Location != snap.Zone.Location {
				continue
			}
			groups.AddRecord(record.Location, record.ID, record.Weight, record.Metrics)
		}
	}
	return contact, groups.Set(), nil
}

// meshReadRoute is the holder that answers a read of zone, and whether it is
// this node.
func (n *Node) meshReadRoute(zone tree.ZoneKey) (Contact, bool, error) {
	snapshot, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return nil, false, err
	}
	holder, err := snapshot.Route(zone.ID)
	if err != nil {
		return nil, false, err
	}
	contact, err := n.contactForPlacement(holder.VNode, holder.Host)
	local := holder.VNode == n.Name() && holder.Storage == n.mesh.service.Self.Storage
	return contact, local, err
}

func (n *Node) durableStore() *storage.Store {
	if n.mesh != nil {
		return n.mesh.service.Store
	}

	return nil
}
func (n *Node) reserveScalePlan(plan TopologyPlan) error { return n.mesh.service.Reserve(plan) }
func (n *Node) contactForPlacement(vnodeName, endpointText string) (Contact, error) {
	if vnodeName == n.Name() && (n.mesh == nil || endpointText == n.mesh.service.Self.Host) {
		return n, nil
	}
	endpoint, err := url.Parse(endpointText)
	if err != nil {
		return nil, err
	}
	host, portText, err := net.SplitHostPort(endpoint.Host)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, err
	}
	contact := n.newContact(vnodeName, map[string]any{host: nil}, port)
	if c, ok := contact.(interface{ SetScheme(string) }); ok {
		c.SetScheme(endpoint.Scheme)
	}
	return contact, nil
}

// Endpoint is this vnode's installed runtime address, including its transport.
func (n *Node) Endpoint() string {
	if n.mesh == nil {
		return ""
	}
	return n.mesh.service.Self.Host
}
