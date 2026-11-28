package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

func TestOpposingPlacementsUseDistinctReceivers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondRate float64
		thirdHost  bool
		expired    bool
		want       string
		blocked    bool
	}{
		{"comparable rates", 250, true, false, "C", true},
		{"comparable rates at boundary", 750, true, false, "C", true},
		{"much hotter second donor", 751, true, false, "A", false},
		{"much colder second donor", 80, true, false, "A", false},
		{"unknown rate", 0, true, false, "A", false},
		{"no alternative waits", 250, false, false, "", true},
		{"reservation has elapsed", 250, false, true, "A", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("test")
			a, b := host(1, 0, 10, "a"), host(1, 0, 10, "b")
			a.Machine, b.Machine = "A", "B"
			ms := []autoscale.MachineFacts{a, b}
			first, _ := s.planPlacement(ms, "a", "target-a", 250, false)
			if first.Machine != "B" {
				t.Fatal(first)
			}
			if tc.expired {
				s.placed["B"] = time.Now().Add(-placementMemory)
			}
			if tc.thirdHost {
				c := host(1, 0, 15, "c")
				c.Machine = "C"
				ms = append(ms, c)
			}
			next, _ := s.planPlacement(ms, "b", "target-b", tc.secondRate, false)
			if next.Machine != tc.want || (next.ReverseOf != "") != tc.blocked {
				t.Fatalf("second placement %+v", next)
			}
			if tc.blocked && tc.want == "" && (next.Kind != autoscale.PlaceNew || next.capMachine != "A") {
				t.Fatalf("machine cap fallback lost eligible remote A: %+v", next)
			}
		})
	}
}

func TestReciprocalCallsInOneSurveyRetainCapFallback(t *testing.T) {
	s := NewServer("test")
	a, b := host(1, 0, 10, "a"), host(1, 0, 10, "b")
	a.Machine, b.Machine = "A", "B"
	for _, m := range []*autoscale.MachineFacts{&a, &b} {
		m.DiskOK, m.MemOK, m.HostCPUOK = true, true, true
		m.MemFreePct, m.DiskFreePct = 95, 95
	}
	call := func(source string) *placementCall {
		return &placementCall{ctx: context.Background(), requester: source, target: "target-" + source, rate: 250, reply: make(chan placementAnswer, 1)}
	}
	first, second := call("a"), call("b")
	s.placeCalls([]autoscale.MachineFacts{a, b}, []*placementCall{first, second})
	left, right := <-first.reply, <-second.reply
	if left.record.Machine != "B" || right.record.Machine != "" || right.record.ReverseOf != "A" || envOf(right.env, "INDEXUS_PLACE_CAP_AGENT") != "A" {
		t.Fatalf("opposing decisions on one survey: %+v, %+v", left.record, right.record)
	}
	if r := right.record.Reserve; r == nil || r.Ready != 0 || r.Boots() == 0 {
		t.Fatalf("ineligible reverse route counted as spare capacity: %+v", r)
	}
}

func TestReciprocalOnlyForgetConfirmedNoDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantBlocked  bool
	}{
		{"confirmed", "echo INDEXUS_NO_RECEIVER; exit 75", false},
		{"ambiguous", "exit 1", true},
		{"success", "echo 'placed kind=existing agent=B'; echo local-id", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("test")
			a, b := host(1, 0, 10, "a"), host(1, 0, 10, "b")
			a.Machine, b.Machine = "A", "B"
			ms := []autoscale.MachineFacts{a, b}
			record, env := s.planPlacement(ms, "a", "first", 250, false)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "spawn.sh"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			_, _ = s.applySpawn(dir, "first", placementAnswer{record: record, env: env})
			next, _ := s.planPlacement(ms, "b", "second", 250, false)
			if (next.ReverseOf != "") != tc.wantBlocked {
				t.Fatalf("record: %+v", next)
			}
		})
	}
}
