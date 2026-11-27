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
