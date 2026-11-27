package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func healthy(at time.Time, received, acked int64) []nodeSample {
	return []nodeSample{
		{At: at, URL: "a", Up: true, Received: received, Acked: acked, Version: "v1", VNodes: 4, MembershipBytes: 4000},
		{At: at, URL: "b", Up: true, Received: received, Acked: acked, Version: "v1", VNodes: 4, MembershipBytes: 4000},
	}
}

func run(s *sentinelState, polls []func(int) []nodeSample) string {
	reason := ""
	for i, poll := range polls {
		if r := s.judge(poll(i)); r != "" {
			reason = r
		}
	}
	return reason
}

func rules() sentinelRules {
	return sentinelRules{stagnation: .5, stagnationFor: 0, minRate: 5, handoffOpen: 2 * time.Minute, handoffLoop: 3, versionsFor: 0, membershipPerVN: 2048, downAfter: 3}
}

// Each stop rule fires on its defect and stays quiet on a healthy run.
func TestSentinelRules(t *testing.T) {
	t0 := time.Now()
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * 2 * time.Second) }
	cases := map[string]func(i int) []nodeSample{
		"": func(i int) []nodeSample { return healthy(at(i), int64(i)*100, int64(i)*100) },
		"stagnation": func(i int) []nodeSample {
			return healthy(at(i), int64(i)*100, int64(i)*10)
		},
		"handoff_open": func(i int) []nodeSample {
			x := healthy(at(i), int64(i)*100, int64(i)*100)
			x[0].Open, x[0].Oldest = 1, 200
			return x
		},
		"handoff_loop": func(i int) []nodeSample {
			x := healthy(at(i), int64(i)*100, int64(i)*100)
			x[1].Open, x[1].Attempts = 1, 3
			return x
		},
		"membership_bloat": func(i int) []nodeSample {
			x := healthy(at(i), int64(i)*100, int64(i)*100)
			x[0].MembershipBytes = 2_600_000
			return x
		},
		"versions_split": func(i int) []nodeSample {
			x := healthy(at(i), int64(i)*100, int64(i)*100)
			x[1].Version = "v2"
			return x
		},
		"node_down": func(i int) []nodeSample {
			x := healthy(at(i), int64(i)*100, int64(i)*100)
			if i > 1 {
				x[1] = nodeSample{At: at(i), URL: "b"}
			}
			return x
		},
	}
	for want, poll := range cases {
		polls := make([]func(int) []nodeSample, 8)
		for i := range polls {
			polls[i] = poll
		}
		got := run(&sentinelState{rules: rules(), seen: map[string]int{}}, polls)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%q: sentinel said %q", want, got)
		}
	}
}

// Overload answered by refusals is saturation, not stagnation.
func TestSentinelSaturationIsNotStagnation(t *testing.T) {
	t0 := time.Now()
	polls := make([]func(int) []nodeSample, 8)
	for i := range polls {
		polls[i] = func(i int) []nodeSample {
			x := healthy(t0.Add(time.Duration(i)*2*time.Second), int64(i)*400, int64(i)*100)
			for j := range x {
				x[j].Refused = int64(i) * 300
			}
			return x
		}
	}
	if got := run(&sentinelState{rules: rules(), seen: map[string]int{}}, polls); got != "" {
		t.Fatalf("saturation taken for stagnation: %s", got)
	}
}

// An open handoff explains differing versions; idle clients are not stagnation.
func TestSentinelToleratesTopologyChangesAndIdleClients(t *testing.T) {
	t0 := time.Now()
	polls := make([]func(int) []nodeSample, 8)
	for i := range polls {
		polls[i] = func(i int) []nodeSample {
			x := healthy(t0.Add(time.Duration(i)*2*time.Second), 3, 0)
			x[1].Version, x[0].Open, x[0].Oldest = "v2", 1, 10
			return x
		}
	}
	if got := run(&sentinelState{rules: rules(), seen: map[string]int{}}, polls); got != "" {
		t.Fatalf("false stop: %s", got)
	}
}

func TestStepLevel(t *testing.T) {
	for _, c := range []struct {
		at   time.Duration
		want float64
	}{{0, 1}, {59 * time.Second, 1}, {75 * time.Second, 1.5}, {90 * time.Second, 2}, {120 * time.Second, 2}, {209 * time.Second, 4}, {240 * time.Second, 4}, {400 * time.Second, 16}} {
		if got := stepLevel(c.at, 2, 30*time.Second, 60*time.Second); got != c.want {
			t.Errorf("stepLevel(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

// AWS 2026-11-26: a vnode whose donor no longer needed it left with exit
// code 0 and stopped the run as node_down. A crash must still show.
func TestSentinelDoesNotWatchAVNodeThatLeftOnItsOwn(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"slot":0,"monitor":"http://m0","alive":true},
			{"slot":1,"monitor":"http://m1","alive":false,"exited":"2026-11-26T17:28:00Z","exit_code":0},
			{"slot":2,"monitor":"http://m2","alive":false,"exited":"2026-11-26T17:28:00Z","exit_code":2},
			{"slot":3,"monitor":"http://m3"}]`)
	}))
	defer agent.Close()
	got := (&sentinelState{}).agentMonitors(agent.URL)
	if want := []string{"http://m0", "http://m2", "http://m3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("watched %v, want %v: the crash stays, the clean leave goes", got, want)
	}
}
