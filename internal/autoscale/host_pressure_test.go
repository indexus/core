package autoscale

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testSelf = "selfNode000000000000000000000000"

// sharedHostSample is what a vnode using process% of a machine at host% reads.
func sharedHostSample(process, host float64) ResourceSample {
	s := ResourceSample{DiskFreePct: 80, DiskOK: true}
	s.setCPU(process, true, hostCPUSample{busyPct: host}, true)
	return s
}

// fakeClock is read by the scale-up goroutine while the test advances it.
type fakeClock struct{ nanos atomic.Int64 }

func clockAt(at time.Time) *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(at.UnixNano())
	return c
}

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// hostController runs with the default thresholds (CPU 60 %, hold 30 s); only
// the sampler and the clock are injected.
func hostController(clock *fakeClock, sample ResourceSample) *Controller {
	return New(Config{
		Enabled: true, DisableDiskSignal: true, Cooldown: time.Hour,
		Now:    clock.now,
		Sample: func(string) ResourceSample { return sample },
	})
}

// tickHeld ticks once, lets the pressure hold elapse, ticks again and returns
// the request sent, if any.
func tickHeld(t *testing.T, a *Controller, clock *fakeClock, in Input) (Request, bool) {
	t.Helper()
	sent := make(chan Request, 1)
	up := func(req Request) error { sent <- req; return nil }
	in.SelfName, in.Resources = testSelf, a.Sample()
	a.Tick(in, up)
	clock.advance(a.Config().PressureHold + time.Second)
	a.Tick(in, up)
	select {
	case req := <-sent:
		return req, true
	case <-time.After(200 * time.Millisecond):
		return Request{}, false
	}
}

// AWS 2026-11-26, t=497.6 s: a host at 97.78 % CPU, its vnodes at 53.94 % and
// 42.27 %, none over its own 60 %. Each vnode must see the machine, and say
// that the machine is what fires.
func TestVNodesSharingAHotHostAskWithHostPressure(t *testing.T) {
	for _, shares := range [][]float64{{50, 40}, {18, 18, 18, 18, 18}} {
		var mu sync.Mutex
		var bodies []map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}))
		for i, share := range shares {
			if share >= 60 {
				t.Fatalf("vnode %d is over its own limit: the test would prove nothing", i)
			}
			clock := clockAt(time.Unix(1_800_000_000, 0))
			a := hostController(clock, sharedHostSample(share, 90))
			req, fired := tickHeld(t, a, clock, Input{})
			if !fired {
				t.Fatalf("%d vnodes: vnode %d at %g%% of a host at 90%% is not hot", len(shares), i, share)
			}
			if req.Reason != ReasonHostPressure || req.Signal != "cpu" {
				t.Fatalf("%d vnodes: vnode %d reason=%q signal=%q, want host_pressure/cpu", len(shares), i, req.Reason, req.Signal)
			}
			if !a.Wanted() {
				t.Fatalf("vnode %d under host pressure does not want a peer", i)
			}
			pressure := a.Snapshot()["pressure"].(map[string]any)
			if pressure["cpu_process_pct"] != share || pressure["cpu_host_pct"] != 90.0 || pressure["cpu_pct"] != 90.0 || pressure["host_pressure"] != true {
				t.Fatalf("metrics lost a reading: %v", pressure)
			}
			if err := a.PostRequest(server.URL, testSelf, 0, 1, req); err != nil {
				t.Fatal(err)
			}
		}
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) != len(shares) {
			t.Fatalf("posted %d requests for %d vnodes", len(bodies), len(shares))
		}
		for _, body := range bodies {
			if body["reason"] != "host_pressure" || body["signal"] != "cpu" || body["transfers_in_flight"] != 0.0 || body["cpu_host_pct"] != 90.0 {
				t.Fatalf("scale request does not name the machine: %v", body)
			}
		}
	}
}

// A vnode over its own limit asks for itself: nothing to coalesce.
func TestVNodeOverItsOwnCPULimitKeepsItsOwnReason(t *testing.T) {
	clock := clockAt(time.Unix(1_800_000_000, 0))
	a := hostController(clock, sharedHostSample(70, 90))
	if req, fired := tickHeld(t, a, clock, Input{}); !fired || req.Reason != "cpu" {
		t.Fatalf("fired=%v reason=%q, want cpu", fired, req.Reason)
	}
}

// Its relief is on the way: a vnode copying a transfer out does not ask for
// one more receiver because the machine is hot, but still asks for itself.
func TestOutgoingTransferHoldsHostPressureNotOwnQueue(t *testing.T) {
	clock := clockAt(time.Unix(1_800_000_000, 0))
	a := hostController(clock, sharedHostSample(40, 90))
	if req, fired := tickHeld(t, a, clock, Input{TransfersOut: 1}); fired {
		t.Fatalf("asked for %q while copying a transfer out", req.Reason)
	}
	pressure := a.Snapshot()["pressure"].(map[string]any)
	if pressure["host_pressure_waits_for_transfer"] != true || pressure["transfers_out"] != 1 {
		t.Fatalf("the wait is not visible: %v", pressure)
	}
	if a.Wanted() {
		t.Fatal("a vnode that waits for its transfer is marked as wanting a peer")
	}

	queued := Input{TransfersOut: 1, OldestWait: a.Config().QueueWaitLimit}
	req, fired := tickHeld(t, a, clock, queued)
	if !fired || req.Reason != "queue" || req.TransfersInFlight != 1 {
		t.Fatalf("fired=%v reason=%q transfers=%d, want its own queue to ask", fired, req.Reason, req.TransfersInFlight)
	}
}

// Writes in flight at the write limit for the whole wait limit ask too.
func TestQueueLimitFollowsWriteLimit(t *testing.T) {
	clock := clockAt(time.Unix(1_800_000_000, 0))
	a := New(Config{Enabled: true, DisableDiskSignal: true, Cooldown: time.Hour, WritesLimit: 500, Now: clock.now,
		Sample: func(string) ResourceSample { return ResourceSample{DiskFreePct: 80, DiskOK: true} }})
	if got := a.Config().QueueLimit; got != 1000 {
		t.Fatalf("queue limit=%d, want 500/s during 2s", got)
	}
	if req, fired := tickHeld(t, a, clock, Input{Queue: 999}); fired {
		t.Fatalf("asked for %q under the queue limit", req.Reason)
	}
	if req, fired := tickHeld(t, a, clock, Input{Queue: 1000}); !fired || req.Reason != "queue" {
		t.Fatalf("fired=%v reason=%q at the queue limit", fired, req.Reason)
	}
}

const (
	procStatBefore = "cpu  1000 0 500 8000 300 50 50 100 0 0\ncpu0 500 0 250 4000 150 25 25 50 0 0\nintr 1\n"
	// user +400, system +200, idle +300, iowait +100, irq +10, softirq +10,
	// steal +80; guest +250 is already inside user.
	procStatAfter = "cpu  1400 0 700 8300 400 60 60 180 250 0\ncpu0 700 0 350 4150 200 30 30 90 125 0\nintr 2\n"
)

// Busy is all but idle, as the placer counts it: iowait and steal are busy,
// guest is not counted twice.
func TestHostCPUBusyFractionBetweenTwoSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	m := &hostCPUMeter{path: path}
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(procStatBefore)
	if _, ok := m.sample(); ok {
		t.Fatal("one read of /proc/stat gave a rate")
	}
	write(procStatAfter)
	got, ok := m.sample()
	if !ok {
		t.Fatal("no sample between two reads")
	}
	const total = 400 + 200 + 300 + 100 + 10 + 10 + 80
	if want := 100 * float64(total-300) / total; got.busyPct != want {
		t.Fatalf("busy=%g want %g", got.busyPct, want)
	}
	if want := 100 * 100.0 / total; got.ioWaitPct != want {
		t.Fatalf("iowait=%g want %g", got.ioWaitPct, want)
	}
	if _, ok := m.sample(); ok {
		t.Fatal("a read with no tick elapsed gave a rate")
	}
	write(procStatBefore)
	if _, ok := m.sample(); ok {
		t.Fatal("counters that went back gave a rate")
	}
	write("cpu  x 0 0 0\n")
	if _, ok := m.sample(); ok {
		t.Fatal("an unreadable line gave a rate")
	}
}

// Where /proc/stat is absent (macOS), the process reading stands for the host.
func TestHostCPUFallsBackToProcessWithoutProcStat(t *testing.T) {
	m := &hostCPUMeter{path: filepath.Join(t.TempDir(), "absent")}
	host, hostOK := m.sample()
	if hostOK {
		t.Fatal("a missing /proc/stat gave a host sample")
	}
	var s ResourceSample
	s.setCPU(70, true, host, hostOK)
	if s.CPUPct != 70 || !s.CPUOK || s.HostCPUOK || s.ProcessCPUPct != 70 {
		t.Fatalf("fallback sample: %+v", s)
	}
	clock := clockAt(time.Unix(1_800_000_000, 0))
	s.DiskFreePct, s.DiskOK = 80, true
	a := hostController(clock, s)
	if req, fired := tickHeld(t, a, clock, Input{}); !fired || req.Reason != "cpu" {
		t.Fatalf("fired=%v reason=%q, want the process signal", fired, req.Reason)
	}
}
