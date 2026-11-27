package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// The agent relays the CPU profile of one slot from its loopback profiling
// port, so a receiver can be profiled from another machine. It refuses when
// profiling is off, an unknown slot and an unbounded duration.
func TestAgentRelaysTheProfileOfOneSlot(t *testing.T) {
	var asked string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.String()
		fmt.Fprint(w, "profile-bytes")
	}))
	defer node.Close()
	address, _ := url.Parse(node.URL)
	port, _ := strconv.Atoi(address.Port())
	origin := "http://" + address.Hostname()

	get := func(debugBase int, query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		relayProfile(w, httptest.NewRequest(http.MethodGet, "/profile?"+query, nil), debugBase, 2, origin)
		return w
	}
	// Slot 1 listens on debug-base+1.
	if w := get(port-1, "slot=1&seconds=3"); w.Code != http.StatusOK || w.Body.String() != "profile-bytes" {
		t.Fatalf("relay: %d %q", w.Code, w.Body.String())
	}
	if asked != "/debug/pprof/profile?seconds=3" {
		t.Fatalf("node was asked %q", asked)
	}
	for query, want := range map[string]int{
		"slot=2&seconds=3":   http.StatusBadRequest, // no such slot
		"slot=x":             http.StatusBadRequest,
		"slot=1&seconds=0":   http.StatusBadRequest,
		"slot=1&seconds=600": http.StatusBadRequest,
	} {
		if w := get(port-1, query); w.Code != want {
			t.Fatalf("%s: status %d, want %d", query, w.Code, want)
		}
	}
	if w := get(0, "slot=0"); w.Code != http.StatusNotFound {
		t.Fatalf("profiling off: status %d, want 404", w.Code)
	}
}
