package autoscale_test

import (
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Physical-resource tick simulation: couples the real autoscale.Controller
// (logical vnode splits) with finite physical machines (shared NVMe/CPU
// capacity, EC2 boot latency, warm-pool sizing, and donor anti-affinity).

type physPolicy struct {
	name             string
	startMachines    int
	maxMachines      int
	slotsPerMachine  int
	hostCapacity     float64       // sustainable writes/s of one machine (CPU/NVMe)
	bootDelay        time.Duration // EC2 boot + cloud-init latency
	sequentialBoot   bool          // legacy spawn.sh: only 1 machine boots at a time
	fixedSpareSlots  int           // legacy: fixed SPARE slots (used when spareRatio == 0)
	spareRatio       float64       // adaptive: warm spare machines = max(1, ceil(live * spareRatio))
	antiAffinity     bool          // avoid placing joiner on donor's machine when another has room
	maxActivePerHost int           // 0 = up to slotsPerMachine; >0 caps concurrent hot vnodes/host
	baseJoinLatency  time.Duration
	poisonAskedOnErr bool // reproduces legacy provisioner bug: asked[near] kept 3m after failed spawn
}

type simMachine struct {
	id         int
	slots      int
	capacity   float64
	idleSince  time.Time
	activeLoad float64
}

type physVNode struct {
	id          tree.ID
	name        string
	root        bool
	machineID   int
	weight      float64
	ctrl        *autoscale.Controller
	coldSince   time.Time
	pendingNear string
	splitSeq    uint32
}

type physJoin struct {
	donorID   tree.ID
	target    tree.ID
	machineID int
	remaining float64 // handoff work units remaining (1.0 = full handoff)
}

type physMetrics struct {
	maxMachines      int
	endMachines      int
	maxVNodes        int
	endVNodes        int
	maxVNodesPerHost int
	maxHostUtil      float64
	maxSkew          float64
	totalOffered     float64
	totalAcked       float64
	totalShed        float64
	shedTicks        int
	splits           int
	rejectedSpawns   int
	folds            int
	machinesReleased int
}

type physSim struct {
	clock       *simClock
	tick        time.Duration
	policy      physPolicy
	writesLimit float64
	writesHold  time.Duration
	cooldown    time.Duration
	coldHold    time.Duration
	idleExit    time.Duration

	machines     []*simMachine
	bootingUntil []time.Time
	vnodes       []*physVNode
	joins        []*physJoin
	asked        map[string]time.Time
	nextSeq      uint64
	nextHostID   int
	metrics      physMetrics
}

func newPhysSim(policy physPolicy, writesLimit float64, startVNodes int) *physSim {
	s := &physSim{
		clock:       newSimClock(),
		tick:        2 * time.Second,
		policy:      policy,
		writesLimit: writesLimit,
		writesHold:  10 * time.Second,
		cooldown:    15 * time.Second,
		coldHold:    30 * time.Second,
		idleExit:    60 * time.Second,
		asked:       map[string]time.Time{},
	}
	for range policy.startMachines {
		s.addMachine()
	}
	for i := range startVNodes {
		id := s.mintID(tree.ID{}, 0)
		m := s.machines[i%len(s.machines)]
		s.vnodes = append(s.vnodes, s.newVNode(id, i == 0, m.id, 1.0/float64(startVNodes)))
	}
	// Pre-warm initial spare pool if policy requests proportional warm headroom.
	s.ensureWarmPool()
	return s
}

func (s *physSim) addMachine() *simMachine {
	s.nextHostID++
	m := &simMachine{
		id:       s.nextHostID,
		slots:    s.policy.slotsPerMachine,
		capacity: s.policy.hostCapacity,
	}
	s.machines = append(s.machines, m)
	return m
}

func (s *physSim) mintID(near tree.ID, keepBits int) tree.ID {
	s.nextSeq++
	var id tree.ID
	if keepBits > 0 {
		id = near
	}
	seq := uint32(s.nextSeq)
	id[tree.Size-4] = byte(seq >> 24)
	id[tree.Size-3] = byte(seq >> 16)
	id[tree.Size-2] = byte(seq >> 8)
	id[tree.Size-1] = byte(seq)
	if keepBits == 0 {
		id[0] = byte(seq)
	}
	return id
}

func (s *physSim) newVNode(id tree.ID, root bool, machineID int, weight float64) *physVNode {
	name := tree.BASE64.Encode(id[:])
	ctrl := autoscale.New(autoscale.Config{
		Enabled:      true,
		Role:         "phys-sim",
		ItemsOnly:    true,
		WritesLimit:  s.writesLimit,
		WritesHold:   s.writesHold,
		Cooldown:     s.cooldown,
		MemRefusePct: 95,
		Now:          s.clock.Now,
	})
	v := &physVNode{
		id:        id,
		name:      name,
		root:      root,
		machineID: machineID,
		weight:    weight,
		ctrl:      ctrl,
	}
	ctrl.SetNearBuilder(func(string, int, string) []string {
		if v.pendingNear != "" {
			return []string{v.pendingNear}
		}
		v.splitSeq++
		target := id
		target[4] ^= byte(v.splitSeq)
		target[5] ^= byte(v.splitSeq >> 8)
		target[6] ^= 0x5A
		v.pendingNear = tree.BASE64.Encode(target[:])
		return []string{v.pendingNear}
	})
	return v
}

func (s *physSim) vnodesOnMachine(machineID int) int {
	n := 0
	for _, v := range s.vnodes {
		if v.machineID == machineID {
			n++
		}
	}
	for _, j := range s.joins {
		if j.machineID == machineID {
			n++
		}
	}
	return n
}

func (s *physSim) machineByID(id int) *simMachine {
	for _, m := range s.machines {
		if m.id == id {
			return m
		}
	}
	return nil
}

func (s *physSim) ensureWarmPool() {
	now := s.clock.Now()
	live := len(s.machines)
	booting := len(s.bootingUntil)
	if live+booting >= s.policy.maxMachines {
		return
	}
	if s.policy.spareRatio > 0 {
		// Adaptive warm pool: keep max(1, ceil(busyMachines * spareRatio)) empty
		// machines ready or booting, launching the deficit in parallel.
		empty := 0
		for _, m := range s.machines {
			if s.vnodesOnMachine(m.id) == 0 {
				empty++
			}
		}
		busy := live - empty
		if busy < 1 {
			busy = 1
		}
		wantSpare := max(1, int(math.Ceil(float64(busy)*s.policy.spareRatio)))
		deficit := wantSpare - (empty + booting)
		if deficit <= 0 {
			return
		}
		if live+booting+deficit > s.policy.maxMachines {
			deficit = s.policy.maxMachines - (live + booting)
		}
		for range deficit {
			s.bootingUntil = append(s.bootingUntil, now.Add(s.policy.bootDelay))
		}
		return
	}
	// Legacy spawn.sh: fixed SPARE free slots, at most ONE machine booting at a time.
	if s.policy.sequentialBoot && booting > 0 {
		return
	}
	freeSlots := 0
	for _, m := range s.machines {
		freeSlots += m.slots - s.vnodesOnMachine(m.id)
	}
	if freeSlots-1 < s.policy.fixedSpareSlots {
		s.bootingUntil = append(s.bootingUntil, now.Add(s.policy.bootDelay))
	}
}

func (s *physSim) pickMachine(donorMachineID int) (*simMachine, bool) {
	capPerHost := s.policy.slotsPerMachine
	if s.policy.maxActivePerHost > 0 && s.policy.maxActivePerHost < capPerHost {
		capPerHost = s.policy.maxActivePerHost
	}
	var best *simMachine
	bestCount := math.MaxInt
	bestLoad := math.MaxFloat64
	for pass := 0; pass < 2; pass++ {
		for _, m := range s.machines {
			if pass == 0 && s.policy.antiAffinity && m.id == donorMachineID {
				continue
			}
			used := s.vnodesOnMachine(m.id)
			if used >= capPerHost {
				continue
			}
			if used < bestCount || (used == bestCount && m.activeLoad < bestLoad) {
				best = m
				bestCount = used
				bestLoad = m.activeLoad
			}
		}
		if best != nil || !s.policy.antiAffinity {
			break
		}
	}
	return best, best != nil
}

func (s *physSim) step(totalOffered float64) {
	now := s.clock.Now()

	// 1. Finish booting EC2 instances.
	stillBooting := s.bootingUntil[:0]
	for _, readyAt := range s.bootingUntil {
		if !now.Before(readyAt) {
			s.addMachine()
		} else {
			stillBooting = append(stillBooting, readyAt)
		}
	}
	s.bootingUntil = stillBooting

	// 2. Compute per-machine physical load (writes + handoff I/O overhead).
	var weightSum float64
	for _, v := range s.vnodes {
		weightSum += v.weight
	}
	for _, m := range s.machines {
		m.activeLoad = 0
	}
	vnodeDemand := make(map[tree.ID]float64, len(s.vnodes))
	for _, v := range s.vnodes {
		d := totalOffered * (v.weight / weightSum)
		vnodeDemand[v.id] = d
		if m := s.machineByID(v.machineID); m != nil {
			m.activeLoad += d
		}
	}
	// Each in-flight handoff adds ~25% of one vnode's I/O pressure on both donor and receiver hosts.
	for _, j := range s.joins {
		for _, v := range s.vnodes {
			if v.id == j.donorID {
				if dm := s.machineByID(v.machineID); dm != nil {
					dm.activeLoad += 0.25 * s.writesLimit
				}
				break
			}
		}
		if rm := s.machineByID(j.machineID); rm != nil {
			rm.activeLoad += 0.25 * s.writesLimit
		}
	}

	// 3. Account offered / acked / shed based on finite physical machine capacity.
	var ackedTick float64
	for _, m := range s.machines {
		util := m.activeLoad / m.capacity
		if util > s.metrics.maxHostUtil {
			s.metrics.maxHostUtil = util
		}
		used := s.vnodesOnMachine(m.id)
		if used > s.metrics.maxVNodesPerHost {
			s.metrics.maxVNodesPerHost = used
		}
		// Writes served by this machine (capped at physical capacity; overload degrades throughput).
		var hostWrites float64
		for _, v := range s.vnodes {
			if v.machineID == m.id {
				hostWrites += vnodeDemand[v.id]
			}
		}
		if m.activeLoad <= m.capacity {
			ackedTick += hostWrites
		} else {
			// Contention penalty: when overloaded, shared NVMe fsync/bbolt thrashing
			// reduces effective throughput below raw capacity (nonlinear collapse).
			overload := m.activeLoad / m.capacity
			effCap := m.capacity / math.Pow(overload, 0.5)
			if m.activeLoad > 0 {
				ackedTick += hostWrites * math.Min(1.0, effCap/m.activeLoad)
			}
		}
	}
	shedTick := math.Max(0, totalOffered-ackedTick)
	s.metrics.totalOffered += totalOffered * s.tick.Seconds()
	s.metrics.totalAcked += ackedTick * s.tick.Seconds()
	s.metrics.totalShed += shedTick * s.tick.Seconds()
	if shedTick > 0.05*totalOffered && totalOffered > 0 {
		s.metrics.shedTicks++
	}

	// 4. Advance in-flight handoffs; speed slows quadratically with host saturation.
	remainingJoins := s.joins[:0]
	for _, j := range s.joins {
		var donor *physVNode
		for _, v := range s.vnodes {
			if v.id == j.donorID {
				donor = v
				break
			}
		}
		if donor == nil {
			continue
		}
		dUtil, rUtil := 1.0, 1.0
		if dm := s.machineByID(donor.machineID); dm != nil {
			dUtil = math.Max(1.0, dm.activeLoad/dm.capacity)
		}
		if rm := s.machineByID(j.machineID); rm != nil {
			rUtil = math.Max(1.0, rm.activeLoad/rm.capacity)
		}
		slowdown := math.Pow(math.Max(dUtil, rUtil), 2)
		progress := (s.tick.Seconds() / s.policy.baseJoinLatency.Seconds()) / slowdown
		j.remaining -= progress
		if j.remaining <= 0 {
			half := donor.weight * 0.5
			donor.weight = half
			donor.pendingNear = ""
			s.vnodes = append(s.vnodes, s.newVNode(j.target, false, j.machineID, half))
		} else {
			remainingJoins = append(remainingJoins, j)
		}
	}
	s.joins = remainingJoins

	// Track skew = max(weight) / mean(weight) across active vnodes.
	if len(s.vnodes) > 0 && weightSum > 0 {
		var maxW float64
		for _, v := range s.vnodes {
			if v.weight > maxW {
				maxW = v.weight
			}
		}
		meanW := weightSum / float64(len(s.vnodes))
		if skew := maxW / meanW; skew > s.metrics.maxSkew {
			s.metrics.maxSkew = skew
		}
	}

	// 5. Tick every vnode's real autoscale.Controller.
	active := make([]tree.VNodeDescriptor, len(s.vnodes))
	for i, v := range s.vnodes {
		active[i] = tree.VNodeDescriptor{ID: v.id, Name: v.name}
	}
	for _, v := range s.vnodes {
		var triggered bool
		var rejected bool
		v.ctrl.Tick(autoscale.Input{
			SelfName:  v.name,
			WriteRate: vnodeDemand[v.id],
			Members:   active,
		}, func(req autoscale.Request) error {
			// Provisioner target idempotency (s.asked with 3m targetPatience).
			if at, ok := s.asked[req.PreferNear]; ok && now.Sub(at) < 3*time.Minute {
				return nil // "already_starting": HTTP 200 OK, started=0!
			}
			s.asked[req.PreferNear] = now
			s.ensureWarmPool()
			m, ok := s.pickMachine(v.machineID)
			if !ok {
				if !s.policy.poisonAskedOnErr {
					delete(s.asked, req.PreferNear)
				}
				rejected = true
				return errNoCapacity
			}
			triggered = true
			raw, _ := tree.BASE64.Decode(req.PreferNear)
			target := s.mintID(tree.FromSlice(raw), 64)
			s.joins = append(s.joins, &physJoin{
				donorID:   v.id,
				target:    target,
				machineID: m.id,
				remaining: 1.0,
			})
			return nil
		})
		for v.ctrl.Snapshot()["up_in_flight"].(bool) {
			runtime.Gosched()
		}
		if rejected {
			s.metrics.rejectedSpawns++
		}
		if triggered {
			s.metrics.splits++
		}
	}

	// Keep warm pool topped up proactively even between spawns.
	if s.policy.spareRatio > 0 {
		s.ensureWarmPool()
	}

	// 6. Symmetric scale-down: COLD-2 folds + release excess idle machines.
	if len(s.vnodes) > 1 && len(s.joins) == 0 {
		for i := len(s.vnodes) - 1; i >= 1; i-- {
			v := s.vnodes[i]
			if vnodeDemand[v.id] < 0.25*s.writesLimit {
				if v.coldSince.IsZero() {
					v.coldSince = now
				}
			} else {
				v.coldSince = time.Time{}
			}
			if !v.coldSince.IsZero() && now.Sub(v.coldSince) >= s.coldHold {
				survivor := s.vnodes[i-1]
				if vnodeDemand[v.id]+vnodeDemand[survivor.id] <= 0.45*s.writesLimit {
					survivor.weight += v.weight
					s.vnodes = append(s.vnodes[:i], s.vnodes[i+1:]...)
					s.metrics.folds++
					break
				}
			}
		}
	}
	if s.policy.spareRatio > 0 {
		busy := 0
		for _, m := range s.machines {
			if s.vnodesOnMachine(m.id) > 0 {
				busy++
				m.idleSince = time.Time{}
			} else if m.idleSince.IsZero() {
				m.idleSince = now
			}
		}
		wantSpare := max(1, int(math.Ceil(float64(max(1, busy))*s.policy.spareRatio)))
		kept := s.machines[:0]
		emptyKept := 0
		for _, m := range s.machines {
			if s.vnodesOnMachine(m.id) > 0 {
				kept = append(kept, m)
				continue
			}
			if emptyKept < wantSpare || now.Sub(m.idleSince) < s.idleExit {
				kept = append(kept, m)
				emptyKept++
			} else {
				s.metrics.machinesReleased++
			}
		}
		s.machines = kept
	}

	if len(s.machines) > s.metrics.maxMachines {
		s.metrics.maxMachines = len(s.machines)
	}
	if len(s.vnodes) > s.metrics.maxVNodes {
		s.metrics.maxVNodes = len(s.vnodes)
	}
	s.metrics.endMachines = len(s.machines)
	s.metrics.endVNodes = len(s.vnodes)
	s.clock.Advance(s.tick)
}

var errNoCapacity = &noCapErr{}

type noCapErr struct{}

func (*noCapErr) Error() string { return "no free host capacity (machine booting)" }

func (s *physSim) run(duration time.Duration, rateFn func(elapsed time.Duration) float64) {
	steps := int(duration / s.tick)
	for i := 0; i < steps; i++ {
		s.step(rateFn(time.Duration(i) * s.tick))
	}
}

// TestPhysSim_LegacyReproducesAWSCollapseVsAdaptive verifies that our physical
// tick simulation reproduces the AWS exp-20261122-201005 overload under the
// legacy policy (sequential boot + stacking 4 vnodes/NVMe) and shows that
// proportional warm headroom + anti-affinity eliminates shedding and thrashing.
func TestPhysSim_LegacyReproducesAWSCollapseVsAdaptive(t *testing.T) {
	t.Parallel()
	// Steps profile matching AWS MODE=steps (2k -> 4k -> 8k -> 16k over 12 min).
	stepsLoad := func(elapsed time.Duration) float64 {
		switch {
		case elapsed < 2*time.Minute:
			return 2000
		case elapsed < 5*time.Minute:
			return 4000
		case elapsed < 8*time.Minute:
			return 8000
		default:
			return 16000
		}
	}

	legacy := physPolicy{
		name:            "legacy_seq_boot_stack4",
		startMachines:   2,
		maxMachines:     24,
		slotsPerMachine: 4,
		hostCapacity:    1500, // one i4i.xlarge NVMe sustainable write rate
		bootDelay:       70 * time.Second,
		sequentialBoot:  true,
		fixedSpareSlots: 4,
		baseJoinLatency: 25 * time.Second,
	}
	simLegacy := newPhysSim(legacy, 1000, 4)
	simLegacy.run(12*time.Minute, stepsLoad)
	legacyShedPct := 100 * simLegacy.metrics.totalShed / simLegacy.metrics.totalOffered

	adaptive := physPolicy{
		name:             "adaptive_warm_pool_1_per_host",
		startMachines:    4,
		maxMachines:      32,
		slotsPerMachine:  4,
		hostCapacity:     1500,
		bootDelay:        70 * time.Second,
		spareRatio:       0.50,
		antiAffinity:     true,
		maxActivePerHost: 1, // 1 hot vnode per NVMe -> split at 60% host capacity leaves 40% I/O headroom for handoff
		baseJoinLatency:  25 * time.Second,
	}
	simAdaptive := newPhysSim(adaptive, 900, 4)
	simAdaptive.run(12*time.Minute, stepsLoad)
	adaptiveShedPct := 100 * simAdaptive.metrics.totalShed / simAdaptive.metrics.totalOffered

	t.Logf("LEGACY   : machines=%d/%d vnodes=%d maxPerHost=%d maxUtil=%.2fx shed=%.1f%% (%d ticks) splits=%d",
		simLegacy.metrics.endMachines, simLegacy.metrics.maxMachines, simLegacy.metrics.endVNodes,
		simLegacy.metrics.maxVNodesPerHost, simLegacy.metrics.maxHostUtil, legacyShedPct, simLegacy.metrics.shedTicks, simLegacy.metrics.splits)
	t.Logf("ADAPTIVE : machines=%d/%d vnodes=%d maxPerHost=%d maxUtil=%.2fx shed=%.1f%% (%d ticks) splits=%d deferred=%d",
		simAdaptive.metrics.endMachines, simAdaptive.metrics.maxMachines, simAdaptive.metrics.endVNodes,
		simAdaptive.metrics.maxVNodesPerHost, simAdaptive.metrics.maxHostUtil, adaptiveShedPct, simAdaptive.metrics.shedTicks, simAdaptive.metrics.splits, simAdaptive.metrics.rejectedSpawns)

	// Legacy MUST show severe host overload & shedding (reproducing AWS).
	if simLegacy.metrics.maxHostUtil < 2.0 || legacyShedPct < 10.0 {
		t.Fatalf("legacy model failed to reproduce AWS overload: maxUtil=%.2fx shed=%.1f%%", simLegacy.metrics.maxHostUtil, legacyShedPct)
	}
	// Adaptive MUST dramatically reduce overload and shedding.
	if adaptiveShedPct >= 5.0 {
		t.Fatalf("adaptive shed=%.1f%% want < 5%% (legacy=%.1f%%)", adaptiveShedPct, legacyShedPct)
	}
	if simAdaptive.metrics.maxVNodesPerHost > 1 {
		t.Fatalf("adaptive stacked %d vnodes on one host, want <= 1", simAdaptive.metrics.maxVNodesPerHost)
	}
}

// TestPhysSim_ElasticRampAndShrinkSymmetry checks a full exponential ramp
// (doubling every 120 s) followed by calm (COLD-2 + idle machine release).
func TestPhysSim_ElasticRampAndShrinkSymmetry(t *testing.T) {
	t.Parallel()
	policy := physPolicy{
		name:             "adaptive_ramp_shrink",
		startMachines:    4,
		maxMachines:      32,
		slotsPerMachine:  4,
		hostCapacity:     1500,
		bootDelay:        70 * time.Second,
		spareRatio:       0.50,
		antiAffinity:     true,
		maxActivePerHost: 1,
		baseJoinLatency:  25 * time.Second,
	}
	sim := newPhysSim(policy, 900, 4)
	sim.run(14*time.Minute, func(elapsed time.Duration) float64 {
		switch {
		case elapsed < 6*time.Minute:
			// Exponential ramp: 2000 * 2^(t / 120s) -> 2k -> 4k -> 8k -> 16k.
			return 2000 * math.Pow(2, elapsed.Seconds()/120.0)
		default:
			// Calm: cluster folds vnodes (COLD-2) and releases excess empty machines.
			return 200
		}
	})
	shedPct := 100 * sim.metrics.totalShed / sim.metrics.totalOffered
	t.Logf("RAMP+SHRINK: peakMachines=%d endMachines=%d released=%d peakVNodes=%d endVNodes=%d folds=%d maxUtil=%.2fx shed=%.1f%%",
		sim.metrics.maxMachines, sim.metrics.endMachines, sim.metrics.machinesReleased,
		sim.metrics.maxVNodes, sim.metrics.endVNodes, sim.metrics.folds, sim.metrics.maxHostUtil, shedPct)

	if sim.metrics.machinesReleased == 0 {
		t.Fatalf("no idle machines released after calm phase (endMachines=%d)", sim.metrics.endMachines)
	}
	if sim.metrics.endMachines > 3 {
		t.Fatalf("too many machines left after 8m calm: endMachines=%d", sim.metrics.endMachines)
	}
	if sim.metrics.endVNodes > 2 {
		t.Fatalf("too many vnodes left after 8m calm: endVNodes=%d", sim.metrics.endVNodes)
	}
}

// TestPhysSim_FailedSpawnStarvationReproducesSkew5x reproduces the exact
// AWS exp-20261122-203248 failure: with 6 hosts (4 vnodes + 2 warm spares),
// vnodes #3 and #4 hit "no free slot" while EC2 boots. When the provisioner
// keeps failed targets in asked[near] for 3m, those quadrants starve and skew
// explodes to 3-5x; clearing asked on failed spawn keeps skew <= 2.0 and shed 0%.
func TestPhysSim_FailedSpawnStarvationReproducesSkew5x(t *testing.T) {
	t.Parallel()
	stepsLoad := func(elapsed time.Duration) float64 {
		switch {
		case elapsed < 2*time.Minute:
			return 2000
		case elapsed < 5*time.Minute:
			return 4000
		default:
			return 8000
		}
	}
	base := physPolicy{
		startMachines:    6, // 4 initial vnodes + 2 warm spares (exact AWS 203248 setup)
		maxMachines:      32,
		slotsPerMachine:  1,
		hostCapacity:     1500,
		bootDelay:        70 * time.Second,
		spareRatio:       0.50,
		antiAffinity:     true,
		maxActivePerHost: 1,
		baseJoinLatency:  20 * time.Second,
	}
	poisoned := base
	poisoned.poisonAskedOnErr = true
	simPoison := newPhysSim(poisoned, 900, 4)
	simPoison.run(8*time.Minute, stepsLoad)
	poisonShed := 100 * simPoison.metrics.totalShed / simPoison.metrics.totalOffered

	clean := base
	clean.poisonAskedOnErr = false
	simClean := newPhysSim(clean, 900, 4)
	simClean.run(8*time.Minute, stepsLoad)
	cleanShed := 100 * simClean.metrics.totalShed / simClean.metrics.totalOffered

	t.Logf("POISONED asked : vnodes=%d maxSkew=%.2fx maxUtil=%.2fx shed=%.1f%%",
		simPoison.metrics.endVNodes, simPoison.metrics.maxSkew, simPoison.metrics.maxHostUtil, poisonShed)
	t.Logf("CLEAN asked    : vnodes=%d maxSkew=%.2fx maxUtil=%.2fx shed=%.1f%%",
		simClean.metrics.endVNodes, simClean.metrics.maxSkew, simClean.metrics.maxHostUtil, cleanShed)

	if simPoison.metrics.maxSkew < 2.2 || poisonShed < 5.0 {
		t.Fatalf("poisoned asked did not reproduce starvation: maxSkew=%.2fx shed=%.1f%%", simPoison.metrics.maxSkew, poisonShed)
	}
	if simClean.metrics.maxSkew > 1.6 {
		t.Fatalf("clean asked still had high skew=%.2fx (want <= 1.6)", simClean.metrics.maxSkew)
	}
	if cleanShed > 0.5 {
		t.Fatalf("clean asked shed=%.1f%% (want <= 0.5%%)", cleanShed)
	}
}
