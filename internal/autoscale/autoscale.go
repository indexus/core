package autoscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// The controller requests process capacity. It does not grant write authority
// or block data admission while a provisioner is working. Node supplies the
// validated placement plan; the store remains the sole transactional writer.

type Config struct {
	Enabled bool
	// ProvisionTimeout bounds control-plane calls independently of admission.
	ProvisionTimeout time.Duration

	Role           string
	ProvisionerURL string
	StoragePath    string

	Window   time.Duration
	Cooldown time.Duration

	PressureHold time.Duration

	MemLimitPct float64
	// DiskMinFreePct: free-disk floor that forces scale-up. 0 disables
	// (INDEXUS_DISK_MIN_FREE_PCT=0). Unset defaults to 40.
	DiskMinFreePct    float64
	DisableDiskSignal bool
	CPULimitPct       float64
	MemLead           time.Duration
	MemFloorPct       float64
	CPUFloorPct       float64
	MemRisePct        float64
	CPURisePct        float64
	RiseHold          time.Duration
	// MemRefusePct: emergency instant scale-up + same env as Settings write refuse
	// (INDEXUS_MEM_REFUSE_PCT). Soft mem scale uses MemLimitPct + hold instead.
	MemRefusePct float64
	// ItemsLimit: owned official item count that forces scale-up even without
	// mem/CPU pressure (0 disables). Env: INDEXUS_ITEMS_LIMIT.
	ItemsLimit int
	// ItemsOnly scales on ItemsLimit alone (plus the memory refusal guard):
	// experiments on shared hosts, where CPU, memory and disk readings belong
	// to the host rather than to one vnode.
	ItemsOnly bool
	// WritesLimit: write demand per second (applied or refused at the cap)
	// that triggers scale-up, also with ItemsOnly: the load signal of
	// one vnode, not of its host (0 disables).
	WritesLimit float64
	// WritesHold, when positive, smooths write demand with an EWMA and a 15%
	// hysteresis deadband so single-tick jitter around WritesLimit does not
	// cause split/fold oscillations (a 2x+ surge still triggers at once).
	WritesHold time.Duration
	// QueueWaitLimit: age of the oldest write in flight at this vnode that
	// asks for capacity. Unset defaults to 2s, above the bounded waits of a
	// healthy write (cutover 500ms, replica ACK 1s); negative disables.
	QueueWaitLimit time.Duration
	// QueueLimit: writes in flight at this vnode that ask for capacity. Unset
	// derives it from WritesLimit: what the vnode may receive during
	// QueueWaitLimit at its write limit (none without WritesLimit); negative
	// disables.
	QueueLimit int
	// Sample overrides SampleResources (tests inject the host and process
	// readings instead of reading /proc).
	Sample Sampler
	// Now overrides time.Now for deterministic tick simulations (nil uses time.Now).
	Now func() time.Time
}

type Window struct {
	mu      sync.Mutex
	buckets []int64
	start   time.Time
	size    int
	bucket  time.Duration
}

func NewWindow(window time.Duration) *Window {
	bucket := time.Minute
	if window > 0 && window < 2*time.Minute {
		bucket = time.Second
	}
	if window < bucket {
		window = bucket
	}
	n := int(window / bucket)
	if n < 1 {
		n = 1
	}
	if n > 120 {
		n = 120
	}
	return &Window{
		buckets: make([]int64, n),
		start:   time.Now().UTC().Truncate(bucket),
		size:    n,
		bucket:  bucket,
	}
}

func (w *Window) Record(n int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateLocked()
	w.buckets[w.size-1] += n
}

func (w *Window) Sum() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateLocked()
	var s int64
	for _, v := range w.buckets {
		s += v
	}
	return s
}

func (w *Window) rotateLocked() {
	now := time.Now().UTC().Truncate(w.bucket)
	elapsed := int(now.Sub(w.start) / w.bucket)
	if elapsed <= 0 {
		return
	}
	if elapsed >= w.size {
		for i := range w.buckets {
			w.buckets[i] = 0
		}
		w.start = now
		return
	}
	copy(w.buckets, w.buckets[elapsed:])
	for i := w.size - elapsed; i < w.size; i++ {
		w.buckets[i] = 0
	}
	w.start = w.start.Add(time.Duration(elapsed) * w.bucket)
}

// ReasonHostPressure is the reason of a request fired by the machine (host
// CPU or host memory) and not by this vnode's own items, writes or queue.
// Every vnode of a hot machine sends it: the provisioner coalesces them into
// one physical decision per machine.
const ReasonHostPressure = "host_pressure"

type Request struct {
	WriteRate  float64 // arrivals/s at the requesting holder
	PreferNear string

	PreferNears []string
	// Reason is the vnode's own signal, or ReasonHostPressure.
	Reason string
	// Signal is the signal that fired, also under ReasonHostPressure.
	Signal string

	SpawnCount int
	// TransfersInFlight: outgoing transfers of this vnode when it asked.
	TransfersInFlight int
	Resources         ResourceSample
}

type Input struct {
	// Queue is the number of writes arrived at this vnode and not answered.
	Queue int
	// OldestWait is the age of the oldest of them.
	OldestWait time.Duration
	// TransfersOut is the number of transfers this vnode is copying out.
	TransfersOut int
	OwnedZones   int
	OwnedItems   int
	// WriteRate is the writes that arrived at this vnode as their holder, per
	// second since the last tick: demand, whether or not it was served.
	WriteRate float64
	Resources ResourceSample
	SelfName  string
	// Members is the immutable membership of the current view, so a scale-up in flight
	// is known to have landed once a vnode near its target appears (SCALE-1).
	Members []tree.VNodeDescriptor
}

// scaleUpLandingBits is how close an active vnode must be to a scale-up
// target to count as that scale-up having landed; spawned names keep 64 bits
// of their target (SPAWN-1).
const scaleUpLandingBits = 64

// scaleUpPatience bounds how long a vnode waits for its previous scale-up to
// land before asking again: a saturated donor makes the join slow.
const scaleUpPatience = 3 * time.Minute

const defaultQueueWaitLimit = 2 * time.Second

type Controller struct {
	cfg Config

	window *Window

	mu         sync.Mutex
	lastUpAt   time.Time
	retryAt    time.Time // earliest new request after an explicit provisioner cooldown
	upInFlight atomic.Bool
	hotAt      atomic.Int64 // unix nanoseconds of the last tick under pressure
	// pendingBefore contains only existing vnodes with the target prefix.
	// Only a vnode not among them lands it: in a deep zone the donor and its
	// neighbours share the target's first 64 bits.
	pendingBefore map[tree.ID]bool
	scaleUpsDone  int

	lastQueue      int
	lastQueueAt    time.Time
	lastMem        float64
	lastMemAt      time.Time
	lastCPU        float64
	lastCPUAt      time.Time
	pressureSince  [4]time.Time
	writeEWMA      float64
	writeEWMAInit  bool
	writeHotSince  time.Time
	risingFast     bool
	lastReason     string
	lastPreferNear string
	// pending is the target of the last scale-up request, sent or timed out,
	// until a vnode near it is active (SCALE-1).
	pending      tree.ID
	pendingSince time.Time
	lastPressure map[string]any

	// nearBuilder partitions retained owned load into PreferNear tags (~1/N each).
	nearBuilder func(reason string, spawnN int, fallback string) []string
}

func New(cfg Config) *Controller {
	if cfg.ProvisionTimeout <= 0 {
		cfg.ProvisionTimeout = 15 * time.Second
	}
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 15 * time.Second
	}
	if cfg.Role == "" {
		cfg.Role = "bootstrap"
	}
	if cfg.PressureHold <= 0 {

		cfg.PressureHold = 30 * time.Second
	}

	if cfg.MemLimitPct <= 0 {
		cfg.MemLimitPct = 40
	}

	if cfg.DiskMinFreePct <= 0 && !cfg.DisableDiskSignal {
		cfg.DiskMinFreePct = defaultDiskMinFreePct
	}
	if cfg.CPULimitPct <= 0 {
		cfg.CPULimitPct = defaultCPUTargetPct
	}
	if cfg.MemLead <= 0 {
		cfg.MemLead = 90 * time.Second
	}
	if cfg.MemFloorPct <= 0 {
		cfg.MemFloorPct = 25
	}
	if cfg.CPUFloorPct <= 0 {
		cfg.CPUFloorPct = 25
	}

	if cfg.MemRisePct <= 0 {
		cfg.MemRisePct = 2.0
	}
	if cfg.CPURisePct <= 0 {
		cfg.CPURisePct = 2.0
	}
	if cfg.RiseHold <= 0 {
		cfg.RiseHold = 12 * time.Second
	}

	if cfg.MemRefusePct <= 0 {
		cfg.MemRefusePct = 60
	}
	if cfg.QueueWaitLimit == 0 {
		cfg.QueueWaitLimit = defaultQueueWaitLimit
	}
	if cfg.QueueLimit == 0 && cfg.WritesLimit > 0 && cfg.QueueWaitLimit > 0 {
		cfg.QueueLimit = int(cfg.WritesLimit * cfg.QueueWaitLimit.Seconds())
	}
	return &Controller{
		cfg:          cfg,
		window:       NewWindow(cfg.Window),
		lastPressure: map[string]any{},
	}
}

func (a *Controller) RecordInsert() {
	a.RecordInsertAt("", "")
}

// RecordInsertAt counts an insert in the pressure window. Location is ignored
// for placement — PreferNear comes from weighted zone split only.
func (a *Controller) RecordInsertAt(collection, location string) {
	if a == nil || !a.cfg.Enabled {
		return
	}
	_ = collection
	_ = location
	a.window.Record(1)
}

func (a *Controller) Snapshot() map[string]any {
	if a == nil {
		return map[string]any{"enabled": false}
	}
	sum := a.window.Sum()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]any{
		"enabled":          a.cfg.Enabled,
		"role":             a.cfg.Role,
		"inserts_window":   sum,
		"window":           a.cfg.Window.String(),
		"cooldown":         a.cfg.Cooldown.String(),
		"scale_ups_done":   a.scaleUpsDone,
		"up_in_flight":     a.upInFlight.Load(),
		"last_up_at":       a.lastUpAt,
		"pressure_hold":    a.cfg.PressureHold.String(),
		"mem_limit_pct":    a.cfg.MemLimitPct,
		"mem_floor_pct":    a.cfg.MemFloorPct,
		"cpu_floor_pct":    a.cfg.CPUFloorPct,
		"disk_min_free":    a.cfg.DiskMinFreePct,
		"cpu_limit_pct":    a.cfg.CPULimitPct,
		"mem_lead":         a.cfg.MemLead.String(),
		"mem_rise_pct":     a.cfg.MemRisePct,
		"cpu_rise_pct":     a.cfg.CPURisePct,
		"rise_hold":        a.cfg.RiseHold.String(),
		"mem_refuse_pct":   a.cfg.MemRefusePct,
		"items_limit":      a.cfg.ItemsLimit,
		"queue_limit":      a.cfg.QueueLimit,
		"queue_wait_limit": a.cfg.QueueWaitLimit.String(),
		"rising_fast":      a.risingFast,
		"admit_blocked":    a.clientWritesBlockedLocked(),
		"last_reason":      a.lastReason,
		"last_prefer_near": a.lastPreferNear,
		"pressure":         maps.Clone(a.lastPressure), // later ticks write into the live map
	}
	return out
}

func (a *Controller) SetNearBuilder(fn func(reason string, spawnN int, fallback string) []string) {
	if a == nil {
		return
	}
	a.nearBuilder = fn
}

func (a *Controller) buildPreferNears(reason string, spawnN int, fallback string) []string {
	if a != nil && a.nearBuilder != nil {
		// Empty means "no stable placement" (e.g. no exclusive load) — do not
		// invent a PreferNear that would collide with the local mesh.
		return a.nearBuilder(reason, spawnN, fallback)
	}
	nears, _ := tree.BASE64.PreferNearTargets(fallback, spawnN)
	return nears
}

func (a *Controller) now() time.Time {
	if a != nil && a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}

// Sample reads the resources this controller evaluates.
func (a *Controller) Sample() ResourceSample {
	path := a.cfg.StoragePath
	if path == "" {
		path = "."
	}
	if a.cfg.Sample != nil {
		return a.cfg.Sample(path)
	}
	return SampleResources(path)
}

// evaluatePressure decides whether this tick asks for capacity. It returns
// the signal that fires ("" for none) and whether the machine, rather than
// this vnode, is what fires it.
// Signals, in order of precedence:
//  1. sample the resources and record the pressure snapshot;
//  2. the vnode's own load: owned items, write arrivals, writes in flight;
//  3. memory at the write-refuse threshold (the only resource signal kept
//     with ItemsOnly);
//  4. free disk under the floor;
//  5. a rise or level signal sustained for its hold. When the machine fires
//     it and this vnode is already copying a transfer out, it waits: its
//     relief is on the way and one more receiver is one more copy.
func (a *Controller) evaluatePressure(in Input) (signal string, host bool) {
	sum := a.window.Sum()
	res := in.Resources
	now := a.now()

	sustained, held, holdNeed := a.samplePressure(in, sum, now)

	if own := a.ownSignal(in, now); own != "" {
		a.notePressure("hot_signal", own)
		return own, false
	}
	// Emergency: at/above write-refuse threshold — spawn immediately (no hold).
	if a.cfg.MemRefusePct > 0 && res.MemPct >= a.cfg.MemRefusePct {
		return "mem", a.hostDriven("mem", res)
	}
	if a.cfg.ItemsOnly {
		return "", false
	}
	if a.cfg.DiskMinFreePct > 0 && res.DiskOK && res.DiskFreePct < a.cfg.DiskMinFreePct {
		return "disk", false
	}
	if sustained == "" || held < holdNeed {
		return "", false
	}
	if !a.hostDriven(sustained, res) {
		return sustained, false
	}
	if in.TransfersOut > 0 {
		a.notePressure("host_pressure_waits_for_transfer", true)
		return "", false
	}
	return sustained, true
}

// ownSignal names the load of this vnode alone that asks for capacity.
func (a *Controller) ownSignal(in Input, now time.Time) string {
	switch {
	case a.cfg.ItemsLimit > 0 && in.OwnedItems >= a.cfg.ItemsLimit:
		return "items"
	case a.cfg.WritesLimit > 0 && a.writesFire(in.WriteRate, now):
		return "writes"
	case a.queueFires(in):
		return "queue"
	}
	return ""
}

// queueFires: writes wait at this vnode. It does not depend on how many are
// applied or acknowledged, so a store that stops serving keeps it true.
func (a *Controller) queueFires(in Input) bool {
	if a.cfg.QueueWaitLimit > 0 && in.OldestWait >= a.cfg.QueueWaitLimit {
		return true
	}
	return a.cfg.QueueLimit > 0 && in.Queue >= a.cfg.QueueLimit
}

// hostDriven reports that a resource signal comes from the machine: the host
// was measured and this process alone is under the limit of that resource.
func (a *Controller) hostDriven(signal string, res ResourceSample) bool {
	switch signal {
	case "cpu", "cpu_rise":
		return res.HostCPUOK && res.ProcessCPUPct < a.cfg.CPULimitPct
	case "mem":
		return res.HostMemOK && res.ProcessMemPct < a.cfg.MemRefusePct
	case "mem_filling", "mem_rise":
		return res.HostMemOK && res.ProcessMemPct < a.cfg.MemLimitPct
	}
	return false
}

func (a *Controller) notePressure(key string, value any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastPressure[key] = value
}

// samplePressure folds one resource sample into the controller under its
// lock and records the pressure snapshot. It returns the sustained signal
// (a mature rise wins, then a mature level), how long it has held and the hold
// it needs before firing.
func (a *Controller) samplePressure(in Input, sum int64, now time.Time) (sustained string, held, holdNeed time.Duration) {
	res := in.Resources
	a.mu.Lock()
	defer a.mu.Unlock()

	queueDelta := in.Queue - a.lastQueue
	if a.lastQueueAt.IsZero() {
		queueDelta = 0
	}
	a.lastQueue = in.Queue
	a.lastQueueAt = now

	memRise := a.memRiseRateLocked(res.MemPct, now)
	cpuRise := a.cpuRiseRateLocked(res.CPUPct, res.CPUOK, now)
	projected := a.projectMemoryLocked(res.MemPct, now)

	sustained, held, holdNeed = a.holdLocked(res, memRise, cpuRise, projected, now)

	a.lastPressure = map[string]any{
		"queue":               in.Queue,
		"queue_delta":         queueDelta,
		"oldest_wait_ms":      in.OldestWait.Milliseconds(),
		"transfers_out":       in.TransfersOut,
		"hot_signal":          sustained,
		"hot_for":             held.Round(time.Second).String(),
		"inserts_window":      sum,
		"owned_items":         in.OwnedItems,
		"write_rate":          in.WriteRate,
		"cpu_pct":             res.CPUPct,
		"cpu_process_pct":     res.ProcessCPUPct,
		"cpu_host_pct":        res.HostCPUPct,
		"cpu_host_ok":         res.HostCPUOK,
		"cpu_host_iowait_pct": res.HostIOWaitPct,
		"cpu_rise":            cpuRise,
		"mem_pct":             res.MemPct,
		"mem_process_pct":     res.ProcessMemPct,
		"mem_host_pct":        res.HostMemPct,
		"mem_rise":            memRise,
		"mem_projected":       projected,
		"disk_free_pct":       res.DiskFreePct,
		"heap_inuse":          res.HeapInuse,
		"rss":                 res.RSS,
	}
	return sustained, held, holdNeed
}

// writesFire folds one write-rate sample into the smoother and reports
// whether write demand asks for capacity.
func (a *Controller) writesFire(rate float64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.writeEWMAInit {
		a.writeEWMA = rate
		a.writeEWMAInit = true
	} else {
		a.writeEWMA = 0.35*rate + 0.65*a.writeEWMA
	}
	ewma := a.writeEWMA
	// When WritesHold > 0, smooth with EWMA + 15% hysteresis deadband so
	// single-tick jitter around WritesLimit does not cause oscillations,
	// while still reacting at once to a genuine 2x+ surge.
	var fire bool
	switch {
	case a.cfg.WritesHold <= 0:
		fire = rate >= a.cfg.WritesLimit
	case rate >= 2*a.cfg.WritesLimit:
		fire = true
	case ewma >= a.cfg.WritesLimit:
		if a.writeHotSince.IsZero() {
			a.writeHotSince = now
		}
		fire = now.Sub(a.writeHotSince) >= a.cfg.WritesHold
	case ewma < 0.85*a.cfg.WritesLimit:
		a.writeHotSince = time.Time{}
	}
	a.lastPressure["write_rate_ewma"] = ewma
	return fire
}

// ClientWritesBlocked reports only measured memory exhaustion. Provisioning,
// cooldown and a rising-but-still-low signal are control-plane policy, not
// reasons to refuse an otherwise authorized durable write.
func (a *Controller) ClientWritesBlocked() bool {
	if a == nil || !a.cfg.Enabled {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientWritesBlockedLocked()
}
func (a *Controller) clientWritesBlockedLocked() bool {
	memory, _ := a.lastPressure["mem_pct"].(float64)
	return a.cfg.MemRefusePct > 0 && memory >= a.cfg.MemRefusePct
}

func (a *Controller) memRiseRateLocked(pct float64, now time.Time) float64 {
	previous, at := a.lastMem, a.lastMemAt
	if at.IsZero() || pct <= 0 {
		return 0
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	rate := (pct - previous) / elapsed
	if rate < 0 {
		return 0
	}
	return rate
}

func (a *Controller) cpuRiseRateLocked(pct float64, ok bool, now time.Time) float64 {
	if !ok {
		a.lastCPU, a.lastCPUAt = 0, time.Time{}
		return 0
	}
	previous, at := a.lastCPU, a.lastCPUAt
	a.lastCPU, a.lastCPUAt = pct, now
	if at.IsZero() {
		return 0
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	rate := (pct - previous) / elapsed
	if rate < 0 {
		return 0
	}
	return rate
}

func (a *Controller) projectMemoryLocked(pct float64, now time.Time) float64 {
	if pct <= 0 {
		return 0
	}
	previous, at := a.lastMem, a.lastMemAt
	a.lastMem, a.lastMemAt = pct, now
	if at.IsZero() {
		return pct
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return pct
	}
	slope := (pct - previous) / elapsed
	if slope <= 0 {
		return pct
	}
	return pct + slope*a.cfg.MemLead.Seconds()
}

// Tick requests capacity under pressure. There is no scale-down here: cool
// vnodes are folded by causal compaction (COLD-2), which is how a topology
// shrinks after a spike.
//
// Phases, in order:
//  1. settle the scale-up still pending (SCALE-1);
//  2. evaluate the pressure: a tick under pressure is stamped hot even while
//     it waits for the pending scale-up;
//  3. stop when nothing fires, in cooldown, or with a request in flight;
//  4. choose the target, record it as pending, and ask in the background.
func (a *Controller) Tick(in Input, scaleUp func(Request) error) {
	if a == nil || !a.cfg.Enabled {
		return
	}
	now := a.now()

	pendingWait := a.settlePending(in.Members, now)

	signal, host := a.evaluatePressure(in)
	fire, reason := signal != "", signal
	if host {
		reason = ReasonHostPressure
	}
	a.notePressure("host_pressure", host)
	if fire {
		a.hotAt.Store(now.UnixNano())
	}
	if pendingWait {
		fire = false
	}
	if !fire {
		a.clearLastReason()
	}
	if !fire || !a.reserveRequest(now) {
		return
	}

	const spawnN = 1
	// Dichotomy over exclusive owned zones (~½ items), avoiding known peers.
	nears := a.buildPreferNears(reason, spawnN, in.SelfName)
	if len(nears) == 0 {
		a.upInFlight.Store(false)
		slog.Info("scale-up skipped: no stable PreferNear (no exclusive load among known peers)",
			"reason", reason, "self", in.SelfName)
		return
	}
	req := Request{WriteRate: in.WriteRate, PreferNear: nears[0], PreferNears: nears, Reason: reason, Signal: signal, SpawnCount: len(nears),
		TransfersInFlight: in.TransfersOut, Resources: in.Resources}
	a.recordPending(req, in.Members, now)
	go a.requestScaleUp(req, scaleUp)
}

// settlePending reports whether the previous scale-up is still awaited.
//
// SCALE-1: one scale-up in flight per vnode. A request that timed out may
// still have started a node; asking again would send a second joiner to
// the same saturated donor. When the joiner lands, reset the pre-split
// write smoother so the donor evaluates its post-split load cleanly.
func (a *Controller) settlePending(active []tree.VNodeDescriptor, now time.Time) (waiting bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingSince.IsZero() {
		return false
	}
	landed := a.pendingLandedLocked(active)
	if !landed && now.Sub(a.pendingSince) < scaleUpPatience {
		return true
	}
	a.pendingSince = time.Time{}
	if landed {
		a.writeEWMAInit = false
		a.writeHotSince = time.Time{}
		a.pressureSince = [4]time.Time{}
	}
	return false
}

// pendingLandedLocked reports that a vnode unknown when the scale-up was
// asked is now active near its target.
func (a *Controller) pendingLandedLocked(active []tree.VNodeDescriptor) bool {
	for _, member := range active {
		if int(tree.CommonPrefixLen(member.ID, a.pending)) >= scaleUpLandingBits && !a.pendingBefore[member.ID] {
			return true
		}
	}
	return false
}

func (a *Controller) clearLastReason() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastReason = ""
}

// Reserve before planning: overlapping ticks must not plan two reliefs for
// the same pressure episode, even when planning or an HTTP response is slow.
func (a *Controller) reserveRequest(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.pendingSince.IsZero() || now.Before(a.retryAt) || !a.lastUpAt.IsZero() && now.Sub(a.lastUpAt) < a.cfg.Cooldown {
		return false
	}
	return a.upInFlight.CompareAndSwap(false, true)
}

// recordPending notes the request about to be sent: its reason, its target
// and the vnodes active before it, so that only a new vnode lands it. A
// target that does not decode leaves the previous pending state untouched.
func (a *Controller) recordPending(req Request, active []tree.VNodeDescriptor, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastReason = req.Reason
	a.lastPreferNear = req.PreferNear
	target, err := tree.BASE64.Decode(req.PreferNear)
	if err != nil {
		return
	}
	a.pending, a.pendingSince = tree.FromSlice(target), now
	clear(a.pendingBefore)
	for _, member := range active {
		if int(tree.CommonPrefixLen(member.ID, a.pending)) >= scaleUpLandingBits {
			if a.pendingBefore == nil {
				a.pendingBefore = make(map[tree.ID]bool)
			}
			a.pendingBefore[member.ID] = true
		}
	}
}

// requestScaleUp sends one scale-up request and records its outcome; it runs
// in its own goroutine and clears upInFlight when it returns.
func (a *Controller) requestScaleUp(req Request, scaleUp func(Request) error) {
	defer a.upInFlight.Store(false)
	if scaleUp != nil {
		if err := scaleUp(req); err != nil {
			slog.Warn("scale-up failed", "reason", req.Reason, "spawn_count", req.SpawnCount, "err", err)
			if !maybeStarted(err) {
				a.mu.Lock()
				a.pendingSince = time.Time{}
				var throttled *scaleCooldown
				if errors.As(err, &throttled) {
					a.retryAt = a.now().Add(throttled.wait)
				}
				a.mu.Unlock()
			}
			return
		}
	}
	a.scaleUpSucceeded()
}

// scaleUpSucceeded starts the cooldown and clears the hold timers, so the
// next request needs fresh sustained pressure.
func (a *Controller) scaleUpSucceeded() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUpAt = a.now()
	a.scaleUpsDone++

	a.pressureSince = [4]time.Time{}
	a.writeHotSince = time.Time{}
}

func (a *Controller) PostScale(provisionerURL, requesterID string, inserts int64, owned int, preferNear, reason string, spawnCount int, preferNears []string) error {
	return a.PostRequest(provisionerURL, requesterID, inserts, owned,
		Request{PreferNear: preferNear, PreferNears: preferNears, Reason: reason, Signal: reason, SpawnCount: spawnCount})
}

// PostRequest sends one scale-up request to the provisioner. Beyond the
// placement it says what fired: "reason" is ReasonHostPressure when the
// machine did, "signal" the signal itself, and "transfers_in_flight" how
// many transfers the requester was copying out.
func (a *Controller) PostRequest(provisionerURL, requesterID string, inserts int64, owned int, req Request) error {
	preferNear, reason, spawnCount, preferNears := req.PreferNear, req.Reason, req.SpawnCount, req.PreferNears
	if preferNear == "" {
		preferNear = requesterID
	}
	if reason == "" {
		reason = "resource_pressure"
	}
	if spawnCount < 1 {
		spawnCount = 1
	}
	if spawnCount > 3 {
		spawnCount = 3
	}
	if len(preferNears) == 0 {
		preferNears, _ = tree.BASE64.PreferNearTargets(preferNear, spawnCount)
	}
	body, _ := json.Marshal(map[string]any{
		"requester_id":  requesterID,
		"local_inserts": inserts,
		"owned_zones":   owned,
		"reason":        reason,
		"role":          a.cfg.Role,
		"prefer_near":   preferNear,
		"prefer_nears":  preferNears,
		"spawn_count":   spawnCount,

		"signal":              req.Signal,
		"write_rate":          req.WriteRate,
		"transfers_in_flight": req.TransfersInFlight,
		"cpu_process_pct":     req.Resources.ProcessCPUPct,
		"cpu_host_pct":        req.Resources.HostCPUPct,
		"cpu_host_ok":         req.Resources.HostCPUOK,
	})
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ProvisionTimeout)
	defer cancel()
	return postJSONContext(ctx, provisionerURL+"/v1/scale", body)
}

func postJSONContext(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("INDEXUS_BEARER"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("issuer response: %w", err)
	}
	if len(answer) > 1<<20 {
		return fmt.Errorf("issuer response exceeds 1 MiB")
	}
	var reply struct {
		RetryMS   int64  `json:"retry_after_ms"`
		Coalesced string `json:"coalesced"`
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests {
		_ = json.Unmarshal(answer, &reply)
	}
	if (resp.StatusCode == http.StatusTooManyRequests || reply.Coalesced == ReasonHostPressure) &&
		reply.RetryMS > 0 && reply.RetryMS <= (time.Duration(1<<63-1)).Milliseconds() {
		return &scaleCooldown{fmt.Errorf("scale deferred: %s", answer), time.Duration(reply.RetryMS) * time.Millisecond}
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, answer)
	}
	slog.Debug("issuer answered", "url", url, "answer", string(answer))
	return nil
}

type scaleCooldown struct {
	error
	wait time.Duration
}

// maybeStarted: a request that got no answer may still have started a node;
// an explicit refusal did not.
func maybeStarted(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
}

// wantedFor is how long a vnode still wants a peer after its last tick under
// pressure: a joiner it asked for lands within it.
const wantedFor = 30 * time.Second

// Wanted reports that the vnode was under pressure within wantedFor. It
// only decides for joiners started by the autoscale (mesh Request.Spawned).
func (a *Controller) Wanted() bool {
	if a == nil || !a.cfg.Enabled {
		return true
	}
	return a.now().Sub(time.Unix(0, a.hotAt.Load())) < wantedFor
}

// Config is the configuration the controller runs with, defaults applied.
func (a *Controller) Config() Config {
	if a == nil {
		return Config{}
	}
	return a.cfg
}

// Inserts is the number of inserts recorded in the pressure window.
func (a *Controller) Inserts() int64 { return a.window.Sum() }
