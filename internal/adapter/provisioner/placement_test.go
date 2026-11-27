package provisioner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

func factsLine(agent string, facts autoscale.MachineFacts) string {
	raw, _ := json.Marshal(map[string]any{"agent": agent, "facts": facts})
	return string(raw)
}

func host(running, receiving int, cpu float64, vnodes ...string) autoscale.MachineFacts {
	return autoscale.MachineFacts{Running: running, Free: 2 - running, Receiving: receiving, VNodes: vnodes, Cores: 4, CPUPct: cpu, CPUOK: true}
}

func envOf(env []string, key string) string {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, key+"="); ok {
			return value
		}
	}
	return ""
}

func TestParseMachineFactsSkipsWhatIsNotFacts(t *testing.T) {
	out := factsLine("http://a", host(1, 0, 10, "v")) + "\nlocal-33\n{\"facts\":{}}\n\n" + factsLine("http://b", host(0, 0, 0)) + "\n"
	got := parseMachineFacts([]byte(out))
	if len(got) != 2 || got[0].Machine != "http://a" || got[1].Machine != "http://b" || got[0].VNodes[0] != "v" || !got[0].CPUOK {
		t.Fatalf("parsed %+v", got)
	}
}

func TestDecidePlacementHandsTheDecisionToTheHook(t *testing.T) {
	s := NewServer("key")
	machines := []autoscale.MachineFacts{host(1, 0, 60, "hot"), host(0, 0, 0)}
	machines[0].Machine, machines[1].Machine = "http://a", "http://b"
	record, env := s.decidePlacement(machines, "hot", "near")
	if record.Kind != autoscale.PlaceNew || record.Machine != "http://b" {
		t.Fatalf("decision %+v", record)
	}
	if envOf(env, "INDEXUS_PLACE_KIND") != "new" || envOf(env, "INDEXUS_PLACE_AGENT") != "http://b" || envOf(env, "INDEXUS_PLACE_MACHINES") != "2" || envOf(env, "INDEXUS_PLACE_REASON") == "" {
		t.Fatalf("environment %v", env)
	}
}

func TestReserveIdentifiesItsSampledFleet(t *testing.T) {
	machines := []autoscale.MachineFacts{host(1, 0, 60, "v"), host(0, 0, 0)}
	for i, name := range []string{"http://a", "http://b"} {
		m := &machines[i]
		m.Machine = name
		m.MemOK = true
		m.MemFreePct = 80
		m.DiskOK = true
		m.DiskFreePct = 80
	}
	_, env := NewServer("key").decidePlacement(machines, "v", "near")
	if got := envOf(env, "INDEXUS_RESERVE_AGENTS"); got != `["http://a","http://b"]` {
		t.Fatalf("agents in reserve: %s", got)
	}
}

func TestPartialFactsIdentifySurveyWithoutAuthorizingSpareBoots(t *testing.T) {
	donor, partial := host(1, 0, 90, "hot"), host(0, 0, 0)
	donor.Machine, partial.Machine = "http://a", "http://b"
	for _, h := range []*autoscale.MachineFacts{&donor, &partial} {
		h.AddressSlots, h.HostCPUOK, h.MemOK, h.DiskOK = 4, true, true, true
		h.MemFreePct, h.DiskFreePct = 80, 80
	}
	partial.MemOK = false
	record, env := NewServer("key").decidePlacement([]autoscale.MachineFacts{donor, partial}, "hot", "near")
	if record.Kind != autoscale.PlaceNew || record.Machine != "" || record.Reserve != nil {
		t.Fatalf("unexpected placement or reserve: %+v", record)
	}
	if got := envOf(env, "INDEXUS_RESERVE_AGENTS"); got != `["http://a","http://b"]` {
		t.Fatalf("incomplete survey identity list %q", got)
	}
	for _, key := range []string{"INDEXUS_READY_RESERVE_DEFICIT", "INDEXUS_BOOT_ALLOWANCE"} {
		if got := envOf(env, key); got != "" {
			t.Fatalf("authorized %s=%q with unknown reserve", key, got)
		}
	}
}

// Two scale-ups decided before the agent shows the first vnode must not
// choose the same machine: the second one waits; the cap cannot bypass resource admission.
func TestDecidePlacementRemembersAMachineJustChosen(t *testing.T) {
	s := NewServer("key")
	facts := func() []autoscale.MachineFacts {
		machines := []autoscale.MachineFacts{host(1, 0, 60, "hot"), host(0, 0, 0)}
		machines[0].Machine, machines[1].Machine = "http://a", "http://b"
		return machines
	}
	if first, _ := s.decidePlacement(facts(), "hot", "n1"); first.Machine != "http://b" {
		t.Fatalf("first %+v", first)
	}
	second, env := s.decidePlacement(facts(), "hot", "n2")
	if second.Kind != autoscale.PlaceNew || second.Machine != "" {
		t.Fatalf("second decision took a machine already chosen: %+v", second)
	}
	if envOf(env, "INDEXUS_PLACE_AGENT") != "" || envOf(env, "INDEXUS_PLACE_CAP_AGENT") != "" || !strings.Contains(envOf(env, "INDEXUS_PLACE_CAP_REASON"), "machine cap reached") {
		t.Fatalf("environment %v", env)
	}
	s.placed["http://b"] = time.Now().Add(-2 * placementMemory)
	if third, _ := s.decidePlacement(facts(), "hot", "n3"); third.Machine != "http://b" {
		t.Fatalf("the memory of a placement did not expire: %+v", third)
	}
}

func TestDecidePlacementWithoutFactsLeavesTheHookItsRule(t *testing.T) {
	s := NewServer("key")
	if record, env := s.decidePlacement(nil, "hot", "near"); env != nil || record.Kind != autoscale.PlaceSlots {
		t.Fatalf("no facts: %+v %v", record, env)
	}
	old := []autoscale.MachineFacts{{Machine: "http://old", Running: 1, Free: 1}}
	if record, env := s.decidePlacement(old, "hot", "near"); env != nil || record.Kind != autoscale.PlaceSlots || record.Machine != "http://old" {
		t.Fatalf("facts without resources: %+v %v", record, env)
	}
}

// The whole path with stub hooks: facts.sh reports, the decision reaches
// spawn.sh in its environment, and the reason shows in GET /v1/placement.
func TestScalePlacesFromFactsAndExposesTheReason(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")
	facts := factsLine("http://a", host(1, 0, 60, "hot")) + "\n" + factsLine("http://b", host(0, 0, 0)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "facts.txt"), []byte(facts), 0o644); err != nil {
		t.Fatal(err)
	}
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script("facts.sh", "#!/bin/sh\ncat \"$(dirname \"$0\")/facts.txt\"\n")
	script("spawn.sh", "#!/bin/sh\necho \"$INDEXUS_PLACE_KIND $INDEXUS_PLACE_AGENT $INDEXUS_PLACE_MACHINES\" >\"$(dirname \"$0\")/seen\"\necho \"placed kind=$INDEXUS_PLACE_KIND agent=$INDEXUS_PLACE_AGENT\" >&2\necho local-0\n")

	s := NewServer("key")
	s.LaunchTemplateID, s.LocalDir, s.SpawnMaxExtra = "local", dir, 0
	body, _ := json.Marshal(map[string]any{"requester_id": "hot", "prefer_near": "near", "spawn_count": 1})
	rec := httptest.NewRecorder()
	s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("scale: %d %s", rec.Code, rec.Body.String())
	}
	seen, _ := os.ReadFile(filepath.Join(dir, "seen"))
	if strings.TrimSpace(string(seen)) != "new http://b 2" {
		t.Fatalf("spawn hook saw %q", seen)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/placement", nil))
	var got struct {
		Decided, Applied map[string]int
		Last             []placementRecord
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Decided["new"] != 1 || got.Applied["new"] != 1 || len(got.Last) != 1 {
		t.Fatalf("placement metrics: %s", rec.Body.String())
	}
	if last := got.Last[0]; last.Machine != "http://b" || last.Requester != "hot" || !strings.Contains(last.Reason, "empty machine") || last.Applied != "new" {
		t.Fatalf("last decision: %+v", last)
	}
}

// Without a facts hook the spawn hook gets no decision: the behaviour before
// resources were reported.
func TestScaleWithoutFactsHookPassesNoDecision(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")
	if err := os.WriteFile(filepath.Join(dir, "spawn.sh"), []byte("#!/bin/sh\necho \"kind=${INDEXUS_PLACE_KIND-unset}\" >\"$(dirname \"$0\")/seen\"\necho local-0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewServer("key")
	s.LaunchTemplateID, s.LocalDir, s.SpawnMaxExtra = "local", dir, 0
	body, _ := json.Marshal(map[string]any{"requester_id": "hot", "spawn_count": 1})
	rec := httptest.NewRecorder()
	s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("scale: %d %s", rec.Code, rec.Body.String())
	}
	if seen, _ := os.ReadFile(filepath.Join(dir, "seen")); strings.TrimSpace(string(seen)) != "kind=unset" {
		t.Fatalf("spawn hook saw %q", seen)
	}
	if s.placementDecided[autoscale.PlaceSlots] != 1 {
		t.Fatalf("decided: %v", s.placementDecided)
	}
}

// Several distinct donors wait for a machine while one boots: each asks the
// hook for its own future machine on top of the floor, and asking again does
// not count a donor twice. The hook subtracts the boots already accepted.
func TestWaitingDonorsAreNotReducedToOneFutureMachine(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	full := func(running int, cpu float64) autoscale.MachineFacts {
		return autoscale.MachineFacts{Running: running, Free: 5 - running, Cores: 4, CPUPct: cpu, CPUOK: true, MemOK: true, MemFreePct: 70, DiskOK: true, DiskFreePct: 70}
	}
	facts := func(spare ...autoscale.MachineFacts) {
		lines := ""
		for i := 0; i < 4; i++ { // 4 machines at 50 % of 4 cores: floor ceil(8*.15/1.75) = 1
			lines += factsLine("http://busy"+string(rune('0'+i)), full(2, 50)) + "\n"
		}
		for _, m := range spare {
			lines += factsLine("http://spare", m) + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "facts.txt"), []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script("facts.sh", "#!/bin/sh\ncat \"$(dirname \"$0\")/facts.txt\"\n")
	script("spawn.sh", "#!/bin/sh\necho \"$INDEXUS_READY_RESERVE_DEFICIT $INDEXUS_RESERVE_DETAIL\" >>\"$(dirname \"$0\")/seen\"\n[ -n \"$INDEXUS_PLACE_AGENT\" ] || { echo 'waiting for a new machine' >&2; exit 1; }\necho \"placed kind=$INDEXUS_PLACE_KIND agent=$INDEXUS_PLACE_AGENT\" >&2\necho local-0\n")
	s := NewServer("key")
	s.LaunchTemplateID, s.LocalDir, s.SpawnMaxExtra = "local", dir, 0
	s.ScaleCooldown, s.GlobalAPICooldown = time.Nanosecond, time.Nanosecond
	ask := func(requester string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"requester_id": requester, "prefer_near": "near-" + requester, "reason": "writes", "spawn_count": 1})
		rec := httptest.NewRecorder()
		s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
		seen, _ := os.ReadFile(filepath.Join(dir, "seen"))
		lines := strings.Split(strings.TrimSpace(string(seen)), "\n")
		time.Sleep(time.Millisecond)
		return rec.Code, lines[len(lines)-1]
	}
	want := func(requester string, code int, handed string) {
		t.Helper()
		if got, line := ask(requester); got != code || line != handed {
			t.Fatalf("%s: status %d, hook saw %q; want %d, %q", requester, got, line, code, handed)
		}
	}
	facts()
	want("d1", 500, "2 floor=1,pending=1,ready=0,warming=0")
	want("d2", 500, "3 floor=1,pending=2,ready=0,warming=0")
	want("d3", 500, "4 floor=1,pending=3,ready=0,warming=0")
	want("d1", 500, "4 floor=1,pending=3,ready=0,warming=0") // asked again: still one need

	// A machine registers, hot from its own start: supply to come, once.
	facts(full(0, 25))
	want("d2", 500, "3 floor=1,pending=3,ready=0,warming=1")
	// Ready: d1 takes it, as a destination it is no reserve; two still wait.
	facts(full(0, 0))
	want("d1", 200, "3 floor=1,pending=2,ready=0,warming=0")
	if last := s.placements[len(s.placements)-1]; last.Reserve == nil || last.Reserve.Boots() != 3 || last.Machine != "http://spare" {
		t.Fatalf("last decision: %+v", last)
	}
	// A donor that stopped asking is no backlog.
	for k := range s.waiting {
		s.waiting[k] = time.Now().Add(-2 * waitingMemory)
	}
	delete(s.placed, "http://spare")
	facts()
	want("d3", 500, "2 floor=1,pending=1,ready=0,warming=0")
}

func TestHostReliefCannotFallBackToItsOwnMachineAtCap(t *testing.T) {
	s := NewServer("key")
	m := host(1, 0, 5, "hot")
	m.Machine = "donor"
	s.relief = map[string]*hostRelief{"donor": {owner: "hot", asking: true}}
	_, env := s.decidePlacement([]autoscale.MachineFacts{m}, "hot", "near")
	if envOf(env, "INDEXUS_PLACE_CAP_AGENT") != "" || !strings.Contains(envOf(env, "INDEXUS_PLACE_CAP_REASON"), "remote receiver") {
		t.Fatalf("host relief fallback: %v", env)
	}
}

func TestPendingSourceLanesKeepTheBacklog(t *testing.T) {
	s := new(Server)
	machines := []autoscale.MachineFacts{
		{Machine: "one", CopySlots: 1, VNodes: []string{"a", "b"}},
		{Machine: "two", CopySlots: 2, VNodes: []string{"c", "d", "e"}},
		{Machine: "unbudgeted", VNodes: []string{"f", "g"}},
	}
	now := time.Unix(123, 0)
	wait := placementRecord{At: now, Placement: autoscale.Placement{Kind: autoscale.PlaceNew}}
	check := func(req string, r placementRecord, offers, backlog int) {
		t.Helper()
		if got := s.pendingLocked(machines, req, "", r); got != offers || len(s.waiting) != backlog {
			t.Fatalf("%s: source lanes %d, backlog %d; want %d/%d", req, got, len(s.waiting), offers, backlog)
		}
	}
	check("a", wait, 1, 1)
	check("b", wait, 1, 2)
	check("a", wait, 1, 2)
	check("c", wait, 2, 3)
	check("d", wait, 3, 4)
	check("e", wait, 3, 5)
	check("f", wait, 4, 6)
	check("g", wait, 5, 7)
	check("unknown", wait, 6, 8)
	// Losing facts for a source may cost more offers; its requests remain.
	keep := machines
	machines = nil
	check("a", wait, 8, 8)
	machines = keep
	// A successful placement removes this requester, leaving its sibling
	// eligible for a later wave as soon as a destination frees up.
	check("a", placementRecord{At: now, Placement: autoscale.Placement{Kind: autoscale.PlaceExisting, Machine: "new"}}, 6, 7)
	wait.At = now.Add(waitingMemory / 2)
	check("b", wait, 6, 7)
	wait.At = now.Add(waitingMemory)
	check("b", wait, 1, 1)
}

func TestReserveCanBootWhenOnlyDonorHasAReadySlot(t *testing.T) {
	s := new(Server)
	h := autoscale.MachineFacts{Machine: "h", Running: 2, Free: 2, VNodes: []string{"a", "b"}, CopySlots: 1, Cores: 4, CPUOK: true, CPUPct: 5, MemOK: true, MemFreePct: 80, DiskOK: true, DiskFreePct: 80}
	for i, req := range []string{"a", "b"} {
		r, env := s.decidePlacement([]autoscale.MachineFacts{h}, req, "")
		if r.Reserve == nil || r.Backlog != i+1 || r.Reserve.Pending != 1 || r.Reserve.Ready != 1 || r.Reserve.Boots() != 1 || envOf(env, "INDEXUS_READY_RESERVE_DEFICIT") != "1" {
			t.Fatalf("request %s: %+v %+v", req, r, r.Reserve)
		}
		if i == 1 && !strings.Contains(envOf(env, "INDEXUS_RESERVE_DETAIL"), "backlog_vnodes=2,source_lanes=1") {
			t.Fatal("coalescing not visible in hook log")
		}
		// The simulated hook elected to boot and confirmed no fallback dispatch.
		s.releaseFallback(r)
	}
	warm := h
	warm.Machine = "starting"
	warm.VNodes = nil
	warm.Running = 0
	warm.CPUPct = 30
	r, _ := s.decidePlacement([]autoscale.MachineFacts{h, warm}, "a", "")
	if r.Reserve == nil || r.Reserve.Warming != 1 || r.Reserve.Boots() != 0 {
		t.Fatalf("already warming a receiver: %+v", r.Reserve)
	}
}

func TestFleetSurveySharesOnlyOverlappingReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var survey fleetSurvey
		var reads atomic.Int32
		release := make(chan struct{})
		probe := func() []autoscale.MachineFacts {
			reads.Add(1)
			<-release
			return []autoscale.MachineFacts{{Machine: "first"}}
		}
		const n = 8
		out := make(chan []autoscale.MachineFacts, n)
		for range n {
			go func() { out <- survey.read(probe) }()
		}
		synctest.Wait() // leader in probe, all other callers waiting for its result
		if reads.Load() != 1 {
			t.Fatalf("%d simultaneous fleet probes", reads.Load())
		}
		close(release)
		for range n {
			if got := <-out; len(got) != 1 || got[0].Machine != "first" {
				t.Fatalf("shared survey: %+v", got)
			}
		}
		if got := survey.read(func() []autoscale.MachineFacts { return nil }); got != nil {
			t.Fatal("failed probe reused old facts")
		}
		got := survey.read(func() []autoscale.MachineFacts { return []autoscale.MachineFacts{{Machine: "fresh"}} })
		if len(got) != 1 || got[0].Machine != "fresh" {
			t.Fatal("next survey retained a failed read")
		}
	})
}

func TestConcurrentSurveyResultsDoNotSharePlacementReservations(t *testing.T) {
	s := NewServer("key")
	facts := []autoscale.MachineFacts{host(1, 0, 60, "hot"), host(0, 0, 0), host(0, 0, 0)}
	for i, name := range []string{"d", "r1", "r2"} {
		facts[i].Machine = name
	}
	targets := make(chan string, 2)
	for _, requester := range []string{"hot", "other"} {
		go func() { r, _ := s.decidePlacement(facts, requester, "near"); targets <- r.Machine }()
	}
	a, b := <-targets, <-targets
	if a == b || !(a == "r1" && b == "r2" || a == "r2" && b == "r1") {
		t.Fatalf("reserved %q and %q", a, b)
	}
	for _, m := range facts {
		if m.Receiving != 0 {
			t.Fatalf("shared facts changed: %+v", m)
		}
	}
}
