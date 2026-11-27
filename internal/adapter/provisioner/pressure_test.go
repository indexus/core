package provisioner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// pressureFixture is a provisioner over stub hooks: facts.sh prints
// facts.txt and spawn.sh adds a line to "spawned" for each vnode it starts.
type pressureFixture struct {
	t   *testing.T
	dir string
	s   *Server
}

func newPressureFixture(t *testing.T) *pressureFixture {
	t.Helper()
	f := &pressureFixture{t: t, dir: t.TempDir(), s: NewServer("key")}
	writeLocalStubs(t, f.dir, "0")
	f.script("facts.sh", "#!/bin/sh\ncat \"$(dirname \"$0\")/facts.txt\"\n")
	f.script("spawn.sh", "#!/bin/sh\necho \"$INDEXUS_PREFER_NEAR\" >>\"$(dirname \"$0\")/spawned\"\necho local-0\n")
	f.s.LaunchTemplateID, f.s.LocalDir, f.s.SpawnMaxExtra = "local", f.dir, 0
	f.s.ScaleCooldown, f.s.GlobalAPICooldown = time.Nanosecond, time.Nanosecond
	return f
}

func (f *pressureFixture) script(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

// facts reports a hot machine a hosting v1..v5 and an empty machine b whose
// agent shows receiving vnodes still joining.
func (f *pressureFixture) facts(receiving int) {
	f.t.Helper()
	hot := autoscale.MachineFacts{Running: 5, VNodes: []string{"v1", "v2", "v3", "v4", "v5"}, Cores: 4, CPUPct: 90, CPUOK: true}
	spare := autoscale.MachineFacts{Running: receiving, Free: 5 - receiving, Receiving: receiving, Cores: 4, CPUOK: true}
	lines := factsLine("http://a", hot) + "\n" + factsLine("http://b", spare) + "\n"
	if err := os.WriteFile(filepath.Join(f.dir, "facts.txt"), []byte(lines), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pressureFixture) ask(requester, reason string) (int, map[string]any) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"requester_id": requester, "prefer_near": "near-" + requester, "reason": reason, "spawn_count": 1})
	rec := httptest.NewRecorder()
	f.s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	time.Sleep(time.Millisecond) // past the nanosecond cooldowns
	return rec.Code, out
}

func (f *pressureFixture) spawned() int {
	raw, _ := os.ReadFile(filepath.Join(f.dir, "spawned"))
	return len(strings.Fields(string(raw)))
}

func coalesced(code int, out map[string]any) bool {
	return code == http.StatusOK && out["status"] == "already_starting" && out["coalesced"] == reasonHostPressure
}

// Five vnodes of one machine each report its pressure: one receiver starts,
// the four other requests are answered as handled, none as a failure.
func TestFiveVNodesUnderHostPressureStartOneReceiver(t *testing.T) {
	f := newPressureFixture(t)
	f.facts(0)
	if code, out := f.ask("v1", reasonHostPressure); code != http.StatusOK || out["action"] != "spawned" {
		t.Fatalf("first request: %d %v", code, out)
	}
	for _, v := range []string{"v2", "v3", "v4", "v5"} {
		if code, out := f.ask(v, reasonHostPressure); !coalesced(code, out) || out["machine"] != "http://a" {
			t.Fatalf("%s was not coalesced: %d %v", v, code, out)
		}
	}
	if f.spawned() != 1 || f.s.scaleCount != 1 || f.s.placementDecided["host_coalesced"] != 4 {
		t.Fatalf("spawned=%d scaleCount=%d decided=%v, want one physical decision", f.spawned(), f.s.scaleCount, f.s.placementDecided)
	}
	// Its own load is another need: such a request keeps today's behaviour.
	if code, out := f.ask("v2", "writes"); code != http.StatusOK || out["action"] != "spawned" {
		t.Fatalf("a request for the vnode's own load was coalesced: %d %v", code, out)
	}
	// So does a requester no agent reports: its machine is unknown.
	if code, out := f.ask("elsewhere", reasonHostPressure); code != http.StatusOK || out["action"] != "spawned" {
		t.Fatalf("unknown machine: %d %v", code, out)
	}
	if f.spawned() != 3 {
		t.Fatalf("spawned=%d want 3", f.spawned())
	}
}

// The relief lasts while its receiver joins and ends with the join.
func TestHostReliefEndsWhenItsReceiverHasJoined(t *testing.T) {
	f := newPressureFixture(t)
	f.facts(0)
	if code, out := f.ask("v1", reasonHostPressure); out["action"] != "spawned" {
		t.Fatalf("first request: %d %v", code, out)
	}
	relief := f.s.relief["http://a"]
	if relief == nil || relief.dest != "http://b" || !relief.placed {
		t.Fatalf("relief %+v", relief)
	}
	// The owner too waits for its receiver.
	if code, out := f.ask("v1", reasonHostPressure); !coalesced(code, out) {
		t.Fatalf("owner asked again while its receiver starts: %d %v", code, out)
	}
	f.facts(1) // the agent now shows the receiver, joining
	relief.at = time.Now().Add(-2 * placementMemory)
	if code, out := f.ask("v2", reasonHostPressure); !coalesced(code, out) {
		t.Fatalf("receiver still joining: %d %v", code, out)
	}
	f.facts(0) // joined
	if code, out := f.ask("v2", reasonHostPressure); out["action"] != "spawned" {
		t.Fatalf("after the join: %d %v", code, out)
	}
	if owner := f.s.relief["http://a"].owner; owner != "v2" || f.spawned() != 2 {
		t.Fatalf("owner=%s spawned=%d", owner, f.spawned())
	}
	// A receiver that never finishes does not hold the machine for ever.
	f.facts(1)
	f.s.relief["http://a"].at = time.Now().Add(-2 * targetPatience)
	if code, out := f.ask("v3", reasonHostPressure); out["action"] != "spawned" {
		t.Fatalf("after targetPatience: %d %v", code, out)
	}
}

// While the relief waits for a machine to boot, its owner's retries go
// through and the other vnodes are answered as handled. An owner that
// stopped asking gives the relief up.
func TestHostReliefWaitingForAMachineStaysWithItsOwner(t *testing.T) {
	f := newPressureFixture(t)
	f.facts(0)
	f.script("spawn.sh", "#!/bin/sh\necho try >>\"$(dirname \"$0\")/spawned\"\necho 'waiting for a new machine' >&2\nexit 1\n")
	if code, _ := f.ask("v1", reasonHostPressure); code != http.StatusInternalServerError {
		t.Fatalf("no machine yet: %d", code)
	}
	if code, out := f.ask("v2", reasonHostPressure); !coalesced(code, out) {
		t.Fatalf("v2 while v1 waits for a boot: %d %v", code, out)
	}
	if code, _ := f.ask("v1", reasonHostPressure); code != http.StatusInternalServerError || f.spawned() != 2 {
		t.Fatalf("the owner's retry did not reach the hook: %d, %d attempts", code, f.spawned())
	}
	f.s.relief["http://a"].at = time.Now().Add(-2 * reliefWait)
	if code, _ := f.ask("v2", reasonHostPressure); code != http.StatusInternalServerError || f.s.relief["http://a"].owner != "v2" {
		t.Fatalf("v2 did not take over: %d %+v", code, f.s.relief["http://a"])
	}
}
