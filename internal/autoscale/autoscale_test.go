package autoscale_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestInsertWindowSum(t *testing.T) {
	w := autoscale.NewWindow(3 * time.Minute)
	w.Record(10)
	w.Record(5)
	if got := w.Sum(); got != 15 {
		t.Fatalf("sum=%d want 15", got)
	}
}

// Disabling a signal is explicit in the configuration, without environment overrides.
func TestExplicitlyDisabledDiskSignal(t *testing.T) {
	var ups atomic.Int64
	a := autoscale.New(autoscale.Config{
		Enabled:           true,
		Role:              "bootstrap",
		DisableDiskSignal: true,
		Window:            time.Minute,
		Cooldown:          time.Hour,
		PressureHold:      time.Millisecond,
		MemLimitPct:       99,
		CPULimitPct:       99,
		MemRisePct:        1000,
		RiseHold:          time.Hour,
	})
	if got := a.Snapshot()["disk_min_free"]; got != float64(0) {
		t.Fatalf("disk_min_free=%v want 0", got)
	}
	hotDisk := autoscale.Input{
		SelfName:  "self",
		Resources: autoscale.ResourceSample{DiskFreePct: 1, DiskOK: true, MemPct: 1},
	}
	for i := 0; i < 5; i++ {
		a.Tick(hotDisk, func(autoscale.Request) error { ups.Add(1); return nil })
		time.Sleep(5 * time.Millisecond)
	}
	if ups.Load() != 0 {
		t.Fatalf("disk=0 still triggered scale-up: ups=%d reason=%v", ups.Load(), a.Snapshot()["last_reason"])
	}
}

func TestInsertWindowShortExpires(t *testing.T) {
	// Buckets are 1s when the window is under 2 minutes, so a "short"
	// window cannot be smaller than one second.
	w := autoscale.NewWindow(time.Second)
	w.Record(7)
	if got := w.Sum(); got != 7 {
		t.Fatalf("sum=%d want 7", got)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if w.Sum() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("short window did not expire, sum=%d", w.Sum())
}

func pressure(queue int) autoscale.Input {
	return autoscale.Input{Queue: queue, SelfName: "selfNode000000000000000000000000"}
}

func memHot(pct float64) autoscale.Input {
	return autoscale.Input{
		SelfName:  "selfNode000000000000000000000000",
		Resources: autoscale.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
	}
}

func underPressure() autoscale.Config {
	return autoscale.Config{
		Enabled:        true,
		Role:           "bootstrap",
		Window:         time.Minute,
		Cooldown:       time.Hour,
		PressureHold:   time.Millisecond,
		MemLimitPct:    65,
		MemFloorPct:    20,
		MemRefusePct:   60,
		DiskMinFreePct: 1,
		CPULimitPct:    99,
		MemRisePct:     1000,
		CPURisePct:     1000,
		RiseHold:       time.Hour,
	}
}

func TestAutoscaleUpTriggersOnce(t *testing.T) {
	var ups atomic.Int64
	a := autoscale.New(underPressure())

	up := func(req autoscale.Request) error { ups.Add(1); return nil }
	a.Tick(memHot(80), up)
	time.Sleep(50 * time.Millisecond)
	a.Tick(memHot(80), up)
	time.Sleep(50 * time.Millisecond)

	if got := ups.Load(); got != 1 {
		t.Fatalf("ups=%d want 1", got)
	}
}

func TestAutoscaleIgnoresWriteVolumeWithoutBacklog(t *testing.T) {
	var ups atomic.Int64
	a := autoscale.New(underPressure())

	for i := 0; i < 500_000; i++ {
		a.RecordInsert()
	}
	for i := 0; i < 5; i++ {
		a.Tick(pressure(0), func(autoscale.Request) error { ups.Add(1); return nil })
		time.Sleep(5 * time.Millisecond)
	}

	if got := ups.Load(); got != 0 {
		t.Fatalf("a node keeping up with its writes asked for %d nodes", got)
	}
}

// Without a write limit there is no queue limit: a backlog alone asks for
// nothing. With one, the queue signal fires (TestQueueLimitFollowsWriteLimit,
// TestBlockedStoreKeepsDemandVisibleAndControllerHot).
func TestAutoscaleIgnoresQueueBacklogWithoutAWriteLimit(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	a := autoscale.New(cfg)

	up := func(autoscale.Request) error { ups.Add(1); return nil }
	a.Tick(pressure(0), up)
	a.Tick(pressure(5000), up)
	time.Sleep(20 * time.Millisecond)
	a.Tick(pressure(5000), up)
	time.Sleep(50 * time.Millisecond)

	if got := ups.Load(); got != 0 {
		t.Fatalf("scaled on a queue alone: ups=%d", got)
	}
}

func TestAutoscaleWaitsForTheSignalToLast(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.PressureHold = time.Hour
	cfg.MemLimitPct = 90
	a := autoscale.New(cfg)

	up := func(autoscale.Request) error { ups.Add(1); return nil }

	a.Tick(memHot(50), up)
	time.Sleep(20 * time.Millisecond)
	a.Tick(memHot(55), up)
	time.Sleep(50 * time.Millisecond)

	if got := ups.Load(); got != 0 {
		t.Fatalf("scaled on a signal younger than the hold: ups=%d", got)
	}
}

func TestAutoscaleReportsMemoryReason(t *testing.T) {
	reason := make(chan string, 4)
	a := autoscale.New(underPressure())

	a.Tick(memHot(80), func(req autoscale.Request) error { reason <- req.Reason; return nil })
	time.Sleep(20 * time.Millisecond)

	select {
	case got := <-reason:
		if got != "mem" {
			t.Fatalf("reason=%q want mem", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a node past its memory limit never asked for a node")
	}
}

func TestAutoscaleAllowsScaleUpWhileTransferBusy(t *testing.T) {
	var ups atomic.Int64
	a := autoscale.New(underPressure())
	a.SetNearBuilder(func(reason string, spawnN int, fallback string) []string {
		return []string{"splitNearKey0000000"}
	})

	busy := memHot(80)
	busy.SelfName = "selfName0000000000"
	var gotNear atomic.Value
	a.Tick(busy, func(req autoscale.Request) error {
		ups.Add(1)
		gotNear.Store(req.PreferNear)
		return nil
	})
	time.Sleep(30 * time.Millisecond)
	if ups.Load() != 1 {
		t.Fatalf("ups=%d want 1 while transfer busy (split placement)", ups.Load())
	}
	near, _ := gotNear.Load().(string)
	if near != "splitNearKey0000000" {
		t.Fatalf("prefer_near=%q want load-split builder result", near)
	}
}

func TestAutoscalePreferNearUsesLoadSplitNotHeat(t *testing.T) {
	a := autoscale.New(underPressure())
	// Heat inserts must not drive PreferNear.
	for i := 0; i < 50; i++ {
		a.RecordInsertAt("col", "AAAA...")
	}
	a.SetNearBuilder(func(reason string, spawnN int, fallback string) []string {
		return []string{"dichotomyKey000000"}
	})
	var request atomic.Value
	a.Tick(memHot(80), func(req autoscale.Request) error {
		request.Store(req)
		return nil
	})
	time.Sleep(30 * time.Millisecond)
	got, _ := request.Load().(autoscale.Request)
	if got.PreferNear != "dichotomyKey000000" {
		t.Fatalf("prefer_near=%q want dichotomy split, not hot prefix", got.PreferNear)
	}
	if len(got.PreferNears) != 1 || got.PreferNears[0] != "dichotomyKey000000" {
		t.Fatalf("prefer_nears=%v", got.PreferNears)
	}
}

func TestAutoscaleUpSkipsFailed(t *testing.T) {
	cfg := underPressure()
	cfg.Cooldown = time.Millisecond
	a := autoscale.New(cfg)

	a.Tick(memHot(80), func(autoscale.Request) error {
		return fmt.Errorf("issuer refused")
	})
	time.Sleep(50 * time.Millisecond)
	if snap := a.Snapshot(); snap["scale_ups_done"].(int) != 0 {
		t.Fatalf("scale_ups_done=%v want 0 after failure", snap["scale_ups_done"])
	}

	a.Tick(memHot(80), func(autoscale.Request) error { return nil })
	time.Sleep(50 * time.Millisecond)
	if snap := a.Snapshot(); snap["scale_ups_done"].(int) != 1 {
		t.Fatalf("scale_ups_done=%v want 1 after success", snap["scale_ups_done"])
	}
}

func TestAutoscaleSpawnedCanScaleUp(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.Role = "spawned"
	a := autoscale.New(cfg)

	up := func(autoscale.Request) error { ups.Add(1); return nil }
	a.Tick(memHot(80), up)
	time.Sleep(50 * time.Millisecond)

	if ups.Load() != 1 {
		t.Fatalf("spawned ups=%d want 1", ups.Load())
	}
}

func TestAutoscaleCPUSaturationCounts(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.CPULimitPct = 80
	a := autoscale.New(cfg)

	hot := autoscale.Input{
		Queue:     0,
		SelfName:  "self",
		Resources: autoscale.ResourceSample{CPUPct: 92, CPUOK: true, DiskFreePct: 80, DiskOK: true},
	}
	up := func(req autoscale.Request) error {
		if req.Reason != "cpu" {
			t.Errorf("reason=%q want cpu", req.Reason)
		}
		ups.Add(1)
		return nil
	}
	a.Tick(hot, up)
	time.Sleep(20 * time.Millisecond)
	a.Tick(hot, up)
	time.Sleep(50 * time.Millisecond)

	if ups.Load() != 1 {
		t.Fatalf("ups=%d want 1 on sustained cpu saturation", ups.Load())
	}
}

func TestAutoscaleIgnoresMissingCPUReading(t *testing.T) {
	var ups atomic.Int64
	a := autoscale.New(underPressure())

	blind := autoscale.Input{
		Queue:     0,
		SelfName:  "self",
		Resources: autoscale.ResourceSample{CPUPct: 0, CPUOK: false, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 4; i++ {
		a.Tick(blind, func(autoscale.Request) error { ups.Add(1); return nil })
		time.Sleep(10 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("scaled with no cpu reading at all: ups=%d", ups.Load())
	}
}

func TestAutoscaleAsksBeforeMemoryRunsOut(t *testing.T) {
	var reason atomic.Value
	cfg := underPressure()
	cfg.MemLimitPct = 85
	cfg.MemLead = 3 * time.Minute
	cfg.MemFloorPct = 50
	cfg.MemRefusePct = 95
	a := autoscale.New(cfg)

	up := func(req autoscale.Request) error { reason.Store(req.Reason); return nil }

	for i, pct := range []float64{60, 63, 66} {
		a.Tick(autoscale.Input{
			Queue:     0,
			SelfName:  "self",
			Resources: autoscale.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up)
		if i == 0 && reason.Load() != nil {
			t.Fatal("asked on the very first memory reading, with no trend to go on")
		}
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := reason.Load(); got != "mem_filling" {
		t.Fatalf("reason=%v want mem_filling well before the limit", got)
	}
}

func TestAutoscaleIgnoresFlatMemory(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 85
	cfg.MemRefusePct = 95
	a := autoscale.New(cfg)

	flat := autoscale.Input{
		Queue:     0,
		SelfName:  "self",
		Resources: autoscale.ResourceSample{MemPct: 60, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 5; i++ {
		a.Tick(flat, func(autoscale.Request) error { ups.Add(1); return nil })
		time.Sleep(10 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("scaled on memory that never moved: ups=%d", ups.Load())
	}
}

func TestAutoscaleIgnoresAColdStartRamp(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 85
	cfg.MemFloorPct = 50
	cfg.MemRefusePct = 95
	a := autoscale.New(cfg)

	for _, pct := range []float64{2, 9, 17, 24} {
		a.Tick(autoscale.Input{
			Queue:     0,
			SelfName:  "self",
			Resources: autoscale.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, func(autoscale.Request) error { ups.Add(1); return nil })
		time.Sleep(20 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("a node booting into half an empty host asked for help: ups=%d", ups.Load())
	}
}

func TestAutoscaleHardMemTriggers(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 50
	a := autoscale.New(cfg)

	in := autoscale.Input{
		Queue:    0,
		SelfName: "self",
		Resources: autoscale.ResourceSample{
			MemPct:      90,
			DiskFreePct: 80,
			DiskOK:      true,
		},
	}
	a.Tick(in, func(req autoscale.Request) error {
		if req.Reason != "mem" {
			t.Errorf("reason=%q want mem", req.Reason)
		}
		ups.Add(1)
		return nil
	})
	time.Sleep(50 * time.Millisecond)

	if ups.Load() != 1 {
		t.Fatalf("ups=%d want 1 on hard mem", ups.Load())
	}
}

func TestAutoscaleRapidMemRiseAsksForNode(t *testing.T) {
	var reason atomic.Value
	cfg := underPressure()
	cfg.MemLimitPct = 90
	cfg.MemFloorPct = 80
	cfg.MemRisePct = 1
	cfg.RiseHold = time.Millisecond
	a := autoscale.New(cfg)

	up := func(req autoscale.Request) error { reason.Store(req.Reason); return nil }

	for i, pct := range []float64{2, 7, 12} {
		a.Tick(autoscale.Input{
			SelfName:  "self",
			Resources: autoscale.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up)
		if i == 0 && reason.Load() != nil {
			t.Fatal("asked on the first sample, with no rise rate yet")
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := reason.Load(); got != "mem_rise" {
		t.Fatalf("reason=%v want mem_rise", got)
	}
}

func TestAutoscaleFlatLowMemDoesNotRise(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 90
	cfg.MemRisePct = 1
	cfg.RiseHold = time.Millisecond
	a := autoscale.New(cfg)

	flat := autoscale.Input{
		SelfName:  "self",
		Resources: autoscale.ResourceSample{MemPct: 8, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 6; i++ {
		a.Tick(flat, func(autoscale.Request) error { ups.Add(1); return nil })
		time.Sleep(20 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("scaled on flat low memory: ups=%d", ups.Load())
	}
}

func TestAutoscaleCPURiseBelowFloorIgnored(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.CPULimitPct = 90
	cfg.CPUFloorPct = 20
	cfg.CPURisePct = 1
	cfg.RiseHold = time.Millisecond
	cfg.MemRisePct = 1000
	a := autoscale.New(cfg)

	up := func(autoscale.Request) error { ups.Add(1); return nil }
	for i, pct := range []float64{2, 8, 14} {
		a.Tick(autoscale.Input{
			SelfName: "self",
			Resources: autoscale.ResourceSample{
				CPUPct: pct, CPUOK: true, MemPct: 5, DiskFreePct: 80, DiskOK: true,
			},
		}, up)
		if i == 0 && ups.Load() != 0 {
			t.Fatal("asked on first CPU sample")
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if ups.Load() != 0 {
		t.Fatalf("cpu_rise below floor spawned: ups=%d", ups.Load())
	}
}

func TestAutoscaleRapidMemRiseAsksSpawnCountOne(t *testing.T) {
	var gotCount atomic.Int64
	var reason atomic.Value
	cfg := underPressure()
	cfg.MemLimitPct = 90
	cfg.MemFloorPct = 80
	cfg.MemRisePct = 1
	cfg.RiseHold = time.Millisecond
	cfg.MemRefusePct = 68
	a := autoscale.New(cfg)

	up := func(req autoscale.Request) error {
		reason.Store(req.Reason)
		gotCount.Store(int64(req.SpawnCount))
		return nil
	}
	for i, pct := range []float64{2, 7, 12} {
		a.Tick(autoscale.Input{
			SelfName:  "self",
			Resources: autoscale.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up)
		if i == 0 && reason.Load() != nil {
			t.Fatal("asked on the first sample")
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if reason.Load() != "mem_rise" {
		t.Fatalf("reason=%v want mem_rise", reason.Load())
	}
	if gotCount.Load() != 1 {
		t.Fatalf("spawn_count=%d want 1", gotCount.Load())
	}
}

func TestAutoscaleHardMemSpawnCount(t *testing.T) {
	cfg := underPressure()
	cfg.MemLimitPct = 55
	cfg.MemRefusePct = 68
	a := autoscale.New(cfg)

	var reason atomic.Value
	var count atomic.Int64
	up := func(req autoscale.Request) error {
		reason.Store(req.Reason)
		count.Store(int64(req.SpawnCount))
		return nil
	}
	// Below refuse: soft path only (mem_filling with hold), not instant mem.
	for _, pct := range []float64{56, 56, 56} {
		a.Tick(memHot(pct), up)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(40 * time.Millisecond)
	if reason.Load() != "mem_filling" {
		t.Fatalf("below refuse reason=%v want mem_filling", reason.Load())
	}
	if count.Load() != 1 {
		t.Fatalf("soft mem spawn_count=%d want 1", count.Load())
	}

	a = autoscale.New(cfg)
	reason = atomic.Value{}
	count.Store(0)
	// At/above refuse: instant emergency "mem".
	a.Tick(memHot(70), up)
	time.Sleep(50 * time.Millisecond)
	if reason.Load() != "mem" {
		t.Fatalf("at refuse reason=%v want mem", reason.Load())
	}
	if count.Load() != 1 {
		t.Fatalf("emergency mem spawn_count=%d want 1", count.Load())
	}
}

func TestAutoscaleMemFillingSpawnCountOne(t *testing.T) {
	cfg := underPressure()
	cfg.MemLimitPct = 55
	cfg.MemFloorPct = 20
	cfg.MemLead = time.Minute
	cfg.PressureHold = time.Millisecond
	cfg.MemRisePct = 1000
	cfg.MemRefusePct = 68
	a := autoscale.New(cfg)

	var reason atomic.Value
	var count atomic.Int64
	up := func(req autoscale.Request) error {
		reason.Store(req.Reason)
		count.Store(int64(req.SpawnCount))
		return nil
	}

	for _, pct := range []float64{30, 31, 32} {
		a.Tick(autoscale.Input{
			SelfName:  "self",
			Resources: autoscale.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up)
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if reason.Load() != "mem_filling" {
		t.Fatalf("reason=%v want mem_filling", reason.Load())
	}
	if count.Load() != 1 {
		t.Fatalf("spawn_count=%d want 1 for mem_filling", count.Load())
	}
}

func TestAutoscaleAdmissionIndependentOfProvisioning(t *testing.T) {
	cfg := underPressure()
	cfg.Cooldown = time.Hour
	a := autoscale.New(cfg)

	release := make(chan struct{})
	started := make(chan struct{})
	a.Tick(memHot(80), func(autoscale.Request) error {
		close(started)
		<-release
		return nil
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scale-up never started")
	}
	// Memory has recovered while the provisioner is still in flight.
	a.Tick(pressure(0), nil)
	if a.ClientWritesBlocked() {
		t.Fatal("provisioner blocks healthy admission")
	}
	close(release)
	time.Sleep(50 * time.Millisecond)

	if a.ClientWritesBlocked() {
		t.Fatal("cooldown blocks healthy admission")
	}
}

func TestAutoscaleAsksForOneNodePerDecision(t *testing.T) {
	var ups atomic.Int64
	a := autoscale.New(autoscale.Config{
		Enabled:        true,
		Role:           "bootstrap",
		Window:         time.Minute,
		Cooldown:       time.Hour,
		PressureHold:   time.Millisecond,
		MemLimitPct:    65,
		DiskMinFreePct: 1,
	})

	up := func(autoscale.Request) error { ups.Add(1); return nil }
	hot := autoscale.Input{
		SelfName:  "self",
		Resources: autoscale.ResourceSample{MemPct: 80, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 5; i++ {
		a.Tick(hot, up)
		time.Sleep(20 * time.Millisecond)
	}

	if n := ups.Load(); n != 1 {
		t.Fatalf("scale requests=%d want 1 while the cooldown holds", n)
	}
	if snap := a.Snapshot(); snap["scale_ups_done"].(int) != 1 {
		t.Fatalf("scale_ups_done=%v want 1", snap["scale_ups_done"])
	}
}

// ItemsOnly ignores host CPU, memory and disk readings and scales on owned
// items alone: the deterministic trigger experiments use on shared hosts.
func TestAutoscaleItemsOnlyIgnoresHostSignals(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.ItemsOnly, cfg.ItemsLimit, cfg.Cooldown = true, 100, time.Millisecond
	a := autoscale.New(cfg)
	up := func(autoscale.Request) error { ups.Add(1); return nil }
	busy := autoscale.Input{SelfName: "selfNode000000000000000000000000",
		Resources: autoscale.ResourceSample{MemPct: 39, CPUPct: 99, CPUOK: true, DiskFreePct: 1, DiskOK: true}, OwnedItems: 99}
	for i := 0; i < 5; i++ {
		a.Tick(busy, up)
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if ups.Load() != 0 {
		t.Fatalf("scaled on host signals: %d", ups.Load())
	}
	busy.OwnedItems = 100
	a.Tick(busy, up)
	time.Sleep(50 * time.Millisecond)
	if ups.Load() != 1 {
		t.Fatalf("items limit did not scale: %d", ups.Load())
	}
}

// SCALE-1: after a scale-up request, sent or timed out, the vnode does not ask
// again until a vnode near the target is active (or patience runs out).
func TestSCALE1_OneScaleUpInFlightPerVNode(t *testing.T) {
	var ups atomic.Int64
	var mu sync.Mutex
	var last string
	cfg := underPressure()
	cfg.ItemsOnly, cfg.ItemsLimit, cfg.Cooldown = true, 10, time.Millisecond
	a := autoscale.New(cfg)
	up := func(req autoscale.Request) error {
		mu.Lock()
		last = req.PreferNear
		mu.Unlock()
		ups.Add(1)
		return fmt.Errorf("provisioner: %w", context.DeadlineExceeded) // no answer: the node may have started
	}
	in := autoscale.Input{SelfName: "selfNode000000000000000000000000", OwnedItems: 50,
		Resources: autoscale.ResourceSample{MemPct: 10, DiskFreePct: 80, DiskOK: true}}
	for i := 0; i < 6; i++ {
		a.Tick(in, up)
		time.Sleep(10 * time.Millisecond)
	}
	if ups.Load() != 1 {
		t.Fatalf("%d scale-ups while the first had not landed", ups.Load())
	}
	mu.Lock()
	target, err := tree.BASE64.Decode(last)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	in.Members = []tree.VNodeDescriptor{{ID: tree.FromSlice(target)}} // the joiner is active now
	a.Tick(in, up)
	time.Sleep(20 * time.Millisecond)
	if ups.Load() != 2 {
		t.Fatalf("no new scale-up once the previous one landed: %d", ups.Load())
	}
}

// The write demand of one vnode scales up at once, also with ItemsOnly: the
// cluster follows its load, not only its data.
func TestAutoscaleWritesLimitScalesOnLoad(t *testing.T) {
	t.Parallel()
	var ups atomic.Int64
	cfg := underPressure()
	cfg.ItemsOnly, cfg.ItemsLimit, cfg.WritesLimit, cfg.Cooldown = true, 1000, 70, time.Millisecond
	a := autoscale.New(cfg)
	up := func(autoscale.Request) error { ups.Add(1); return nil }
	in := autoscale.Input{SelfName: "selfNode000000000000000000000000", OwnedItems: 10, WriteRate: 69}
	a.Tick(in, up)
	time.Sleep(30 * time.Millisecond)
	if ups.Load() != 0 {
		t.Fatalf("scaled under the write limit: %d", ups.Load())
	}
	in.WriteRate = 70
	a.Tick(in, up)
	time.Sleep(50 * time.Millisecond)
	if ups.Load() != 1 {
		t.Fatalf("write demand did not scale: %d", ups.Load())
	}
}

// SCALE-1 in a deep zone: the donor shares the target's first 64 bits, so a
// vnode already active when the scale-up was asked does not land it; only a
// new one does (AWS 2026-11-19: 8 asks, 12 vnodes for one target).
func TestSCALE1_DonorNearTheTargetDoesNotLandIt(t *testing.T) {
	t.Parallel()
	var ups atomic.Int64
	cfg := underPressure()
	cfg.ItemsOnly, cfg.WritesLimit, cfg.Cooldown = true, 10, time.Millisecond
	a := autoscale.New(cfg)
	a.SetNearBuilder(func(string, int, string) []string { return []string{"AAAAAAAAAAAAAAAA"} })
	up := func(autoscale.Request) error { ups.Add(1); return nil }
	raw, _ := tree.BASE64.Decode("AAAAAAAAAAAAAAAA")
	donor := tree.FromSlice(raw)
	in := autoscale.Input{SelfName: "AAAAAAAAAAAAAAAA", WriteRate: 100, Members: []tree.VNodeDescriptor{{ID: donor}}}
	for range 5 {
		a.Tick(in, up)
		time.Sleep(10 * time.Millisecond)
	}
	if n := ups.Load(); n != 1 {
		t.Fatalf("%d scale-ups while the first had not landed, want 1", n)
	}
	joined := donor
	joined[len(joined)-1] ^= 1
	in.Members = append(in.Members, tree.VNodeDescriptor{ID: joined})
	a.Tick(in, up)
	a.Tick(in, up)
	time.Sleep(30 * time.Millisecond)
	if n := ups.Load(); n != 2 {
		t.Fatalf("%d scale-ups after the first landed, want 2", n)
	}
}

// A donor wants the joiners it asked for while recently under pressure.
func TestWantedFollowsPressure(t *testing.T) {
	t.Parallel()
	cfg := underPressure()
	cfg.ItemsOnly, cfg.WritesLimit = true, 10
	a := autoscale.New(cfg)
	if a.Wanted() {
		t.Fatal("a vnode never under pressure wants an autoscale joiner")
	}
	a.Tick(autoscale.Input{SelfName: "AAAAAAAAAAAAAAAA", WriteRate: 100}, func(autoscale.Request) error { return nil })
	if !a.Wanted() {
		t.Fatal("a vnode under pressure refused its joiner")
	}
}

func TestWritesLimitEWMAAndHysteresisPreventOscillation(t *testing.T) {
	t.Parallel()
	var ups atomic.Int64
	cfg := underPressure()
	cfg.ItemsOnly, cfg.WritesLimit, cfg.WritesHold, cfg.Cooldown = true, 100, 40*time.Millisecond, time.Millisecond
	a := autoscale.New(cfg)
	a.SetNearBuilder(func(string, int, string) []string { return []string{"AAAAAAAAAAAAAAAA"} })
	up := func(autoscale.Request) error { ups.Add(1); return nil }

	// Noisy jitter hovering just below/above WritesLimit must not trigger a split.
	for _, rate := range []float64{85, 105, 80, 108, 78, 104} {
		a.Tick(autoscale.Input{SelfName: "AAAAAAAAAAAAAAAA", WriteRate: rate}, up)
		time.Sleep(10 * time.Millisecond)
	}
	if n := ups.Load(); n != 0 {
		t.Fatalf("noisy jitter around WritesLimit triggered %d scale-ups, want 0", n)
	}

	// Sustained demand above WritesLimit for >= RiseHold triggers cleanly.
	for range 6 {
		a.Tick(autoscale.Input{SelfName: "AAAAAAAAAAAAAAAA", WriteRate: 130}, up)
		time.Sleep(15 * time.Millisecond)
	}
	if n := ups.Load(); n != 1 {
		t.Fatalf("sustained write load triggered %d scale-ups, want 1", n)
	}
}
