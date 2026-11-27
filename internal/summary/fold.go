package summary

import (
	"context"
	"errors"
	"maps"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

const (
	summaryRemoteWindow = time.Second
	summaryRetryAfter   = 3 * time.Second
	summaryRetryCap     = 30 * time.Second
	summaryRPCTimeout   = 5 * time.Second
	// summaryFlushChunk bounds how many ready edges one FlushPendingEdges pass
	// values and pushes before committing ACKs and yielding pushMu. On a 40-node
	// split with 37k pending edges, unbounded single-wave flushes held pushMu
	// for 50+ seconds before retiring a single edge.
	summaryFlushChunk = 2048
	// A poll page holds at most this many boundaries (about 350 bytes each in
	// JSON, far below the transport's 32 MiB cap), within summaryPollTimeout.
	summaryPollPage    = 8192
	summaryPollTimeout = 10 * time.Second
	// Boundary reconcile must outpace handoff storms: a one-minute cadence
	// left root lag stranded for the whole drain after soft topology races.
	ReconcileEvery = 5 * time.Second
	// A poll that changes nothing while no write and no contribution arrived
	// doubles the wait before the next one, up to this cap. Each poll exports
	// every boundary of the peer (87 000 on a 300 000-record node): at a 5 s
	// cadence that alone kept an idle node at 5 to 10 % of a core.
	ReconcileIdleMax         = 2 * time.Minute
	InventoryEvery           = 5 * time.Minute
	summaryContributionBatch = 512
	// FullTicksAfterChange is how many consecutive Update ticks run the
	// boundary reconciliation after an authority change.
	FullTicksAfterChange = 5
)

// summaryPollWindow is how many remote hosts one steady-state boundary
// reconcile asks (SUM-2). Child push is the primary path; this poll only
// repairs a lost push, so it rotates over the hosts instead of asking all of
// them every tick, which cost N·(N−1) calls per tick across the network.
func summaryPollWindow(hosts int) int {
	return min(hosts, max(4, bits.Len(uint(hosts))))
}

// rotateHosts returns window consecutive hosts starting at offset, wrapping:
// successive offsets advanced by window cover every host in ⌈N/window⌉ ticks.
func rotateHosts(hosts []string, offset uint64, window int) []string {
	out := make([]string, 0, window)
	for i := 0; i < window; i++ {
		out = append(out, hosts[(offset+uint64(i))%uint64(len(hosts))])
	}
	return out
}

// summaryContribution is one child's absolute versioned total as retained by
// its parent. Replacing it is the only allowed update; a bare delta is not.
type summaryContribution struct {
	incomplete bool
	epoch      uint64
	holder     causal.Endpoint
	value      *wire.Abelian
	revision   uint64
	generation uint64
	// exact is the one location of every record below the child, or "".
	exact string
}

// Edge is a boundary this node pushes to the owner of its parent.
type Edge struct {
	// Pending is the contribution not yet acknowledged by the parent's owner.
	Pending     *mesh.Contribution
	lastAttempt time.Time
	failures    int
	// retryAt holds a failed edge back until its backoff expires, so one
	// unreachable parent does not make every later flush resend it.
	retryAt time.Time
	// Stale: the value is read from the store when the edge is sent.
	Stale bool
	// sent is the last contribution the parent's owner acknowledged, and
	// sentTo that owner's host. A flush does not send again a value the same
	// owner already holds: after a split every boundary is queued, and most
	// values have not changed (2026-11-20: 60 000 edges, flushes of 130 to
	// 190 s spent sending). If the parent moved to a new host, sentTo !=
	// holder.Host forces a send to the new owner even when the value matches.
	sent   *mesh.Contribution
	sentTo string
}

// sameValue is whether b carries what a carries, its revision and view generation aside.
func sameValue(a, b *mesh.Contribution) bool {
	return a != nil && b != nil && a.Holder == b.Holder && a.Epoch == b.Epoch &&
		a.Incomplete == b.Incomplete && a.Count == b.Count && a.Exact == b.Exact && a.Digest == b.Digest && slices.Equal(a.Metrics, b.Metrics)
}

func coalesceDecision(lastAttempt, now time.Time, window time.Duration) (sendNow bool, wait time.Duration) {
	if lastAttempt.IsZero() || !now.Before(lastAttempt.Add(window)) {
		return true, 0
	}
	return false, lastAttempt.Add(window).Sub(now)
}

func contributionFromWire(in mesh.Contribution) summaryContribution {
	return summaryContribution{
		incomplete: in.Incomplete, epoch: in.Epoch, revision: in.Revision,
		generation: in.Generation,
		holder:     in.Holder,
		value:      wire.NewAbelian(in.Count, append([]float64(nil), in.Metrics...)),
		exact:      exactBelow(in.Location, in.Exact),
	}
}

// exactBelow keeps a peer's exact location only if it lies below location.
func exactBelow(location, exact string) string {
	if !strings.HasPrefix(exact, summaryPath(location)) {
		return ""
	}
	return exact
}

// itemCount is how many items a record of this weight counts for.
func itemCount(weight float64) int { return int(math.Round(storage.WeightOf(weight))) }

func (t *Tree) OwnsHere(collection, location string) bool {
	cached := t.topology.Load()
	if cached == nil || !cached.local {
		return false
	}
	zone, err := tree.NewZoneKey(collection, location)
	if err != nil {
		return false
	}
	owner, _ := cached.view.OwnerName(zone.ID)
	return owner == t.name
}

func (t *Tree) ready() bool {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	return !t.LastFold.IsZero()
}

func (t *Tree) Ensure(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if t.ready() {
		return true
	}
	if t.RefreshMu.LockContext(ctx) != nil {
		return false
	}
	defer t.RefreshMu.Unlock()
	if t.ready() {
		return true
	}
	if _, err := t.Fold(ctx, true); err != nil {
		return false
	}
	return t.ready()
}

func (t *Tree) ChildHolderCurrent(collection, location string, holder causal.Endpoint) bool {
	zone, err := tree.NewZoneKey(collection, location)
	if err != nil {
		return false
	}
	snapshot, err := t.service.Store.CausalRouting()
	if err != nil {
		return false
	}
	current, err := snapshot.Route(zone.ID)
	return err == nil && current == holder
}

// armFlushLocked makes sure one flush runs at or before at. The node keeps
// a single timer for all its edges: a timer per edge made a fold of N
// boundaries start N flushes, each of which walked all N edges.
func (t *Tree) armFlushLocked(at time.Time) {
	if t.flushTimer != nil && !t.flushAt.After(at) {
		return
	}
	if t.flushTimer != nil {
		t.flushTimer.Stop()
	}
	t.flushAt = at
	var timer *time.Timer
	timer = time.AfterFunc(time.Until(at), func() {
		t.Mu.Lock()
		if t.flushTimer != timer {
			t.Mu.Unlock()
			return
		}
		t.flushTimer = nil
		t.flushAt = time.Time{}
		t.Mu.Unlock()
		t.sendEdge()
	})
	t.flushTimer = timer
}

// StopFlushLocked cancels the scheduled flush (tests, shutdown).
func (t *Tree) StopFlushLocked() {
	if t.flushTimer != nil {
		t.flushTimer.Stop()
		t.flushTimer = nil
		t.flushAt = time.Time{}
	}
}

type pendingSummaryEdge struct {
	key          tree.Key
	contribution mesh.Contribution
	sent         *mesh.Contribution // edge.sent when the edge was collected
	sentTo, host string
}

func (t *Tree) sendEdge() {
	t.pushMu.Lock()
	if t.pushing {
		t.pushAgain = true
		t.pushMu.Unlock()
		return
	}
	t.pushing = true
	t.pushMu.Unlock()

	for {
		t.FlushPendingEdges()
		t.pushMu.Lock()
		if !t.pushAgain {
			t.pushing = false
			t.pushMu.Unlock()
			return
		}
		t.pushAgain = false
		t.pushMu.Unlock()
	}
}

// FlushPendingEdges sends the parent of every due edge its current value, in
// four steps that never hold t.Mu across a store read or a push:
//
//	dueEdges     which edges are due (one hold of t.Mu)
//	valueEdges   value each stale edge (t.Mu per edge, store reads outside)
//	pushEdges    resolve each parent's holder and push, one host at a time
//	settleEdges  record what was sent and arm the retries (one hold of t.Mu)
func (t *Tree) FlushPendingEdges() {
	began := time.Now()
	defer t.observeFlush(began)

	now := time.Now()
	keys, moreReady := t.dueEdges(now)
	if len(keys) == 0 {
		return
	}
	pending := t.valueEdges(keys, now)
	t.stats.valueMS.Add(time.Since(now).Milliseconds())
	if len(pending) == 0 {
		if moreReady {
			t.Mu.Lock()
			t.armFlushLocked(time.Now().Add(time.Millisecond))
			t.Mu.Unlock()
		}
		return
	}
	pushed := time.Now()
	failures := t.pushEdges(pending)
	t.settleEdges(pending, failures, moreReady)
	t.stats.pushMS.Add(time.Since(pushed).Milliseconds())
}

func (t *Tree) observeFlush(began time.Time) {
	elapsed := time.Since(began).Milliseconds()
	t.stats.flushes.Add(1)
	t.stats.flushLastMS.Store(elapsed)
	for {
		seen := t.stats.flushMaxMS.Load()
		if elapsed <= seen || t.stats.flushMaxMS.CompareAndSwap(seen, elapsed) {
			return
		}
	}
}

// dueEdges is the pending edges whose retry time has come, siblings side by
// side, at most summaryFlushChunk of them; more says some were left for the
// next flush. It arms the timer for the earliest edge not due yet.
func (t *Tree) dueEdges(now time.Time) (keys []tree.Key, more bool) {
	t.Mu.Lock()
	t.drainDirtyLocked()
	var earliestRetry time.Time
	for key := range t.pendingEdges {
		edge := t.Edges[key]
		if edge == nil {
			delete(t.pendingEdges, key)
			continue
		}
		if !edge.retryAt.After(now) {
			keys = append(keys, key)
			continue
		}
		if earliestRetry.IsZero() || edge.retryAt.Before(earliestRetry) {
			earliestRetry = edge.retryAt
		}
	}
	if !earliestRetry.IsZero() {
		t.armFlushLocked(earliestRetry)
	}
	t.Mu.Unlock()
	// Siblings under the same (Collection, Parent) follow each other so
	// zoneSums reuses one ZoneChildren scan across all of them even when
	// concurrent refreshLocked calls advance storeGen during a handoff.
	slices.SortFunc(keys, tree.Key.Compare)
	if len(keys) > summaryFlushChunk {
		return keys[:summaryFlushChunk], true
	}
	return keys, false
}

// valueEdges claims and stores each edge under t.Mu, reading its value from
// the store outside the lock. Write notifications use dirtyMu, not t.Mu.
// The sums are closed on return: no version is pinned across the network.
func (t *Tree) valueEdges(keys []tree.Key, now time.Time) []pendingSummaryEdge {
	pending := make([]pendingSummaryEdge, 0, len(keys))
	sums, gen := t.openZoneSums(), uint64(0)
	defer sums.close()
	for _, key := range keys {
		claim, queued := t.claimEdge(key, now, sums, &gen)
		if !queued {
			continue
		}
		var (
			valued *mesh.Contribution
			valErr error
		)
		if claim.stale {
			valued, valErr = t.contributionOf(key, claim, sums)
		}
		if item, send := t.storeEdgeValue(key, now, claim, valued, valErr); send {
			pending = append(pending, item)
		}
	}
	return pending
}

// edgeClaim is what valuing one stale edge needs, copied under t.Mu.
type edgeClaim struct {
	stale                       bool
	incoming                    []incomingEntry
	storeGen, epoch, generation uint64
}

// claimEdge takes the edge's stale mark under t.Mu; queued is false when the
// edge left the queue meanwhile.
func (t *Tree) claimEdge(key tree.Key, now time.Time, sums *zoneSums, gen *uint64) (claim edgeClaim, queued bool) {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	if _, queued := t.pendingEdges[key]; !queued {
		return claim, false
	}
	t.currentLocked(sums, gen)
	edge := t.Edges[key]
	if edge == nil || !edge.Stale || edge.retryAt.After(now) {
		return claim, true
	}
	edge.Stale = false
	claim = edgeClaim{stale: true, incoming: t.snapshotIncomingLocked(key.Collection, key.Location), storeGen: t.storeGen, epoch: t.epoch}
	if v := t.View(); v != nil {
		claim.generation = v.Generation
	}
	return claim, true
}

// contributionOf reads the edge's value outside t.Mu.
func (t *Tree) contributionOf(key tree.Key, claim edgeClaim, sums *zoneSums) (*mesh.Contribution, error) {
	value, exact, err := t.valueWithIncoming(key.Collection, key.Location, sums, claim.incoming)
	endpoint, endpointErr := t.service.Store.Endpoint(t.service.Self.Host)
	if err == nil {
		err = endpointErr
	}
	if err != nil {
		return nil, err
	}
	return &mesh.Contribution{
		Collection: key.Collection,
		Location:   key.Location,
		Epoch:      claim.epoch,
		Generation: claim.generation,
		Holder:     endpoint,
		Count:      value.Count(),
		Metrics:    append([]float64(nil), value.Metrics()...),
		Exact:      exact,
		Digest:     t.descriptionDigest(key.Collection),
	}, nil
}

// storeEdgeValue records the value on the edge under t.Mu and says whether
// the edge has something to send now.
func (t *Tree) storeEdgeValue(key tree.Key, now time.Time, claim edgeClaim, valued *mesh.Contribution, valErr error) (item pendingSummaryEdge, send bool) {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	if _, queued := t.pendingEdges[key]; !queued {
		return item, false
	}
	edge := t.Edges[key]
	if claim.stale && edge != nil {
		if valErr != nil {
			edge.Stale = true
			edge.failures++
			wait := summaryRetryAfter << min(edge.failures-1, 8)
			edge.retryAt = time.Now().Add(min(wait, summaryRetryCap))
			t.armFlushLocked(edge.retryAt)
			t.stats.valued.Add(1)
			return item, false // keep even a first value queued; never send an older one
		} else {
			t.revision++
			valued.Revision = t.revision
			valued.Incomplete = t.exportIncompleteLocked(key.Collection, key.Location)
			edge.Pending = valued
			if t.storeGen != claim.storeGen {
				edge.Stale = true
			}
		}
		t.stats.valued.Add(1)
	}
	if edge == nil || edge.Pending == nil {
		delete(t.pendingEdges, key)
		return item, false
	}
	if edge.retryAt.After(now) {
		// One timer serves every edge: an earlier flush may have
		// replaced the one armed for this retry, so arm it again
		// (2026-11-20: 18 847 edges left pending once writes stopped).
		t.armFlushLocked(edge.retryAt)
		return item, false
	}
	edge.lastAttempt = now
	return pendingSummaryEdge{key: key, contribution: *edge.Pending, sent: edge.sent, sentTo: edge.sentTo}, true
}

// pushEdges delivers every pending edge to the holder of its parent and
// returns the outcome per edge: nil when the holder has the value. It sets
// pending[i].host for an edge whose holder is another host.
func (t *Tree) pushEdges(pending []pendingSummaryEdge) map[tree.Key]error {
	failures := make(map[tree.Key]error, len(pending))
	byHost := t.routeEdges(pending, failures)
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	outcomes := make([]map[tree.Key]error, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i] = t.pushHost(host, byHost[host])
		}()
	}
	wg.Wait()
	for _, outcome := range outcomes {
		maps.Copy(failures, outcome)
	}
	return failures
}

// routeEdges settles in failures the edges that need no push (no route, a
// parent held here, a value the holder already has) and groups the others by
// the host of their parent's holder.
func (t *Tree) routeEdges(pending []pendingSummaryEdge, failures map[tree.Key]error) map[string][]pendingSummaryEdge {
	byHost := map[string][]pendingSummaryEdge{}
	replica, err := t.replica()
	for i := range pending {
		item := &pending[i]
		if err != nil {
			failures[item.key] = err
			continue
		}
		parent := tree.BASE64.Parent(item.contribution.Location)
		holder, resolveErr := resolveHolder(replica, item.contribution.Collection, parent)
		if resolveErr != nil {
			failures[item.key] = resolveErr
			continue
		}
		if holder.Owner == t.service.Self.Owner {
			failures[item.key] = t.AcceptWire(item.contribution)
			continue
		}
		item.host = holder.Host
		if item.sentTo == holder.Host && sameValue(item.sent, &item.contribution) {
			t.stats.unchanged.Add(1)
			failures[item.key] = nil // the owner holds this value already
			continue
		}
		byHost[holder.Host] = append(byHost[holder.Host], *item)
	}
	return byHost
}

// pushHost sends one host its edges in batches and returns the outcome per
// edge. A transport failure fails every edge not sent yet.
func (t *Tree) pushHost(host string, items []pendingSummaryEdge) map[tree.Key]error {
	outcome := make(map[tree.Key]error, len(items))
	for start := 0; start < len(items); start += summaryContributionBatch {
		part := items[start:min(start+summaryContributionBatch, len(items))]
		contributions := make([]mesh.Contribution, len(part))
		for j := range part {
			contributions[j] = part[j].contribution
		}
		batchErr := t.push(host, contributions)
		t.stats.pushBatches.Add(1)
		// NotOwner means at least one edge targeted a parent that already
		// handed off; the sender must re-resolve. It is not a transport
		// failure.
		if batchErr != nil && !errors.Is(batchErr, storage.ErrNotOwner) {
			t.stats.pushFailures.Add(1)
			for _, item := range items[start:] {
				outcome[item.key] = batchErr
			}
			return outcome
		}
		for _, item := range part {
			outcome[item.key] = batchErr
		}
	}
	return outcome
}

// settleEdges records under t.Mu what each push achieved: a failed edge
// backs off, a delivered one remembers what its holder has, and an edge with
// nothing newer to send leaves the queue.
func (t *Tree) settleEdges(pending []pendingSummaryEdge, failures map[tree.Key]error, moreReady bool) {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	for _, item := range pending {
		edge := t.Edges[item.key]
		if edge == nil {
			continue
		}
		if failures[item.key] != nil {
			edge.failures++
			wait := summaryRetryAfter << min(edge.failures-1, 8)
			edge.retryAt = time.Now().Add(min(wait, summaryRetryCap))
			t.armFlushLocked(edge.retryAt)
			continue
		}
		edge.failures = 0
		edge.retryAt = time.Time{}
		sent := item.contribution
		if item.host != "" {
			edge.sent, edge.sentTo = &sent, item.host
		}
		if edge.Pending != nil && edge.Pending.Epoch == sent.Epoch && edge.Pending.Revision == sent.Revision && edge.Pending.Generation == sent.Generation && edge.Pending.Holder == sent.Holder {
			edge.Pending = nil
		}
		// A write during the push marked the edge stale (REM-112): its newer
		// value has not been sent, so the edge stays queued.
		if edge.Pending == nil && !edge.Stale {
			delete(t.pendingEdges, item.key)
			continue
		}
		_, wait := coalesceDecision(edge.lastAttempt, time.Now(), summaryRemoteWindow)
		if wait == 0 {
			wait = time.Millisecond
		}
		t.armFlushLocked(time.Now().Add(wait))
	}
	if moreReady {
		t.armFlushLocked(time.Now().Add(time.Millisecond))
	}
}

func (t *Tree) push(host string, contributions []mesh.Contribution) error {
	if len(contributions) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), summaryRPCTimeout)
	defer cancel()
	_, err := t.service.Exchange(ctx, host, mesh.Request{
		Kind:          mesh.KindSummaryContribute,
		Contributions: contributions,
	})
	return err
}

func (t *Tree) replica() (storage.RoutingSnapshot, error) {
	replica, err := t.service.Store.CausalRouting()
	return replica, err
}

func resolveHolder(replica storage.RoutingSnapshot, collection, location string) (causal.Endpoint, error) {
	zone, err := tree.NewZoneKey(collection, location)
	if err != nil {
		return causal.Endpoint{}, err
	}
	return replica.Route(zone.ID)
}

// flushPending sends every pending edge now. One flush sends them all:
// calling it once per pending key walked the whole edge set once per key.
func (t *Tree) flushPending() {
	t.Mu.Lock()
	t.drainDirtyLocked()
	pending := len(t.pendingEdges)
	t.Mu.Unlock()
	if pending > 0 {
		t.sendEdge()
	}
}

func (t *Tree) summaryDigest() (causal.Digest, error) {
	snapshot, err := t.service.Store.CausalRouting()
	if err != nil {
		return causal.Digest{}, err
	}
	return t.summaryDigestOf(snapshot), nil
}

// Only local transfer steps and routing changes invalidate derived repair.
// An unrelated peer's data certificate is not a new local inventory.
func (t *Tree) summaryDigestOf(snapshot storage.RoutingSnapshot) causal.Digest {
	return causal.Hash(struct {
		Routing       causal.Digest
		LocalTransfer uint64
	}{snapshot.RoutingDigest(), t.service.Store.LocalTransferRevision()})
}

func (t *Tree) Fold(ctx context.Context, force bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Activation, maintenance and first reads can all request a rebuild.
	// Serialize the whole snapshot/fold and recheck whether it is still due
	// after waiting, instead of retaining several inventories concurrently.
	if err := t.InventoryMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer t.InventoryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	snapshot, err := t.service.Store.CausalRouting()
	if err != nil {
		return false, err
	}
	digest := t.summaryDigestOf(snapshot)
	// Capture before the inventory snapshot. A concurrent commit may cause
	// one extra later scan, but can never be acknowledged without being read.
	revision, err := t.service.Store.Revision()
	if err != nil {
		return false, err
	}
	t.Mu.Lock()
	due := force || digest != t.lastFoldDigest || t.LastFold.IsZero() || (revision != t.lastFoldRevision && time.Since(t.LastFold) >= InventoryEvery && t.hooks.Quiet())
	if !due {
		t.Mu.Unlock()
		return false, nil
	}
	t.foldAttempt++
	attempt := t.foldAttempt
	t.Mu.Unlock()
	complete, err := t.FoldLocal(ctx, snapshot.Current())
	t.finishFold(attempt, digest, revision, complete && err == nil)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (t *Tree) finishFold(attempt uint64, digest causal.Digest, revision uint64, complete bool) {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	if attempt != t.foldAttempt {
		return
	}
	if complete {
		t.lastFoldDigest = digest
		t.lastFoldRevision = revision
		t.LastFold = time.Now()
	} else {
		// A stale snapshot is unfinished work, retried on the next tick.
		t.LastFold = time.Time{}
	}
}

func (t *Tree) Reconcile(ctx context.Context, force bool) error {
	defer func() {
		if ctx.Err() == nil {
			t.hooks.Reconciled()
		}
	}()
	if err := t.RefreshMu.LockContext(ctx); err != nil {
		return err
	}
	defer t.RefreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := t.hooks.RefreshView(); err != nil {
		return err
	}
	digest, err := t.summaryDigest()
	if err != nil {
		return err
	}
	t.Mu.Lock()
	if digest != t.lastDigest {
		t.fullTicks = FullTicksAfterChange
	}
	if t.fullTicks > 0 || t.activeLocked() {
		t.PollEvery = ReconcileEvery
	}
	every := max(t.PollEvery, ReconcileEvery)
	// all: every host, on start, on an explicit Update and for a few ticks
	// after an authority change. Otherwise a rotating window (SUM-2).
	all := force || t.Summaries == nil || t.fullTicks > 0 || t.LastReconcile.IsZero()
	full := all || time.Since(t.LastReconcile) >= every
	if t.fullTicks > 0 {
		t.fullTicks--
	}
	changedBefore := t.stats.pollChanged.Load()
	t.Mu.Unlock()

	if _, err := t.Fold(ctx, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.flushPending()
	var failed error
	if full {
		if err := t.PollRemoteHosts(ctx, all); err != nil {
			failed = errors.Join(failed, err)
		} else {
			t.Mu.Lock()
			t.lastDigest = digest
			t.LastReconcile = time.Now()
			if t.stats.pollChanged.Load() != changedBefore {
				t.PollEvery = ReconcileEvery
			} else {
				t.PollEvery = min(2*every, ReconcileIdleMax)
			}
			t.markQuietLocked()
			t.Mu.Unlock()
		}
	}
	return failed
}

// activeLocked reports a write, an accepted contribution or a
// failed push since the last poll: then the next poll keeps the short cadence.
func (t *Tree) activeLocked() bool {
	return t.hooks.LastWrite() > t.quietSince.write || t.stats.accepted.Load() != t.quietSince.accepted || t.stats.pushFailures.Load() != t.quietSince.pushFailures
}

func (t *Tree) markQuietLocked() {
	t.quietSince.write = t.hooks.LastWrite()
	t.quietSince.accepted = t.stats.accepted.Load()
	t.quietSince.pushFailures = t.stats.pushFailures.Load()
}

func (t *Tree) PollRemoteHosts(ctx context.Context, all bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	hosts, err := t.remoteHosts()
	if err != nil {
		return err
	}
	// A retired endpoint is not a permanent cache-readiness dependency.
	live := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		live[host] = true
	}
	t.Mu.Lock()
	for host := range t.peerUnavailable {
		if !live[host] {
			delete(t.peerUnavailable, host)
			delete(t.peerFloor, host)
		}
	}
	t.Mu.Unlock()
	if len(hosts) > 0 {
		window := len(hosts)
		if !all {
			window = summaryPollWindow(len(hosts))
		}
		hosts = rotateHosts(hosts, t.pollRound.Load(), window)
	}
	var failed error
	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return errors.Join(failed, err)
		}
		// Advance only work actually attempted: a cancelled partial window
		// must not skip its suffix forever, nor restart every full poll at zero.
		t.pollRound.Add(1)
		failed = errors.Join(failed, t.PollHost(ctx, host))
	}
	return errors.Join(failed, ctx.Err())
}

// pollResponse observes a versioned status before accepting its newer values.
func (t *Tree) pollResponse(parent context.Context, host string, request mesh.Request) (mesh.Response, error) {
	ctx, cancel := context.WithTimeout(parent, summaryPollTimeout)
	defer cancel()
	response, err := t.service.Exchange(ctx, host, request)
	if err == nil {
		err = ctx.Err() // a late response must not turn cancellation into success
	}
	t.stats.pollCalls.Add(1)
	if err != nil {
		t.stats.pollFailures.Add(1)
		return response, err
	}
	t.Mu.Lock()
	if t.peerUnavailable == nil {
		t.peerUnavailable = map[string]bool{}
	}
	status := response.SummaryStatus
	freshStatus := status != nil && (t.peerFloor[host].Revision == 0 || newerSummaryStatus(t.peerFloor[host], *status))
	wasUnavailable := t.peerUnavailable[host]
	if freshStatus {
		if t.peerFloor == nil {
			t.peerFloor = map[string]mesh.SummaryStatus{}
		}
		t.peerFloor[host] = *status
		t.peerUnavailable[host] = status.Unavailable
	}
	if wasUnavailable != t.peerUnavailable[host] {
		// A status transition changes the completeness of existing exports,
		// even when no new data boundary was created or returned.
		for key := range t.Edges {
			t.queueEdgeLocked(key)
		}
	}
	if t.peerUnavailable[host] {
		for key, in := range t.incoming {
			if in.holder.Host == host && !in.incomplete {
				in.incomplete = true
				t.incoming[key] = in
				t.queueAboveLocked(key.Collection, tree.BASE64.Parent(key.Location))
				t.stats.pollChanged.Add(1)
			}
		}
	}
	t.Mu.Unlock()
	t.acceptAll(response.Contributions)
	return response, nil
}

// PollHost reads every boundary below this node's parents, then explicitly
// repairs previously held keys absent from the enumeration.
func (t *Tree) PollHost(ctx context.Context, host string) error {
	seen := map[tree.Key]bool{}
	page := &mesh.SummaryPage{Limit: summaryPollPage}
	for {
		response, err := t.pollResponse(ctx, host, mesh.Request{Kind: mesh.KindSummaryGet, TargetVNode: t.service.VNode, SummaryPage: page})
		if err != nil {
			return err
		}
		for _, contribution := range response.Contributions {
			seen[tree.Key{Collection: contribution.Collection, Location: contribution.Location}] = true
		}
		last := len(response.Contributions) - 1
		if !response.SummaryMore || last < 0 {
			break
		}
		page = &mesh.SummaryPage{Collection: response.Contributions[last].Collection, Location: response.Contributions[last].Location, Limit: summaryPollPage}
	}
	// Absence from an enumeration is not a zero contribution. Ask retained
	// missing keys explicitly, so a deleted last row can return a versioned
	// complete zero and recover the cache without inventing data coverage.
	missing := map[string][]string{}
	t.Mu.Lock()
	for key, in := range t.incoming {
		if in.holder.Host != host || seen[key] {
			continue
		}
		missing[key.Collection] = append(missing[key.Collection], key.Location)
		if !in.incomplete {
			in.incomplete = true
			t.incoming[key] = in
			t.queueAboveLocked(key.Collection, tree.BASE64.Parent(key.Location))
			t.stats.pollChanged.Add(1)
		}
	}
	t.Mu.Unlock()
	for collection, locations := range missing {
		slices.Sort(locations)
		for len(locations) > 0 {
			n := min(len(locations), summaryContributionBatch)
			if _, err := t.pollResponse(ctx, host, mesh.Request{Kind: mesh.KindSummaryGet, TargetVNode: t.service.VNode, Zone: tree.ZoneKey{Collection: collection}, Locations: locations[:n]}); err != nil {
				return err
			}
			locations = locations[n:]
		}
	}
	return ctx.Err()
}

func (t *Tree) acceptAll(contributions []mesh.Contribution) {
	dirty := map[tree.Key]struct{}{}
	for _, contribution := range contributions {
		changed, parent, err := t.acceptChild(contribution)
		if err != nil {
			continue
		}
		if changed {
			t.stats.pollChanged.Add(1)
			dirty[tree.Key{Collection: contribution.Collection, Location: parent}] = struct{}{}
		}
	}
	for parent := range dirty {
		t.refreshZone(parent.Collection, parent.Location)
	}
}

func (t *Tree) remoteHosts() ([]string, error) {
	snapshot, err := t.service.Store.CausalRouting()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var hosts []string
	for _, vnode := range snapshot.Current().Active {
		holder, err := snapshot.Placement(vnode.Name)
		if err != nil {
			return nil, err
		}
		if holder.Host == "" || holder.Host == t.service.Self.Host || seen[holder.Host] {
			continue
		}
		seen[holder.Host] = true
		hosts = append(hosts, holder.Host)
	}
	sort.Strings(hosts)
	return hosts, nil
}
