package provisioner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/auth"
)

func writeLocalStubs(t *testing.T, dir string, countOut string) {
	t.Helper()
	mustWrite := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("count.sh", "#!/bin/sh\necho "+countOut+"\n")
	mustWrite("spawn.sh", "#!/bin/sh\necho local-0\n")
	mustWrite("terminate.sh", "#!/bin/sh\nexit 0\n")
}

func TestScaleReconcilesGhostScaleCount(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")

	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LaunchTemplateID = "local"
	s.LocalDir = dir
	s.SpawnMaxExtra = 2
	s.ScaleCooldown = time.Millisecond
	// Simulate prior launches that were terminated outside SoftLeave.
	s.scaleCount = 2

	body, _ := json.Marshal(map[string]any{
		"requester_id":  "lab",
		"local_inserts": 9999,
		"spawn_count":   1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.scale(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200 (ghost scaleCount must not block)", rec.Code, rec.Body.String())
	}
	if s.scaleCount != 1 {
		t.Fatalf("scaleCount=%d want 1 after successful spawn", s.scaleCount)
	}
}

func TestDrainLockRespectsDownCooldown(t *testing.T) {
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.DownCooldown = time.Hour
	s.lastDownscale = time.Now()

	body, _ := json.Marshal(map[string]any{
		"requester_id": "lab",
		"instance_id":  "local-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/drain-lock", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.drainLock(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s want 429 during down cooldown", rec.Code, rec.Body.String())
	}

	// Holder renewing mid-drain must not be blocked.
	s.drainingID = "local-1"
	s.drainingSince = time.Now()
	req = httptest.NewRequest(http.MethodPost, "/v1/drain-lock", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	s.drainLock(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s want 200", rec.Code, rec.Body.String())
	}
}

func TestScaleParallelRequestersReserveSlots(t *testing.T) {
	dir := t.TempDir()
	// Slow spawn so overlapping requests both see reserved capacity.
	mustWrite := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("count.sh", "#!/bin/sh\necho 0\n")
	mustWrite("spawn.sh", "#!/bin/sh\nsleep 0.2\necho local-$$\n")
	mustWrite("terminate.sh", "#!/bin/sh\nexit 0\n")

	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LaunchTemplateID = "local"
	s.LocalDir = dir
	s.SpawnMaxExtra = 2
	s.ScaleCooldown = time.Millisecond
	s.GlobalAPICooldown = time.Millisecond

	// First request reserves 2; second should 409.
	done := make(chan int, 2)
	go func() {
		body, _ := json.Marshal(map[string]any{
			"requester_id": "a", "spawn_count": 2, "prefer_near": "aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.scale(rec, req)
		done <- rec.Code
	}()
	time.Sleep(50 * time.Millisecond)
	body, _ := json.Marshal(map[string]any{
		"requester_id": "b", "spawn_count": 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.scale(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second status=%d body=%s want 409 while slots reserved", rec.Code, rec.Body.String())
	}
	code := <-done
	if code != http.StatusOK {
		t.Fatalf("first status=%d want 200", code)
	}
}

func TestScaleMaxWhenReallyFull(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "2")

	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LaunchTemplateID = "local"
	s.LocalDir = dir
	s.SpawnMaxExtra = 2
	s.ScaleCooldown = time.Millisecond
	s.scaleCount = 2

	body, _ := json.Marshal(map[string]any{
		"requester_id":  "lab",
		"local_inserts": 9999,
		"spawn_count":   1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.scale(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s want 409 when live inventory is full", rec.Code, rec.Body.String())
	}
}

func TestMissingProviderRejectsScale(t *testing.T) {
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	body := bytes.NewBufferString(`{"ip":"127.0.0.1","pub_key":"unused","node_id":"node"}`)
	recorder := httptest.NewRecorder()
	s.scale(recorder, httptest.NewRequest(http.MethodPost, "/v1/scale", body))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing provider accepted scale: status=%d", recorder.Code)
	}
}

// The same target asked twice (a retry after a timeout) starts one vnode.
func TestScaleIsIdempotentPerTarget(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LaunchTemplateID, s.LocalDir, s.SpawnMaxExtra = "local", dir, 4
	s.ScaleCooldown, s.GlobalAPICooldown = time.Nanosecond, time.Nanosecond
	ask := func(requester, near string) map[string]any {
		body, _ := json.Marshal(map[string]any{"requester_id": requester, "spawn_count": 1, "prefer_near": near})
		rec := httptest.NewRecorder()
		s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		time.Sleep(time.Millisecond)
		return out
	}
	ask("hot", "AAAAAAAAAAAAAAAA")
	if out := ask("hot", "AAAAAAAAAAAAAAAA"); out["status"] != "already_starting" {
		t.Fatalf("second ask for the same target: %v", out)
	}
	if s.scaleCount != 1 {
		t.Fatalf("scaleCount=%d want 1", s.scaleCount)
	}
	if out := ask("hot", "BBBBBBBBBBBBBBBB"); out["status"] == "already_starting" {
		t.Fatalf("another target was refused: %v", out)
	}
}

// A failed spawn (e.g. no free slot while EC2 boots) must NOT poison asked for
// targetPatience; the donor's retry must succeed as soon as a slot appears.
func TestFailedSpawnDoesNotPoisonTargetIdempotency(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")
	// First spawn fails ("no free slot on any agent").
	failSpawn := filepath.Join(dir, "spawn.sh")
	if err := os.WriteFile(failSpawn, []byte("#!/bin/sh\necho 'no free slot' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LaunchTemplateID, s.LocalDir, s.SpawnMaxExtra = "local", dir, 4
	s.ScaleCooldown, s.GlobalAPICooldown = time.Nanosecond, time.Nanosecond

	body, _ := json.Marshal(map[string]any{"requester_id": "hot", "spawn_count": 1, "prefer_near": "CCCCCCCCCCCCCCCC"})
	rec := httptest.NewRecorder()
	s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("first failed spawn status=%d want 500", rec.Code)
	}

	// Machine finishes booting: spawn.sh now succeeds.
	if err := os.WriteFile(failSpawn, []byte("#!/bin/sh\necho local-new\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	s.scale(rec2, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry after slot freed: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &out)
	if out["status"] == "already_starting" || out["action"] != "spawned" {
		t.Fatalf("failed spawn poisoned asked: %v", out)
	}
}

// A fleet-managed hook is the capacity authority. The old startup vnode limit
// must not stop it after a policy increase or count inventory under s.mu.
func TestScaleDelegatedCapacityBeyondStartupCount(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "128")
	if err := os.WriteFile(filepath.Join(dir, "count.sh"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LocalDir = dir
	s.SpawnMaxExtra = 0
	s.scaleCount = 128
	body := []byte(`{"requester_id":"lab","local_inserts":9999,"spawn_count":1}`)
	rec := httptest.NewRecorder()
	s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || s.scaleCount != 129 {
		t.Fatalf("status=%d count=%d body=%s", rec.Code, s.scaleCount, rec.Body.String())
	}
}

func TestScaleRejectsUnboundedDirectEC2(t *testing.T) {
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(auth.PublicKeyBase64(key.Public))
	s.LaunchTemplateID = "must-not-launch"
	s.SpawnMaxExtra = 0
	rec := httptest.NewRecorder()
	s.scale(rec, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewBufferString(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestScaleRefusalCarriesRemainingCooldown(t *testing.T) {
	for _, tc := range []struct {
		reason string
		global bool
	}{{"api_cooldown", true}, {"requester_cooldown", false}} {
		t.Run(tc.reason, func(t *testing.T) {
			s := NewServer("")
			s.GlobalAPICooldown = time.Minute
			s.ScaleCooldown = time.Minute
			since := time.Now().Add(-40 * time.Second)
			if tc.global {
				s.lastScale = since
			} else {
				s.lastScaleBy["donor"] = since
			}
			_, refusal := s.admitScale(scaleReq{RequesterID: "donor"}, "", nil)
			if refusal == nil || refusal.status != http.StatusTooManyRequests {
				t.Fatalf("expected cooldown refusal, got %v", refusal)
			}
			body := refusal.body.(map[string]any)
			ms := body["retry_after_ms"].(int64)
			if body["error"] != tc.reason || ms <= 0 || ms > 20_000 {
				t.Fatalf("expected remaining cooldown, got %v", body)
			}
		})
	}
}

func TestOnlyConfirmedUnplacedHookSendsCapacityRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   string
		burst int
		want  int
	}{
		{"waiting", "INDEXUS_NO_RECEIVER\nbusy", 1, 429},
		{"unrelated-error", "agent connection interrupted", 1, 500},
		{"multiple-targets", "INDEXUS_NO_RECEIVER\nbusy", 2, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLocalStubs(t, dir, "0")
			if err := os.WriteFile(filepath.Join(dir, "spawn.sh"), []byte("#!/bin/sh\necho '"+tc.out+"' >&2\nexit 75\n"), 0755); err != nil {
				t.Fatal(err)
			}
			s := NewServer("")
			s.SpawnMaxExtra = 0
			s.LocalDir = dir
			body, _ := json.Marshal(scaleReq{RequesterID: "source", PreferNear: "target", SpawnCount: tc.burst})
			response := httptest.NewRecorder()
			s.scale(response, httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body)))
			if response.Code != tc.want {
				t.Fatalf("response %d %s", response.Code, response.Body.String())
			}
			if tc.want == 429 {
				var rejection struct {
					Reason string `json:"error"`
					Wait   int64  `json:"retry_after_ms"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &rejection); err != nil {
					t.Fatal(err)
				}
				if rejection.Reason != "waiting_for_capacity" || rejection.Wait < (waitingMemory/3).Milliseconds() || rejection.Wait >= (2*waitingMemory/3).Milliseconds() {
					t.Fatalf("invalid capacity wait %+v", rejection)
				}
				if len(s.asked) != 0 || s.scaleReserved != 0 || s.scaleCount != 0 {
					t.Fatal("unplaced vnode retained a launch reservation")
				}
			}
		})
	}
}
