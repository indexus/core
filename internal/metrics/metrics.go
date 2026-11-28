// Package metrics holds the process-wide counters an experiment reads from
// GET /metrics. One node runs per process, so the counters are global.
// Every update is one atomic add; nothing here takes a lock on a hot path.
package metrics

import (
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

// Wire counts one direction of one mesh request kind. Membership* isolates the
// membership control messages, the control plane's share.
type Wire struct {
	Calls, Errors, RequestBytes, ResponseBytes, Nanos atomic.Int64
	MembershipCalls, MembershipBytes                  atomic.Int64
}

// Record adds one finished call.
func (w *Wire) Record(request, response int, took time.Duration, failed, membership bool) {
	w.Calls.Add(1)
	w.RequestBytes.Add(int64(request))
	w.ResponseBytes.Add(int64(response))
	w.Nanos.Add(int64(took))
	if failed {
		w.Errors.Add(1)
	}
	if membership {
		w.MembershipCalls.Add(1)
		w.MembershipBytes.Add(int64(request + response))
	}
}

func (w *Wire) snapshot() map[string]int64 {
	return map[string]int64{
		"calls": w.Calls.Load(), "errors": w.Errors.Load(),
		"request_bytes": w.RequestBytes.Load(), "response_bytes": w.ResponseBytes.Load(),
		"nanos": w.Nanos.Load(), "membership_calls": w.MembershipCalls.Load(), "membership_bytes": w.MembershipBytes.Load(),
	}
}

var (
	wireMu        sync.Mutex
	sent, handled = map[string]*Wire{}, map[string]*Wire{}
)

func wire(m map[string]*Wire, kind string) *Wire {
	if kind == "" {
		kind = "exchange"
	}
	wireMu.Lock()
	defer wireMu.Unlock()
	w := m[kind]
	if w == nil {
		w = &Wire{}
		m[kind] = w
	}
	return w
}

// Sent is the client side of mesh requests of one kind; Handled the server side.
func Sent(kind string) *Wire    { return wire(sent, kind) }
func Handled(kind string) *Wire { return wire(handled, kind) }

// Histogram has power-of-two microsecond buckets: bucket i counts durations
// in [2^(i-1), 2^i) µs, bucket 0 everything under 1 µs.
type Histogram struct{ buckets [40]atomic.Int64 }

func (h *Histogram) Observe(d time.Duration) {
	us := uint64(d / time.Microsecond)
	i := bits.Len64(us)
	if i >= len(h.buckets) {
		i = len(h.buckets) - 1
	}
	h.buckets[i].Add(1)
}

func (h *Histogram) snapshot() []int64 {
	out := make([]int64, len(h.buckets))
	last := 0
	for i := range h.buckets {
		if out[i] = h.buckets[i].Load(); out[i] != 0 {
			last = i + 1
		}
	}
	return out[:last]
}

// CopyAdmission measures strict warm requests on one side of a host copy.
// Its wait histogram includes only successful admissions. Deferrals and
// interrupted requests remain visible separately.
type CopyAdmission struct {
	Granted, Deferred, Interrupted, RequesterLeft atomic.Int64
	TimeToGrant                                   Histogram
}

func (a *CopyAdmission) snapshot() map[string]any {
	return map[string]any{
		"granted": a.Granted.Load(), "deferred": a.Deferred.Load(), "interrupted": a.Interrupted.Load(), "requester_left": a.RequesterLeft.Load(),
		"time_to_grant_us_log2": a.TimeToGrant.snapshot(),
	}
}

// Writes are client writes as the node that received them sees them.
// Hops counts, at the applying node, how many relays a write took.
var Writes struct {
	Received, Acknowledged, Refused, Relayed atomic.Int64
	// Applied counts new durable mutations at the holder, excluding retries.
	Applied atomic.Int64
	Latency Histogram
	// Handler is the whole client write request at the entry node, from
	// the handler's start (scope check, JSON decode) to its response.
	Handler Histogram
	Hops    [8]atomic.Int64
}

// Gossip is the anti-entropy loop. Gauges hold the last observed value.
var Gossip struct {
	Rounds, Partners, KnownVNodes, MembershipBytes    atomic.Int64
	FullPackets, DeltaPackets, ReferencePackets       atomic.Int64
	FullJSONBytes, DeltaJSONBytes, ReferenceJSONBytes atomic.Int64
	RoundNanos                                        atomic.Int64
	// AsyncSyncs counts bounded background membership refresh hints.
	AsyncSyncs        atomic.Int64
	AsyncSyncDropped  atomic.Int64 // hints dropped at the per-service worker limit
	AsyncSyncFailures atomic.Int64 // failed background exchange attempts
	Version           atomic.Value // string
}

// Handoffs are completed transfers of authority this node gave or took.
var Handoffs struct {
	Given, Taken, Folds, FoldsAborted atomic.Int64
	// Busy counts joiners answered "busy" by a donor serving another one.
	Busy atomic.Int64
	// InstallToFirstWrite measures the time from install-zone committing a
	// zone on a receiver to the first local write applied on that zone.
	// This includes arrival delay; it excludes replication and response delivery.
	InstallToFirstWrite Histogram
	// Per-attempt timings, not an availability bound across lost ACKs/retries.
	// Call includes pre-fence queueing and the donor's final ACK commit.
	CutoverCall, CutoverFailed Histogram
	// Begins after FreezeZones returns and ends at a successful install reply.
	// It excludes source freeze preparation and all unsuccessful attempts.
	FreezeReturnToInstallACK Histogram
	// The host copy budget (mesh.CopyBudget). Waits: sources that found no
	// slot at first. Timeouts: attempts that exhausted their
	// bounded wait. Shared: receivers that used their source's slot. Busy:
	// callers that may not wait (receivers, resumed grains) and found none.
	CopySlotWaits, CopySlotTimeouts, CopySlotShared, CopySlotBusy atomic.Int64
	CopySlotWaiting, CopySlotWaitNanos                            atomic.Int64
	SourceAdmission, ReceiverAdmission                            CopyAdmission
	CopyPeerWaitNanos, CopyPacedNanos, CopyPacedLots              atomic.Int64
}

// Process milestones are measured from the same monotonic clock. A restarted
// vnode records its first successful inbound installation in the new process.
type milestone struct{ nanos atomic.Int64 }

var FirstAppliedWrite, FirstInboundInstall milestone

// Mark returns the process age to the caller that first observed the event.
func (m *milestone) Mark() time.Duration {
	if m.nanos.Load() == 0 {
		age := max(time.Nanosecond, time.Since(started))
		if m.nanos.CompareAndSwap(0, int64(age)) {
			return age
		}
	}
	return 0
}

// open tracks donor handoff operations in flight, for the sentinel's
// "handoff open too long" and "operation restarted" rules. Handoffs are rare
// events: a mutex per start and end is fine.
var open = struct {
	sync.Mutex
	since    map[string]time.Time
	starts   map[string]int
	target   map[string]string
	progress map[string]*handoffProgress
	phase    map[string]handoffPhase
}{since: map[string]time.Time{}, starts: map[string]int{}, target: map[string]string{}, progress: map[string]*handoffProgress{}, phase: map[string]handoffPhase{}}

type handoffProgress struct {
	move      bool
	installed int
	last      time.Time
}

// HandoffInstalled tracks successful lot installations across retry attempts
// within this process. Sampling phase changes alone cannot show forward progress.
func HandoffInstalled(operation string, zones int) {
	open.Lock()
	defer open.Unlock()
	if p := open.progress[operation]; p != nil {
		p.installed += zones
		p.last = time.Now()
	}
}

type handoffPhase struct {
	name    string
	since   time.Time
	running bool
}

// HandoffPhase names the step currently waiting, independently of /metrics
// response time. It stores no payload or unbounded per-zone state.
func HandoffPhase(operation, name string) {
	open.Lock()
	defer open.Unlock()
	if _, ok := open.since[operation]; !ok {
		return
	}
	open.phase[operation] = handoffPhase{name, time.Now(), true}
}

// HandoffStarted records an attempt of operation towards target (a vnode
// name) and returns its number.
func HandoffStarted(operation, target string) int {
	return handoffBegin(operation, target, true, false)
}

// Waiting for resource admission is observable, but is not a failed attempt
// to move authority (and must not trip the handoff-loop sentinel).
func HandoffQueued(operation, target string, move bool) {
	handoffBegin(operation, target, false, move)
}

func handoffBegin(operation, target string, attempt, move bool) int {
	open.Lock()
	defer open.Unlock()
	if _, ok := open.since[operation]; !ok {
		open.since[operation] = time.Now()
		open.progress[operation] = &handoffProgress{}
	}
	if move {
		open.progress[operation].move = true
	}
	open.target[operation] = target
	open.phase[operation] = handoffPhase{"begin", time.Now(), true}
	if attempt {
		open.starts[operation]++
	}
	if len(open.starts) > 4096 { // bound memory on a long-lived node
		for k := range open.starts {
			if _, running := open.since[k]; !running {
				delete(open.starts, k)
			}
		}
	}
	return open.starts[operation]
}

// HandoffEnded records the end of an attempt; done means the operation is
// finished and will not run again.
func HandoffEnded(operation string, done bool) {
	open.Lock()
	defer open.Unlock()
	phase := open.phase[operation]
	phase.running = false
	open.phase[operation] = phase
	if done {
		delete(open.phase, operation)
		delete(open.since, operation)
		delete(open.starts, operation)
		delete(open.target, operation)
		delete(open.progress, operation)
	}
}

// OpenHandoffs reads the same donor registry as the dashboard without enumerating it.
func OpenHandoffs() int {
	open.Lock()
	defer open.Unlock()
	return len(open.since)
}

func openSnapshot() map[string]any {
	open.Lock()
	defer open.Unlock()
	oldest, attempts := 0.0, 0
	var list []map[string]any
	for op, t := range open.since {
		oldest = max(oldest, time.Since(t).Seconds())
		attempts = max(attempts, open.starts[op])
		if len(list) < 32 { // for the dashboard: who is giving to whom
			phase := open.phase[op]
			p := open.progress[op]
			var lastAge any
			if !p.last.IsZero() {
				lastAge = time.Since(p.last).Seconds()
			}
			list = append(list, map[string]any{"installed_zones": p.installed, "last_install_seconds": lastAge, "phase": phase.name, "phase_seconds": time.Since(phase.since).Seconds(), "running": phase.running, "op": op, "target": open.target[op], "physical_move": p.move, "seconds": time.Since(t).Seconds(), "attempts": open.starts[op]})
		}
	}
	return map[string]any{"count": len(open.since), "oldest_seconds": oldest, "max_attempts": attempts, "list": list}
}

// Transport counts mesh requests by the protocol that carried them.
var Transport struct {
	HTTP3, TCP, Fallbacks atomic.Int64
}

// Replication tracks convergent replication health and degraded-mode activity.
var Replication struct {
	LagSeq, ShadowBytes             atomic.Int64
	AcksReplicated, AcksLocal       atomic.Int64
	FallbackWrites, FallbackRepairs atomic.Int64
	DegradedReads                   atomic.Int64
}

var started = time.Now()

// Snapshot is the /metrics document.
func Snapshot() map[string]any {
	wireMu.Lock()
	out, in := map[string]any{}, map[string]any{}
	for k, w := range sent {
		out[k] = w.snapshot()
	}
	for k, w := range handled {
		in[k] = w.snapshot()
	}
	wireMu.Unlock()
	hops := make([]int64, len(Writes.Hops))
	for i := range hops {
		hops[i] = Writes.Hops[i].Load()
	}
	version, _ := Gossip.Version.Load().(string)
	return map[string]any{
		"uptime_seconds": time.Since(started).Seconds(),
		"mesh_sent":      out,
		"mesh_handled":   in,
		"writes": map[string]any{
			"received": Writes.Received.Load(), "acknowledged": Writes.Acknowledged.Load(),
			"applied": Writes.Applied.Load(),
			"refused": Writes.Refused.Load(), "relayed": Writes.Relayed.Load(),
			"latency_us_log2": Writes.Latency.snapshot(), "handler_us_log2": Writes.Handler.snapshot(),
			"hops": hops,
		},
		"gossip": map[string]any{
			"rounds": Gossip.Rounds.Load(), "partners": Gossip.Partners.Load(),
			"full_packets": Gossip.FullPackets.Load(), "delta_packets": Gossip.DeltaPackets.Load(), "reference_packets": Gossip.ReferencePackets.Load(),
			"full_json_bytes": Gossip.FullJSONBytes.Load(), "delta_json_bytes": Gossip.DeltaJSONBytes.Load(), "reference_json_bytes": Gossip.ReferenceJSONBytes.Load(),
			"known_vnodes": Gossip.KnownVNodes.Load(), "membership_bytes": Gossip.MembershipBytes.Load(),
			"round_nanos": Gossip.RoundNanos.Load(), "version": version,
			"async_syncs":         Gossip.AsyncSyncs.Load(),
			"async_sync_dropped":  Gossip.AsyncSyncDropped.Load(),
			"async_sync_failures": Gossip.AsyncSyncFailures.Load(),
		},
		"transport": map[string]any{
			"http3": Transport.HTTP3.Load(), "tcp": Transport.TCP.Load(), "fallbacks": Transport.Fallbacks.Load(),
		},
		"handoffs": map[string]any{
			"given": Handoffs.Given.Load(), "taken": Handoffs.Taken.Load(),
			"folds": Handoffs.Folds.Load(), "folds_aborted": Handoffs.FoldsAborted.Load(),
			"busy": Handoffs.Busy.Load(), "open": openSnapshot(),
			"copy_slot_waits": Handoffs.CopySlotWaits.Load(), "copy_slot_timeouts": Handoffs.CopySlotTimeouts.Load(),
			"copy_slot_waiting": Handoffs.CopySlotWaiting.Load(), "copy_slot_wait_ms": Handoffs.CopySlotWaitNanos.Load() / int64(time.Millisecond),
			"copy_slot_shared": Handoffs.CopySlotShared.Load(), "copy_slot_busy": Handoffs.CopySlotBusy.Load(),
			"warm_source_admission":   Handoffs.SourceAdmission.snapshot(),
			"warm_receiver_admission": Handoffs.ReceiverAdmission.snapshot(),
			"copy_peer_wait_ms":       Handoffs.CopyPeerWaitNanos.Load() / int64(time.Millisecond),
			"copy_paced_lots":         Handoffs.CopyPacedLots.Load(), "copy_paced_ms": Handoffs.CopyPacedNanos.Load() / int64(time.Millisecond),
			"install_to_first_write_us_log2":       Handoffs.InstallToFirstWrite.snapshot(),
			"cutover_call_us_log2":                 Handoffs.CutoverCall.snapshot(),
			"cutover_failed_us_log2":               Handoffs.CutoverFailed.snapshot(),
			"freeze_return_to_install_ack_us_log2": Handoffs.FreezeReturnToInstallACK.snapshot(),
		},
		"replication": map[string]any{
			"lag_seq":          Replication.LagSeq.Load(),
			"shadow_bytes":     Replication.ShadowBytes.Load(),
			"acks_replicated":  Replication.AcksReplicated.Load(),
			"acks_local":       Replication.AcksLocal.Load(),
			"fallback_writes":  Replication.FallbackWrites.Load(),
			"fallback_repairs": Replication.FallbackRepairs.Load(),
			"degraded_reads":   Replication.DegradedReads.Load(),
		},
		"first_write_after_s":           time.Duration(FirstAppliedWrite.nanos.Load()).Seconds(),
		"first_inbound_install_after_s": time.Duration(FirstInboundInstall.nanos.Load()).Seconds(),
	}
}
