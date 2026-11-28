package autoscale

import (
	"testing"
	"time"
)

// A level sustained for PressureHold must not be forgotten when brief rises
// keep restarting a different timer (observed in the Linux process lab).
func TestPressureLevelSurvivesIntermittentRise(t *testing.T) {
	clock := clockAt(time.Unix(1800000000, 0))
	a := hostController(clock, ResourceSample{})
	for second := 0; second <= 30; second++ {
		signal, held, need := a.samplePressure(Input{Resources: sharedHostSample(1, 70+10*float64(second%2))}, 0, clock.now())
		if second == 30 && (signal != "cpu" || held < need) {
			t.Fatalf("CPU always >=70%% for 30s, signal=%s held=%s need=%s", signal, held, need)
		}
		clock.advance(time.Second)
	}
}

func TestMaturePressureLevelWinsOverFreshRise(t *testing.T) {
	clock := clockAt(time.Unix(1800000000, 0))
	a := hostController(clock, ResourceSample{})
	a.samplePressure(Input{Resources: sharedHostSample(1, 80)}, 0, clock.now())
	clock.advance(30 * time.Second)
	a.samplePressure(Input{Resources: sharedHostSample(1, 80)}, 0, clock.now())
	clock.advance(time.Second)
	signal, held, need := a.samplePressure(Input{Resources: sharedHostSample(1, 90)}, 0, clock.now())
	if signal != "cpu" || held < need {
		t.Fatalf("fresh rise hid a mature level: %s held=%s need=%s", signal, held, need)
	}
}

func TestDifferentPressureResourcesDoNotShareAge(t *testing.T) {
	clock := clockAt(time.Unix(1800000000, 0))
	a := New(Config{Now: clock.now, CPURisePct: 10000, MemRisePct: 10000})
	a.samplePressure(Input{Resources: sharedHostSample(1, 80)}, 0, clock.now())
	clock.advance(20 * time.Second)
	sample := sharedHostSample(1, 10)
	sample.MemPct = 45
	signal, held, _ := a.samplePressure(Input{Resources: sample}, 0, clock.now())
	if signal != "mem_filling" || held != 0 {
		t.Fatalf("memory inherited CPU age: %s %s", signal, held)
	}
}

func TestPressureLevelRestartsAfterCooling(t *testing.T) {
	clock := clockAt(time.Unix(1800000000, 0))
	a := New(Config{Now: clock.now, CPURisePct: 10000, MemRisePct: 10000})
	for _, step := range []struct {
		after time.Duration
		cpu   float64
	}{{0, 80}, {20 * time.Second, 10}, {time.Second, 80}} {
		clock.advance(step.after)
		a.samplePressure(Input{Resources: sharedHostSample(1, step.cpu)}, 0, clock.now())
	}
	clock.advance(4 * time.Second)
	_, held, _ := a.samplePressure(Input{Resources: sharedHostSample(1, 80)}, 0, clock.now())
	if held != 4*time.Second {
		t.Fatalf("cooled interval retained: %s", held)
	}
}

func TestIntermittentHostRisesRequestCapacityAtTheLevelDeadline(t *testing.T) {
	clock := clockAt(time.Unix(1800000000, 0))
	a := hostController(clock, ResourceSample{})
	sent := make(chan Request, 1)
	for second := 0; second <= 30; second++ {
		a.Tick(Input{SelfName: testSelf, Resources: sharedHostSample(1, 70+10*float64(second%2))}, func(r Request) error { sent <- r; return nil })
		if second < 30 {
			clock.advance(time.Second)
		}
	}
	select {
	case req := <-sent:
		if req.Reason != ReasonHostPressure || req.Signal != "cpu" {
			t.Fatalf("unexpected capacity request: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("continuous high CPU did not request capacity by PressureHold")
	}
}

func TestPressureResourceFluctuationsDoNotHideAnotherMatureResource(t *testing.T) {
	clock := clockAt(time.Unix(1800000000, 0))
	a := New(Config{Now: clock.now, CPURisePct: 10000, MemRisePct: 10000})
	for second := 0; second <= 30; second++ {
		sample := sharedHostSample(1, 80)
		sample.MemPct = 20 + 25*float64(second%2)
		signal, held, need := a.samplePressure(Input{Resources: sample}, 0, clock.now())
		if second == 30 && (signal != "cpu" || held < need) {
			t.Fatalf("memory pulses hid continuous CPU pressure: %s %s/%s", signal, held, need)
		}
		clock.advance(time.Second)
	}
}

func TestSharedCPUWaitsOnlyABoundedTimeForInboundInstalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
		wait   time.Duration
		want   string
		host   bool
	}{
		{"continued installs cannot starve read-only CPU relief", func(*Input) {}, 30 * time.Second, "cpu", true},
		{"stalled transfer stops delaying immediately", func(in *Input) { in.InboundLots = 0 }, time.Second, "cpu", true},
		{"arriving writes keep CPU relief", func(in *Input) { in.WriteRate = 1 }, time.Second, "cpu", true},
		{"own CPU keeps CPU relief", func(in *Input) { in.Resources = sharedHostSample(80, 90) }, time.Second, "cpu", false},
		{"queue", func(in *Input) { in.OldestWait = 3 * time.Second }, time.Second, "queue", false},
		{"disk", func(in *Input) { in.Resources.DiskFreePct = 1 }, time.Second, "disk", false},
		{"memory", func(in *Input) { in.Resources.MemPct = 99 }, time.Second, "mem", false},
		{"owned items", func(in *Input) { in.OwnedItems = 500 }, time.Second, "items", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := clockAt(time.Unix(1_800_000_000, 0))
			a := New(Config{Now: clock.now, Enabled: true, ItemsLimit: 500, PressureHold: 30 * time.Second, CPULimitPct: 60, MemRefusePct: 90, DiskMinFreePct: 20})
			in := Input{Resources: sharedHostSample(5, 90), InboundLots: 1}
			a.evaluatePressure(in)
			clock.advance(30 * time.Second)
			if sig, _ := a.evaluatePressure(in); sig != "" {
				t.Fatalf("incoming mounts triggered %s", sig)
			}
			if got := a.Snapshot()["pressure"].(map[string]any); got["cpu_waits_inbound"] != true || got["inbound_lots"] != int64(1) {
				t.Fatal(got)
			}
			tc.change(&in)
			clock.advance(tc.wait)
			if sig, host := a.evaluatePressure(in); sig != tc.want || host != tc.host {
				t.Fatalf("got (%q,%t), want (%q,%t)", sig, host, tc.want, tc.host)
			}
		})
	}
}

func TestInboundInstallsDeferCPURiseOnce(t *testing.T) {
	clock := clockAt(time.Unix(1_800_000_000, 0))
	a := New(Config{Now: clock.now, Enabled: true, CPULimitPct: 95, CPUFloorPct: 25, CPURisePct: 1, RiseHold: 2 * time.Second})
	for second := 0; second <= 5; second++ {
		in := Input{Resources: sharedHostSample(1, 35+float64(second)*2), InboundLots: 3}
		sig, host := a.evaluatePressure(in)
		if second < 5 && sig != "" || second == 5 && (sig != "cpu_rise" || !host) {
			t.Fatalf("at %ds: %q host=%t", second, sig, host)
		}
		clock.advance(time.Second)
	}
}
