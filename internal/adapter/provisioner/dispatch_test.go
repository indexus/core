package provisioner

import (
	"context"
	"math"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Replay the 179 addresses visible at t=284s in aws-direct-64k-r2-1127-154608.
// Arrival order and five simultaneous offers are controlled counterfactuals.
// Both policies get identical targets and install exactly five receivers.
func TestPlacementReplayUniform32k(t *testing.T) {
	raw, err := os.ReadFile("testdata/uniform-32k-vnodes.txt")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := func(name string) tree.VNodeDescriptor {
		id, err := tree.DecodeName(name)
		if err != nil {
			t.Fatal(err)
		}
		return tree.VNodeDescriptor{ID: id, Name: name}
	}
	var initial []tree.VNodeDescriptor
	for _, name := range strings.Fields(string(raw)) {
		initial = append(initial, descriptor(name))
	}
	targets := [][2]string{
		{"e4ydRwEl6owD5bwX", "e4SdRwEl6owD5bwX"}, {"-fgmaHRVqJqw7_ha", "-hgmaHRVqJqw7_ha"}, {"L5yS7cU7K9AyZ5ff", "L7yS7cU7K9AyZ5ff"}, {"Zo6NcIiqC1hBEk8f", "Zs6NcIiqC1hBEk8f"}, {"vcAIlMHletFjPGMe", "vYAIlMHletFjPGMe"},
		{"W9roD-RVoXbsGaM8", "X9roD-RVoXbsGaM8"}, {"Rx6pjM3TbMGxhdUz", "Qx6pjM3TbMGxhdUz"}, {"9DDwHUGGduXc_I6X", "8DDwHUGGduXc_I6X"}, {"Scyl6hzvg6fUyiZk", "Ssyl6hzvg6fUyiZk"}, {"ERzBdsIKG_o5XMWT", "FRzBdsIKG_o5XMWT"},
	}
	rate := func(view *tree.View, name string) float64 {
		cell, err := tree.Cell(view, name)
		if err != nil {
			t.Fatal(err)
		}
		depth := 0
		for _, mask := range cell.Mask {
			depth += bits.OnesCount8(mask)
		}
		return math.Ldexp(32000, -depth)
	}
	for _, priority := range []bool{false, true} {
		view, err := tree.NewViewFrom(1, initial)
		if err != nil {
			t.Fatal(err)
		}
		s := NewServer("test")
		var calls []*placementCall
		for _, pair := range targets {
			calls = append(calls, &placementCall{ctx: t.Context(), requester: pair[0], target: pair[1], rate: rate(view, pair[0]), recheck: true, reply: make(chan placementAnswer, 1)})
		}
		if waiting := s.placeCalls(nil, append([]*placementCall(nil), calls...)); len(waiting) != len(calls) {
			t.Fatal("launched without supply")
		}
		var ms []autoscale.MachineFacts
		for _, id := range []string{"A", "B", "C", "D", "E"} {
			ms = append(ms, autoscale.MachineFacts{Machine: id, Cores: 4, CPUOK: true, DiskOK: true, MemOK: true, DiskFreePct: 80, MemFreePct: 80, Free: 4})
		}
		if priority {
			s.placeCalls(ms, calls)
		} else {
			for _, c := range calls {
				r, _ := s.decidePlacement(ms, c.requester, c.target)
				if r.Machine != "" {
					c.reply <- placementAnswer{record: r}
				}
			}
		}
		joined := 0
		for _, c := range calls {
			select {
			case a := <-c.reply:
				if a.record.PreferNear != c.target {
					t.Fatal(a.record)
				}
				view, err = tree.Extend(view, descriptor(c.target))
				if err != nil {
					t.Fatal(err)
				}
				joined++
			default:
			}
		}
		peak, total := 0., 0.
		for _, v := range view.Active {
			load := rate(view, v.Name)
			peak = max(peak, load)
			total += load
		}
		t.Logf("priority=%v offers=5 installs=%d peak=%.0f writes/s total=%.0f", priority, joined, peak, total)
		want := 1000.
		if priority {
			want = 500
		}
		if joined != 5 || peak != want || total != 32000 {
			t.Fail()
		}
	}
}

// Exercise the actual HTTP endpoint and local hook, including the transition
// from an explicit pre-dispatch refusal to supply appearing without a retry.
func TestScaleWaitDispatchesOriginalAddress(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	unavailable := host(1, 0, 80, "hot")
	available := host(0, 0, 0)
	available.MemOK = true
	available.MemFreePct = 90
	available.DiskOK = true
	available.DiskFreePct = 90
	write("facts.sh", "#!/bin/sh\necho '"+factsLine("donor", unavailable)+"'\nif test -f \"$INDEXUS_LOCAL_DIR/available\"; then echo '"+factsLine("receiver", available)+"'; fi\n")
	write("spawn.sh", `#!/bin/sh
if test -z "$INDEXUS_PLACE_AGENT"; then
 touch "$INDEXUS_LOCAL_DIR/available"
 echo INDEXUS_NO_RECEIVER
 exit 75
fi
echo "$INDEXUS_PREFER_NEAR $INDEXUS_PLACE_AGENT" >>"$INDEXUS_LOCAL_DIR/launches"
echo local-receiver
`)
	s := NewServer("test")
	s.LocalDir = dir
	s.SpawnMaxExtra = 0
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/scale", strings.NewReader(`{"requester_id":"hot","prefer_near":"X9roD-RVoXbsGaM8","prefer_nears":["X9roD-RVoXbsGaM8"],"write_rate":1000}`)))
	data, err := os.ReadFile(filepath.Join(dir, "launches"))
	if w.Code != 200 || err != nil || string(data) != "X9roD-RVoXbsGaM8 receiver\n" {
		t.Fatalf("%d %s launches=%q err=%v", w.Code, w.Body, data, err)
	}
}

func TestCancelledWaitNeverPlaces(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	call := &placementCall{ctx: ctx, recheck: true}
	if left := new(Server).placeCalls(nil, []*placementCall{call}); len(left) != 0 {
		t.Fatal("retained expired demand")
	}
}

func TestCoalescedRetryOnlyForUndispatchedAddress(t *testing.T) {
	f := newPressureFixture(t)
	f.facts(0)
	if _, out := f.ask("v1", reasonHostPressure); out["action"] != "spawned" {
		t.Fatal(out)
	}
	for _, requester := range []string{"v1", "v2"} {
		code, out := f.ask(requester, reasonHostPressure)
		_, retry := out["retry_after_ms"]
		if !coalesced(code, out) || retry != (requester == "v2") {
			t.Fatal(requester, out)
		}
	}
}

func TestConcurrentFallbacksCannotPromiseTheSameMachine(t *testing.T) {
	s := NewServer("test")
	m := host(2, 0, 5, "a", "b")
	m.Free = 2
	m.Machine = "donor"
	first, _ := s.decidePlacement([]autoscale.MachineFacts{m}, "a", "target-a")
	second, _ := s.decidePlacement([]autoscale.MachineFacts{m}, "b", "target-b")
	if first.capMachine != "donor" || second.capMachine != "" {
		t.Fatalf("fallbacks %q, %q", first.capMachine, second.capMachine)
	}
	s.releaseUndispatched(first)
	third, _ := s.decidePlacement([]autoscale.MachineFacts{m}, "b", "target-b")
	s.releaseUndispatched(first)
	if third.capMachine != "donor" || s.placed["donor"] != third.At {
		t.Fatalf("lost fallback reservation: %+v", third)
	}
}

func TestHotSiblingCanTakeOverOnlyAfterConfirmedWait(t *testing.T) {
	s := NewServer("test")
	relief := &hostRelief{owner: "cool", rate: 125, at: time.Now()}
	s.relief = map[string]*hostRelief{"machine": relief}
	req := scaleReq{RequesterID: "hot", WriteRate: 1000}
	if !s.coalescedLocked(req, "machine", nil, time.Now()) {
		t.Fatal("preempted ambiguous dispatch")
	}
	relief.unplaced = true
	if s.coalescedLocked(req, "machine", nil, time.Now()) {
		t.Fatal("hot donor blocked behind confirmed wait")
	}
	relief.asking = true
	if !s.coalescedLocked(req, "machine", nil, time.Now()) {
		t.Fatal("preempted active launch")
	}
}
