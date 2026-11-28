package autoscale

import (
	"strings"
	"testing"
	"time"
)

// machine is a 4-core host with two slots and plenty of memory and disk.
func machine(name string, running, receiving int, cpu float64, vnodes ...string) MachineFacts {
	return MachineFacts{Machine: name, Running: running, Free: 2 - running, Receiving: receiving, VNodes: vnodes,
		Cores: 4, CPUPct: cpu, CPUOK: true, MemFreePct: 80, MemOK: true, DiskFreePct: 80, DiskOK: true}
}

func with(m MachineFacts, change func(*MachineFacts)) MachineFacts {
	change(&m)
	return m
}

func TestPlace(t *testing.T) {
	// On 4 cores a receiver is budgeted 1.75 cores = 43.75 %: a machine is a
	// target up to 60 - 43.75 = 16.25 % CPU.
	cases := []struct {
		name     string
		machines []MachineFacts
		donor    string
		canBoot  bool
		kind     string
		machine  string
		reason   string // must be part of the reason
	}{
		{name: "nothing reported", canBoot: true, reason: "no machine reported"},
		{
			name:     "an empty machine before the donor machine, even with headroom there",
			machines: []MachineFacts{machine("a", 1, 0, 15, "hot"), machine("b", 0, 0, 0)},
			donor:    "hot", canBoot: true, kind: PlaceNew, machine: "b", reason: "empty machine",
		},
		{
			name:     "never local while a machine can boot: a local split moves no load off the machine",
			machines: []MachineFacts{machine("a", 1, 0, 5, "hot")},
			donor:    "hot", canBoot: true, kind: PlaceNew, machine: "", reason: "wait for a new machine",
		},
		{
			name:     "local only when nothing can boot",
			machines: []MachineFacts{machine("a", 1, 0, 15, "hot")},
			donor:    "hot", canBoot: false, kind: PlaceLocal, machine: "a", reason: "donor machine keeps headroom",
		},
		{
			name:     "a hot donor machine is skipped for an empty machine",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"), machine("b", 0, 0, 0)},
			donor:    "hot", canBoot: true, kind: PlaceNew, machine: "b", reason: "empty machine",
		},
		{
			name:     "an empty machine is preferred to a busy one with headroom",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"), machine("b", 1, 0, 5), machine("c", 0, 0, 0)},
			donor:    "hot", canBoot: true, kind: PlaceNew, machine: "c",
		},
		{
			name:     "without an empty machine, the least loaded with headroom",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"), machine("b", 1, 0, 15), machine("c", 1, 0, 10)},
			donor:    "hot", canBoot: true, kind: PlaceExisting, machine: "c", reason: "least loaded",
		},
		{
			name:     "not hot, but hot with a receiver: skipped",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"), machine("b", 1, 0, 40)},
			donor:    "hot", canBoot: true, kind: PlaceNew, machine: "", reason: "wait for a new machine",
		},
		{
			name:     "no second receiver on a machine while the first still joins",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"), machine("b", 1, 1, 5)},
			donor:    "hot", canBoot: true, kind: PlaceNew, machine: "", reason: "wait for a new machine",
		},
		{
			name:     "a joining vnode on the donor machine forbids local",
			machines: []MachineFacts{machine("a", 1, 1, 5, "hot"), machine("b", 1, 0, 10)},
			donor:    "hot", canBoot: true, kind: PlaceExisting, machine: "b",
		},
		{
			name: "low disk is no target",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"),
				with(machine("b", 0, 0, 0), func(m *MachineFacts) { m.DiskFreePct = 20 }), machine("c", 1, 0, 10)},
			donor: "hot", canBoot: true, kind: PlaceExisting, machine: "c",
		},
		{
			name: "low memory is no target",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"),
				with(machine("b", 0, 0, 0), func(m *MachineFacts) { m.MemFreePct = 30 }), machine("c", 1, 0, 10)},
			donor: "hot", canBoot: true, kind: PlaceExisting, machine: "c",
		},
		{
			name: "an unknown memory or disk reading is not checked",
			machines: []MachineFacts{machine("a", 1, 0, 49, "hot"),
				with(machine("b", 0, 0, 0), func(m *MachineFacts) { m.MemOK, m.MemFreePct, m.DiskOK, m.DiskFreePct = false, 0, false, 0 })},
			donor: "hot", canBoot: true, kind: PlaceNew, machine: "b",
		},
		{
			name:     "a full machine is no target",
			machines: []MachineFacts{machine("a", 2, 0, 10, "hot", "other"), machine("b", 1, 0, 10)},
			donor:    "hot", canBoot: true, kind: PlaceExisting, machine: "b",
		},
		{
			name:     "more cores make the same receiver cheaper",
			machines: []MachineFacts{with(machine("a", 1, 0, 49, "hot"), func(m *MachineFacts) { m.Cores = 16 })},
			donor:    "hot", canBoot: false, kind: PlaceLocal, machine: "a",
		},
		{
			name:     "unknown donor: never local, still placed",
			machines: []MachineFacts{machine("a", 1, 0, 5, "x"), machine("b", 1, 0, 20, "y")},
			donor:    "elsewhere", canBoot: true, kind: PlaceExisting, machine: "a",
		},
		{
			name:     "cap reached: does not bypass CPU headroom",
			machines: []MachineFacts{machine("a", 1, 0, 60, "hot"), machine("b", 1, 0, 40), machine("c", 1, 1, 10)},
			donor:    "hot", canBoot: false, kind: "", machine: "", reason: "machine cap reached",
		},
		{
			name:     "cap reached: does not admit a second receiver",
			machines: []MachineFacts{machine("a", 2, 0, 60, "hot", "other"), machine("c", 1, 1, 10)},
			donor:    "hot", canBoot: false, kind: "", machine: "", reason: "machine cap reached",
		},
		{
			name: "cap reached: does not use an overloaded donor instead of a short disk",
			machines: []MachineFacts{machine("a", 1, 0, 60, "hot"),
				with(machine("b", 0, 0, 0), func(m *MachineFacts) { m.DiskFreePct = 5 })},
			donor: "hot", canBoot: false, kind: "", machine: "", reason: "machine cap reached",
		},
		{
			name: "every machine short of disk, the empty one too: booting does not help",
			machines: []MachineFacts{with(machine("a", 1, 0, 60, "hot"), func(m *MachineFacts) { m.DiskFreePct = 6 }),
				with(machine("b", 0, 0, 0), func(m *MachineFacts) { m.DiskFreePct = 6 })},
			donor: "hot", canBoot: true, kind: "", machine: "", reason: "an empty machine is already short",
		},
		{
			name:     "cap reached and no slot: nothing is placed",
			machines: []MachineFacts{machine("a", 2, 0, 60, "hot", "other")},
			donor:    "hot", canBoot: false, kind: "", machine: "", reason: "no free slot",
		},
		{
			name: "CPU missing on one machine: use the measured spare",
			machines: []MachineFacts{machine("a", 1, 0, 5, "hot"), {Machine: "old", Running: 0, Free: 2},
				machine("c", 0, 0, 0)},
			donor: "hot", canBoot: true, kind: PlaceNew, machine: "c", reason: "empty machine",
		},
		{
			name:     "unknown CPU cannot bypass another machine's projected CPU limit at cap",
			machines: []MachineFacts{machine("a", 1, 0, 50, "hot"), {Machine: "unknown", Free: 5}},
			donor:    "hot", canBoot: false, reason: "machine cap reached",
		},
		{
			name:     "facts missing and no slot",
			machines: []MachineFacts{{Machine: "old", Running: 2, Free: 0}},
			donor:    "hot", canBoot: true, kind: "", machine: "", reason: "no free slot",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Place(c.machines, c.donor, c.canBoot)
			if got.Kind != c.kind || got.Machine != c.machine || !strings.Contains(got.Reason, c.reason) || got.Reason == "" {
				t.Fatalf("Place = %+v, want kind %q on %q with reason containing %q", got, c.kind, c.machine, c.reason)
			}
		})
	}
}

// The run of 2026-11-26: machines of 4 vCPU and 2 slots, donors at 28-49 %,
// three spare machines up and empty. No machine may get two receivers while
// a spare is free. Each donor gets a spare, the one at 15 % too: a receiver
// on its own machine would move nothing off it. The fourth request finds
// no spare left and takes the machine at 15 %, which still has headroom.
func TestPlaceSpreadsReceiversOverSpares(t *testing.T) {
	machines := []MachineFacts{
		machine("d1", 1, 0, 49, "v1"), machine("d2", 1, 0, 15, "v2"), machine("d3", 1, 0, 35, "v3"),
		machine("s1", 0, 0, 0), machine("s2", 0, 0, 0), machine("s3", 0, 0, 0),
	}
	want := []Placement{{Kind: PlaceNew, Machine: "s1"}, {Kind: PlaceNew, Machine: "s2"}, {Kind: PlaceNew, Machine: "s3"}, {Kind: PlaceExisting, Machine: "d2"}}
	for n, donor := range []string{"v1", "v2", "v3", "v1"} {
		got := Place(machines, donor, true)
		if got.Kind != want[n].Kind || got.Machine != want[n].Machine {
			t.Fatalf("request %d by %s: %+v, want %+v", n, donor, got, want[n])
		}
		for i := range machines {
			if machines[i].Machine != got.Machine {
				continue
			}
			if machines[i].Receiving > 0 {
				t.Fatalf("donor %s: second receiver on %s", donor, got.Machine)
			}
			machines[i].Running++
			machines[i].Free--
			machines[i].Receiving++
		}
	}
	// One more request finds every machine paying for a join: it waits.
	if got := Place(machines, "v3", true); got.Kind != PlaceNew || got.Machine != "" {
		t.Fatalf("last request: %+v", got)
	}
}

// A paid-machine cap must never turn an inadmissible target into a target.
func TestSixtyPercentHeadroomAtBoundary(t *testing.T) {
	for _, canBoot := range []bool{false, true} {
		for _, cpu := range []float64{16.25, 16.251} {
			// Without boots the donor's own machine is the target; with
			// boots, another machine at the same CPU.
			machines := []MachineFacts{machine("a", 1, 0, cpu, "donor")}
			if canBoot {
				machines = []MachineFacts{machine("d", 1, 0, 90, "donor"), machine("a", 1, 0, cpu, "other")}
			}
			got := Place(machines, "donor", canBoot)
			if cpu == 16.25 && got.Machine != "a" {
				t.Fatalf("exactly60%% refused: %+v", got)
			}
			if cpu > 16.25 && got.Machine != "" {
				t.Fatalf("over60%% admitted with canBoot=%v: %+v", canBoot, got)
			}
		}
	}
	a := New(Config{})
	c := a.Config()
	if c.CPULimitPct != 60 || c.DiskMinFreePct != 40 {
		t.Fatalf("default headroom: cpu=%v diskfree=%v", c.CPULimitPct, c.DiskMinFreePct)
	}
}

// itemized sets the owned items of each vnode of the machine, in its order.
func itemized(m MachineFacts, items ...int64) MachineFacts {
	m.Items = map[string]int64{}
	for i, name := range m.VNodes {
		m.Items[name] = items[i]
	}
	return m
}

// The run of 2026-11-26 at 4k/s: one machine owned 26 % of the items with
// three vnodes, and received 25 % of the writes. A receiver goes where it
// reduces the peak of the donor and receiver together.
func TestPlaceRelievesTheDonorMachine(t *testing.T) {
	five := func(name string, running int, cpu float64, vnodes ...string) MachineFacts {
		return with(machine(name, running, 0, cpu, vnodes...), func(m *MachineFacts) { m.Free = 5 - running })
	}
	hot := itemized(five("hot", 3, 10, "v1", "v2", "v3"), 89_815, 81_268, 51_968)
	cases := []struct {
		name    string
		others  []MachineFacts
		canBoot bool
		kind    string
		machine string
	}{
		{"the lighter machine relieves it", []MachineFacts{itemized(five("light", 1, 10, "w"), 50_000)}, true, PlaceExisting, "light"},
		{"receiver can end heavier when the pair peak still falls", []MachineFacts{itemized(five("heavy", 1, 10, "w"), 140_000)}, true, PlaceExisting, "heavy"},
		{"the least loaded by items, not by CPU", []MachineFacts{
			itemized(five("busy-cpu", 1, 12, "w"), 10_000), itemized(five("idle-cpu", 1, 2, "x"), 60_000)}, true, PlaceExisting, "busy-cpu"},
		{"at the machine cap, the donor machine itself", []MachineFacts{itemized(five("heavy", 1, 10, "w"), 180_000)}, false, PlaceLocal, "hot"},
		{"an item count missing: the rule is off", []MachineFacts{five("heavy", 1, 10, "w")}, true, PlaceExisting, "heavy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Place(append([]MachineFacts{hot}, c.others...), "v1", c.canBoot)
			if got.Kind != c.kind || got.Machine != c.machine {
				t.Fatalf("Place = %+v, want %q on %q", got, c.kind, c.machine)
			}
		})
	}
}

func TestPlaceHalfSplitLowersPeakPerCore(t *testing.T) {
	const large = int64(1<<63 - 1)
	for _, test := range []struct {
		name                    string
		donor, moving, target   int64
		donorCores, targetCores int
		accept                  bool
	}{
		{"peak crosses to target", 100, 50, 65, 4, 4, true},
		{"equal to old peak", 100, 50, 75, 4, 4, false},
		{"no traffic moved", 100, 0, 0, 4, 4, false},
		{"budget larger odd half", 100, 51, 74, 4, 4, false},
		{"larger host absorbs more", 100, 50, 174, 4, 8, true},
		{"smaller host would saturate", 100, 50, 25, 8, 4, false},
		{"near max strict", large, 2, large - 2, 4, 4, true},
		{"near max equal", large, 2, large - 1, 4, 4, false},
		{"target sum beyond int64", large, large, large, 4, 8, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			donor := itemized(machine("donor", 2, 0, 50, "hot", "other"), test.moving, test.donor-test.moving)
			target := itemized(machine("target", 1, 0, 5, "peer"), test.target)
			donor.Cores, target.Cores = test.donorCores, test.targetCores
			got := Place([]MachineFacts{donor, target}, "hot", true)
			if (got.Machine == target.Machine) != test.accept {
				t.Fatalf("Place = %+v, expected remote relief %v", got, test.accept)
			}
		})
	}
}

// One vnode per machine and equal halves: moving half of one onto another
// machine leaves that machine heavier than the donor was. Only an empty
// machine relieves it.
func TestPlaceOneVNodePerMachineWaitsForAnEmptyOne(t *testing.T) {
	machines := []MachineFacts{itemized(machine("a", 1, 0, 10, "v1"), 60_000), itemized(machine("b", 1, 0, 5, "v2"), 55_000)}
	if got := Place(machines, "v1", true); got.Kind != PlaceNew || got.Machine != "" {
		t.Fatalf("Place = %+v, want a wait for a new machine", got)
	}
	machines = append(machines, machine("c", 0, 0, 0))
	if got := Place(machines, "v1", true); got.Kind != PlaceNew || got.Machine != "c" {
		t.Fatalf("Place = %+v, want the empty machine c", got)
	}
}

func TestPlacePacksUsefulRemoteCapacityBeforeSpare(t *testing.T) {
	donor := itemized(machine("donor", 2, 0, 10, "hot", "cold"), 100, 200)
	target := itemized(machine("target", 1, 0, 5, "light"), 20)
	spare := machine("spare", 0, 0, 0)
	if got := Place([]MachineFacts{donor, target, spare}, "hot", true); got.Kind != PlaceExisting || got.Machine != "target" {
		t.Fatalf("wasted spare despite useful remote headroom: %+v", got)
	}
	unknown := machine("unrelated", 1, 1, 0, "not-counted")
	if got := Place([]MachineFacts{donor, target, spare, unknown}, "hot", true); got.Machine != "target" {
		t.Fatalf("unrelated unknown host disabled a known relief: %+v", got)
	}
	// Incomplete load information retains conservative empty-host preference.
	delete(target.Items, "light")
	if got := Place([]MachineFacts{donor, target, spare}, "hot", true); got.Machine != "spare" {
		t.Fatalf("packed without a complete load observation: %+v", got)
	}
}

func TestPlacementUnknownCountsUseOneTransitiveOrder(t *testing.T) {
	a := itemized(machine("a", 1, 0, 10, "a"), 100)
	b := itemized(machine("b", 1, 0, 0, "b"), 200)
	c := machine("c", 1, 0, 5, "c") // unknown: all candidates use CPU order
	for _, ms := range [][]MachineFacts{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}} {
		if got := pick(ms, func(MachineFacts) bool { return true }); got == nil || got.Machine != "b" {
			t.Fatalf("input order changed selection: %+v", got)
		}
	}
	started := itemized(machine("pending", 1, 0, 1), 0)
	if _, known := started.load(); known {
		t.Fatal("unidentified running vnode counted as empty")
	}
}

func TestPlaceCanUseFifthVNodeSlotOnAnotherMachine(t *testing.T) {
	donor := MachineFacts{Machine: "donor", Running: 5, VNodes: []string{"d1", "d2", "d3", "d4", "d5"}, Cores: 4, CPUOK: true, CPUPct: 10}
	donor = itemized(donor, 100, 100, 100, 100, 100)
	target := MachineFacts{Machine: "target", Running: 4, Free: 1, VNodes: []string{"t1", "t2", "t3", "t4"}, Cores: 4, CPUOK: true, CPUPct: 10}
	target = itemized(target, 5, 5, 5, 5)
	if got := Place([]MachineFacts{donor, target, machine("spare", 0, 0, 0)}, "d1", true); got.Machine != "target" {
		t.Fatalf("did not reuse fifth slot with physical headroom: %+v", got)
	}
}

func TestKnownRemoteRanksTrafficOnlyAcrossCompleteAlternatives(t *testing.T) {
	now := time.Now()
	donor := machine("donor", 2, 0, 70, "hot", "other")
	donor.Items = map[string]int64{"hot": 100, "other": 500}
	candidate := func(name string, items int64, rate float64) MachineFacts {
		m := machine(name, 1, 0, 5, name)
		m.Items = map[string]int64{name: items}
		m.AppliedRate = &AppliedRate{PerSecond: rate, AsOf: now}
		return m
	}
	lowItems := candidate("low-items", 20, 200)
	lowRate := candidate("low-rate", 80, 50)
	check := func(want string, group ...MachineFacts) {
		t.Helper()
		p := Place(append(group, donor), "hot", true)
		if p.Kind != PlaceExisting || p.Machine != want {
			t.Errorf("placement %+v, want %s", p, want)
		}
	}
	check("low-rate", lowItems, lowRate)
	room := lowItems
	room.Free, room.Cores = 3, 16
	check("low-items", room, lowRate)
	noRelief := candidate("heavy", 550, 0)
	check("low-rate", noRelief, lowRate)
	tooHot := candidate("no-headroom", 1, 0)
	tooHot.CPUPct = 50
	check("low-rate", tooHot, lowRate)
	missing := candidate("missing", 90, 0)
	for _, bad := range []*AppliedRate{nil, {AsOf: now.Add(-AppliedRateMaxAge - time.Second)}, {AsOf: now.Add(time.Minute)}, {AsOf: now, PerSecond: -1}} {
		missing.AppliedRate = bad
		check("low-items", missing, lowRate, lowItems)
		check("low-items", lowItems, lowRate, missing)
	}
}

func TestPreferExistingBelowThirtyWithoutBypassingCopyBudget(t *testing.T) {
	donor := itemized(machine("donor", 2, 0, 60, "hot", "other"), 100, 900)
	low := itemized(machine("low", 1, 0, 29, "a"), 30)
	low.Cores = 8
	mid := itemized(machine("mid", 1, 0, 32, "b"), 1)
	mid.Cores = 8
	spare := machine("spare", 0, 0, 0)
	if got := Place([]MachineFacts{donor, mid, spare, low}, "hot", true); got.Machine != "low" {
		t.Fatalf("preferred available existing host: %+v", got)
	}
	low.Cores = 4
	if got := Place([]MachineFacts{donor, low, spare}, "hot", true); got.Machine != "spare" {
		t.Fatalf("29%% plus copy would exceed admission: %+v", got)
	}
}

func TestMeasuredSecondReceiverHasTwoBudgetsAndOneWarmLane(t *testing.T) {
	cold := MachineFacts{ResourceManaged: true, HostCPUOK: true, CPUOK: true, MemOK: true, DiskOK: true, MemFreePct: 60, DiskFreePct: 60, Free: 1, Cores: 4, CPUPct: 25, Receiving: 1, CopySlots: 1, WarmReceiverLane: true, ReceiverReserveCores: 0.5}
	if ready, known := ReceiverOffer(cold); !ready || !known {
		t.Fatal("expected offer at exactly 50% projected CPU")
	}
	for label, change := range map[string]func(*MachineFacts){
		"third join":          func(m *MachineFacts) { m.Receiving++ },
		"unobserved":          func(m *MachineFacts) { m.UnobservedSlots++ },
		"no serialized lane":  func(m *MachineFacts) { m.WarmReceiverLane = false },
		"unbounded copies":    func(m *MachineFacts) { m.CopySlots = 0 },
		"projected CPU":       func(m *MachineFacts) { m.CPUPct = 25.1 },
		"30% host":            func(m *MachineFacts) { m.CPUPct = 30; m.Cores = 8 },
		"memory":              func(m *MachineFacts) { m.MemFreePct = 39 },
		"disk":                func(m *MachineFacts) { m.DiskFreePct = 39 },
		"unknown machine CPU": func(m *MachineFacts) { m.HostCPUOK = false },
	} {
		candidate := cold
		change(&candidate)
		if ready, _ := ReceiverOffer(candidate); ready {
			t.Errorf("%s advertised an offer", label)
		}
	}
}
