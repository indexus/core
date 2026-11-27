package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSentinelKeepsCleanExitsOfActivatedGenerations(t *testing.T) {
	for name, raw := range map[string]map[string]any{
		"ready":        {"client_ready": true},
		"installed":    {"handoffs": map[string]any{"taken": float64(1)}},
		"write":        {"writes": map[string]any{"applied": float64(1)}},
		"write-demand": {"demand": map[string]any{"applied": float64(1)}},
	} {
		t.Run(name, func(t *testing.T) {
			var body atomic.Value
			body.Store(`[{"monitor":"http://m","started":"2026-11-26T17:00:00Z","alive":true}]`)
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body.Load().(string))
			}))
			defer agent.Close()
			watch := &sentinelState{rules: rules(), seen: map[string]int{}}
			if len(watch.agentMonitors(agent.URL)) != 1 {
				t.Fatal("live slot not watched")
			}
			watch.judge([]nodeSample{{At: time.Now(), URL: "http://m", Up: true, Raw: raw}})
			body.Store(`[{"monitor":"http://m","started":"2026-11-26T17:00:00Z","exited":"2026-11-26T17:01:00Z","exit_code":0}]`)
			if len(watch.agentMonitors(agent.URL)) != 1 {
				t.Fatal("exit 0 hid a vnode that was activated")
			}
			var reason string
			for range 3 {
				reason = watch.judge([]nodeSample{{At: time.Now(), URL: "http://m"}})
			}
			if !strings.Contains(reason, "node_down") {
				t.Fatalf("activated vnode disappeared without an alarm: %q", reason)
			}
			// The same endpoint can be reused by a new join which never activates.
			body.Store(`[{"monitor":"http://m","started":"2026-11-26T17:02:00Z","exited":"2026-11-26T17:03:00Z","exit_code":0}]`)
			if len(watch.agentMonitors(agent.URL)) != 0 {
				t.Fatal("an abandoned new process inherited the previous vnode's activation")
			}
			// Missing exit status is not evidence of a clean departure.
			body.Store(`[{"monitor":"http://m","started":"2026-11-26T17:02:00Z","exited":"2026-11-26T17:03:00Z"}]`)
			if len(watch.agentMonitors(agent.URL)) != 1 {
				t.Fatal("unknown exit status was hidden")
			}
		})
	}
}
