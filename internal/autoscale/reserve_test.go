package autoscale

import "testing"

func TestReadyReserveCountsResourceOffersNotFreeSlots(t *testing.T) {
	loaded := MachineFacts{Machine: "loaded", Running: 5, Cores: 4, CPUOK: true, CPUPct: 80, MemOK: true, MemFreePct: 70, DiskOK: true, DiskFreePct: 70}
	spare := loaded
	spare.Machine, spare.Running, spare.Free, spare.CPUPct = "spare", 0, 5, 0
	// Eight busy hosts plus one spare leave 14.8 cores in excess of 30%: two offers.
	machines := make([]MachineFacts, 8)
	for i := range machines {
		machines[i] = loaded
	}
	machines = append(machines, spare)
	boots := func(selected string) int {
		reserve, _ := ReadyReserve(machines, selected, 15, 0, nil)
		return reserve.Boots()
	}
	if reserve, ok := ReadyReserve(machines, "", 15, 0, nil); !ok || reserve.Boots() != 1 {
		t.Fatalf("reserve=%+v known=%v", reserve, ok)
	}
	if got := boots("spare"); got != 2 {
		t.Fatalf("promised target counted as spare: %d", got)
	}
	spare.CPUPct = 20 // 20+43.75 > 60: five free slots offer no receiver.
	machines[8] = spare
	if got := boots(""); got != 2 {
		t.Fatalf("overloaded spare counted: %d", got)
	}
	machines[8].MemOK = false
	if _, ok := ReadyReserve(machines, "", 15, 0, nil); ok {
		t.Fatal("unknown RAM passed as ready")
	}
}

// Three donors wait for a machine: each needs one, on top of the floor. One
// boot in flight (the hook's to subtract) no longer covers the wave.
func TestReadyReserveAddsDistinctPendingRequestsToTheFloor(t *testing.T) {
	loaded := MachineFacts{Machine: "loaded", Running: 2, Free: 3, Cores: 4, CPUOK: true, CPUPct: 50, MemOK: true, MemFreePct: 70, DiskOK: true, DiskFreePct: 70}
	machines := []MachineFacts{loaded, loaded, loaded, loaded} // 3.2 excess cores: floor ceil(3.2*.15/1.75) = 1
	reserve, ok := ReadyReserve(machines, "", 15, 3, nil)
	if want := (Reserve{Floor: 1, Pending: 3}); !ok || reserve != want || reserve.Boots() != 4 {
		t.Fatalf("reserve=%+v boots=%d known=%v", reserve, reserve.Boots(), ok)
	}
	if none, _ := ReadyReserve(machines, "", 15, 0, nil); none.Boots() != 1 {
		t.Fatalf("without a backlog the floor stands alone: %+v", none)
	}
}

// A machine counts once: ready, warming, or a destination (none).
func TestReadyReserveCountsAMachineOnce(t *testing.T) {
	empty := MachineFacts{Running: 0, Free: 5, Cores: 4, CPUOK: true, MemOK: true, MemFreePct: 70, DiskOK: true, DiskFreePct: 70}
	named := func(name string, cpu float64, receiving int) MachineFacts {
		m := empty
		m.Machine, m.CPUPct, m.Receiving = name, cpu, receiving
		return m
	}
	// ready: admissible. boot: just registered, start-up CPU 25+43.75 > 60.
	// old: the same CPU on a machine that is not fresh. dest: just chosen.
	machines := []MachineFacts{named("ready", 0, 0), named("boot", 25, 0), named("old", 25, 0), named("dest", 0, 1), named("chosen", 0, 0)}
	reserve, ok := ReadyReserve(machines, "chosen", 0, 4, map[string]bool{"boot": true, "dest": true, "ready": true})
	if want := (Reserve{Pending: 4, Ready: 1, Warming: 1}); !ok || reserve != want || reserve.Boots() != 2 {
		t.Fatalf("reserve=%+v boots=%d", reserve, reserve.Boots())
	}
	short := named("boot", 25, 0)
	short.DiskFreePct = 10 // will not become a target by waiting
	if reserve, _ := ReadyReserve([]MachineFacts{short}, "", 0, 1, map[string]bool{"boot": true}); reserve.Warming != 0 {
		t.Fatalf("a machine short of disk counted as warming: %+v", reserve)
	}
}

func TestQuietFleetUsesExistingCapacityBeforeBootingSpeculativeSpares(t *testing.T) {
	busy := MachineFacts{Machine: "busy", Running: 3, Free: 2, Cores: 4, CPUOK: true, CPUPct: 60, MemOK: true, MemFreePct: 75, DiskOK: true, DiskFreePct: 75}
	light := busy
	light.Machine, light.Cores, light.CPUPct = "light", 12, 20
	fleet := []MachineFacts{busy, light} // (4*60+12*20)/16 = 30%
	r, ok := ReadyReserve(fleet, "", 15, 0, nil)
	if !ok || r.Floor != 0 || r.Ready != 1 || r.Boots() != 0 {
		t.Fatalf("low fleet utilization created a spare: %+v", r)
	}
	r, _ = ReadyReserve(fleet, "light", 15, 2, nil)
	if r.Ready != 0 || r.Boots() != 2 {
		t.Fatalf("genuine waiting donors lost their supply: %+v", r)
	}
	light.Cores, light.CPUPct = 4, 19
	r, _ = ReadyReserve([]MachineFacts{light}, "", 15, 1, nil)
	if r.Ready != 0 || r.Boots() != 1 {
		t.Fatalf("under 30%% but without room for a copy: %+v", r)
	}
}

func TestBootPacingRequiresMeasuredFleetHeadroom(t *testing.T) {
	busy := MachineFacts{CPUOK: true, HostCPUOK: true, Cores: 4, CPUPct: 60, MemOK: true, MemFreePct: 70, DiskOK: true, DiskFreePct: 70}
	spare := busy
	spare.Cores, spare.CPUPct = 12, 20
	check := func(want bool) {
		t.Helper()
		r, ok := ReadyReserve([]MachineFacts{busy, spare}, "", 0, 10, nil)
		if !ok || r.ReuseHeadroom != want {
			t.Fatalf("reserve=%+v known=%v want headroom=%v", r, ok, want)
		}
	}
	check(true)
	spare.CPUPct++
	check(false)
	spare.CPUPct--
	spare.DiskFreePct = 20
	check(false)
	spare.DiskFreePct = 70
	spare.HostCPUOK = false
	check(false)
	// One full disk should not accelerate the entire fleet when many
	// other measured cores have room.
	busy.DiskFreePct = 10
	spare.HostCPUOK = true
	spare.CPUPct = 10
	check(true)
}
