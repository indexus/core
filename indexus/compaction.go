package indexus

import (
	"context"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/protocol"
)

// CausalCompactionConfig sets opportunistic pair folds (COLD-2). A fold never
// destroys a disk or stops a process; retired endpoints can still forward.
//
// A vnode is eligible once it has stayed under CoolWriteRate writes per second
// for CoolFor. Zero values use the defaults. The node binary enables
// compaction unless its config sets "compaction": false; a library Node
// enables it with EnableCausalCompaction.
type CausalCompactionConfig struct {
	CoolFor, Interval, Timeout time.Duration
	// CoolWriteRate is the write rate (per second) under which this vnode
	// counts as cool. Default 10.
	CoolWriteRate float64
	// MaxCombinedFraction, HeatBudget and MaxItems: see mesh.ColdConfig.
	// MaxItems defaults to half the autoscale item threshold, when set.
	MaxCombinedFraction float64
	MaxItems            int64
	HeatBudget          uint64
}
type causalCompactionResult struct {
	At    time.Time
	Error string
}

const defaultCoolWriteRate = 10

func (c CausalCompactionConfig) limits() mesh.ColdConfig {
	return mesh.ColdConfig{MaxCombinedFraction: c.MaxCombinedFraction, HeatBudget: c.HeatBudget, MaxItems: c.MaxItems}
}

// coolMeter tracks since when the write rate has stayed under a threshold.
// Rates are measured between successive observations, at least a second apart.
type coolMeter struct {
	mu        sync.Mutex
	at        time.Time
	count     int64
	coolSince time.Time
}

func (m *coolMeter) reset(now time.Time, count int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at, m.count, m.coolSince = now, count, now
}

// cool returns how long the rate has stayed at or under limit per second.
// Only judged time counts: a window shorter than a second proves nothing, so
// a CoolFor under a second still needs one measured second.
func (m *coolMeter) cool(now time.Time, count int64, limit float64) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.at.IsZero() {
		m.at, m.count, m.coolSince = now, count, now
		return 0
	}
	if elapsed := now.Sub(m.at); elapsed >= time.Second {
		if float64(count-m.count)/elapsed.Seconds() > limit {
			m.coolSince = now
		}
		m.at, m.count = now, count
	}
	return m.at.Sub(m.coolSince)
}

func (n *Node) EnableCausalCompaction(c CausalCompactionConfig) error {
	if n.mesh == nil {
		return fmt.Errorf("causal runtime required")
	}
	if c.CoolFor < 0 || c.Interval < 0 || c.Timeout < 0 || c.CoolWriteRate < 0 || c.MaxCombinedFraction < 0 || c.MaxCombinedFraction > 1 {
		return fmt.Errorf("invalid compaction bounds")
	}
	if c.CoolFor == 0 {
		c.CoolFor = 2 * time.Minute
	}
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if c.Timeout == 0 {
		c.Timeout = 90 * time.Second
	}
	if c.CoolWriteRate == 0 {
		c.CoolWriteRate = defaultCoolWriteRate
	}
	n.mesh.cool.reset(time.Now(), n.mesh.writes.Load())
	n.mesh.compaction.Store(&c)
	return nil
}
func (n *Node) DisableCausalCompaction() {
	if n.mesh != nil {
		n.mesh.compaction.Store(nil)
	}
}

// CompactCausal is the explicit local entry point. It resumes durable work or
// starts one pair fold; it never directly edits routing membership or grants.
func (n *Node) CompactCausal(ctx context.Context, c CausalCompactionConfig) error {
	return n.compactCausal(ctx, c, false)
}

func (n *Node) compactCausal(ctx context.Context, c CausalCompactionConfig, automatic bool) error {
	if n.mesh == nil {
		return fmt.Errorf("causal runtime required")
	}
	limits := c.limits()
	limits.Automatic = automatic
	if limits.MaxItems == 0 && n.autoscale != nil {
		limits.MaxItems = int64(n.autoscale.Config().ItemsLimit) / 2
	}
	err := n.mesh.service.Compact(ctx, limits)
	if errors.Is(err, mesh.ErrColdDeferred) {
		return nil
	}
	// No peer covers this whole cell in the required direction. No drain
	// was started, so retrying this automatic plan need not flush summaries.
	if automatic && errors.Is(err, protocol.ErrNoBetterConfiguration) {
		return err
	}
	refreshErr := n.meshRefreshView()
	if refreshErr != nil {
		return errors.Join(err, refreshErr)
	}
	if ctx.Err() == nil {
		foldCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, foldErr := n.mesh.summary.Fold(foldCtx, true)
		cancel()
		if foldErr != nil {
			return errors.Join(err, foldErr)
		}
	}
	return err
}
func (n *Node) meshScheduleCompaction() {
	if n.mesh == nil || n.maintenanceHeld.Load() {
		return
	}
	c := n.mesh.compaction.Load()
	if c == nil {
		return
	}
	now := time.Now()
	if n.mesh.cool.cool(now, n.mesh.writes.Load(), c.CoolWriteRate) < c.CoolFor || now.Sub(time.Unix(0, n.mesh.lastColdAttempt.Load())) < c.Interval {
		return
	}
	snapshot, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return
	}
	root, _ := snapshot.Identity()
	if root.Name == n.Name() || !snapshot.HasMember(n.Name()) {
		return
	}
	if !n.mesh.compactionRunning.CompareAndSwap(false, true) {
		return
	}
	n.mesh.lastColdAttempt.Store(now.UnixNano())
	go func() {
		defer n.mesh.compactionRunning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
		defer cancel()
		err := n.compactCausal(ctx, *c, true)
		result := &causalCompactionResult{At: time.Now()}
		if err != nil && !errors.Is(err, protocol.ErrNoBetterConfiguration) && !errors.Is(err, protocol.ErrColdPlanPreempted) {
			result.Error = err.Error()
		}
		n.mesh.lastColdResult.Store(result)
	}()
}

// RetirementWitnessed reports whether this vnode was folded into a survivor
// and the survivor acknowledged the retirement: it holds no authority any
// more and only forwards stale requests.
func (n *Node) RetirementWitnessed() bool {
	if n == nil || n.durableStore() == nil {
		return false
	}
	status, err := n.durableStore().ColdStatus()
	return err == nil && status.Drain != nil && status.Drain.Witnessed
}

// EnableReplication syncs this node's zones to the other members of their
// R(z) on every maintenance tick (convergent replication, step 3). With
// waitAck each write also waits, at most a second, for one of them to hold
// it: needed when the local disk is the only copy (NVMe), not on a redundant
// one (EBS), where the background copy serves availability. byEndpoint
// treats each process as its own machine, for clusters of several processes
// on one host.
func (n *Node) EnableReplication(byEndpoint, waitAck bool) error {
	if n.mesh == nil {
		return fmt.Errorf("causal runtime required")
	}
	if byEndpoint {
		n.mesh.service.ReplicaMachine = func(h causal.Endpoint) string { return h.Host }
	}
	n.mesh.service.ReplicateWrites = waitAck
	n.mesh.replicate.Store(true)
	return nil
}

// DegradedRead returns a record from this node's fallback log overlaid on the
// zone owner's shadow copy, resolving the owner from the local P2P routing
// view. It never reads authoritative live data (SHADOW-1, READ-1 degraded path).
func (n *Node) DegradedRead(ctx context.Context, collection, location, id string) (*Item, bool, error) {
	if n.mesh == nil {
		return nil, false, fmt.Errorf("causal runtime required")
	}
	z, err := tree.NewZoneKey(collection, location)
	if err != nil {
		return nil, false, err
	}
	routing, err := n.mesh.service.Store.CausalRouting()
	if err != nil {
		return nil, false, err
	}
	owner, err := routing.Route(z.ID)
	if err != nil {
		return nil, false, err
	}
	res, err := n.mesh.service.Handle(ctx, mesh.Request{
		Kind:     mesh.KindDegradedRead,
		Owner:    owner,
		Zone:     z,
		RecordID: id,
	})
	if err != nil {
		return nil, false, err
	}
	if res.Record == nil {
		return nil, false, nil
	}
	item := &Item{
		Collection: res.Record.Collection,
		Location:   res.Record.Location,
		Id:         res.Record.ID,
		Metrics:    append([]float64(nil), res.Record.Metrics...),
		Payload:    append([]byte(nil), res.Record.Payload...),
		Weight:     res.Record.Weight,
		Binary:     res.Record.Binary,
	}
	return item, true, nil
}
