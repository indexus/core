// Package provisioner owns AWS/local process orchestration, not certificates or data authority.
package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/tree"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	NetworkPublicKey string
	BootstrapIPs     []string
	P2PPort          int
	// Scale-out (RunInstances or local process spawn)
	LaunchTemplateID string
	Region           string
	SpawnMaxExtra    int // 0 delegates capacity to LocalDir; never unbounded direct EC2
	ScaleCooldown    time.Duration
	ReadyReservePct  float64 // percentage of fleet CPU use above 30% reserved for receivers
	ProjectTag       string
	// LocalDir, when set, replaces EC2 with scripts under that directory
	// (spawn.sh / terminate.sh / count.sh). LaunchTemplateID may be "local".
	LocalDir string

	survey        fleetSurvey
	mu            sync.Mutex
	lastScale     time.Time
	lastDownscale time.Time
	scaleCount    int
	scaleReserved int                  // in-flight launches not yet reflected in inventory
	lastScaleBy   map[string]time.Time // per requester_id cooldown
	// asked: targets (prefer_near) already asked for, and when. A second ask
	// for the same target within targetPatience starts nothing: it is the
	// same vnode asked again (a retry after a timeout), not a new need.
	asked         map[string]time.Time
	drainingID    string // instance currently holding SoftLeave drain lock
	drainingSince time.Time
	// placed: machines lately chosen for a vnode, and when (placementMemory).
	placed map[string]time.Time
	// waiting: the requesters that found no machine, and when they last asked
	// (waitingMemory). seen: when each machine first reported (warmingPatience).
	waiting, seen map[string]time.Time
	bootWave      bootWave
	// relief: the host-pressure spawn in flight per donor machine (pressure.go).
	relief map[string]*hostRelief
	// Placement metrics (GET /v1/placement).
	placementDecided, placementApplied map[string]int
	placements                         []placementRecord

	// DownCooldown is the quiet period after a successful terminate before
	// another SoftLeave may take the drain lock. Keeps the mesh from
	// collapsing faster than transfers can rebalance.
	DownCooldown time.Duration

	// GlobalAPICooldown is a short anti-stampede pause between AWS/local
	// launch batches (not a mesh-size target). Defaults to 2s when unset.
	GlobalAPICooldown time.Duration
}

const drainLockTTL = 5 * time.Minute

// targetPatience is how long a target stays "already starting" (see asked);
// the node's own patience for a scale-up to land is the same.
const targetPatience = 3 * time.Minute

func NewServer(publicKey string) *Server {
	return &Server{NetworkPublicKey: publicKey, P2PPort: 21000, SpawnMaxExtra: 1, ScaleCooldown: 3 * time.Minute, ReadyReservePct: 15, DownCooldown: 2 * time.Minute, GlobalAPICooldown: 2 * time.Second, ProjectTag: "indexus-aws", lastScaleBy: map[string]time.Time{}}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/scale", s.scale)
	mux.HandleFunc("/v1/downscale", s.downscale)
	mux.HandleFunc("/v1/drain-lock", s.drainLock)
	mux.HandleFunc("/v1/drain-unlock", s.drainUnlock)
	mux.HandleFunc("/v1/placement", s.placement)
	return mux
}
func (s *Server) Serve(ctx context.Context, addr string, authorize func(*http.Request) bool) error {
	handler := s.Handler()
	server := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorize == nil || !authorize(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type scaleReq struct {
	RequesterID  string `json:"requester_id"`
	LocalInserts int64  `json:"local_inserts"`
	OwnedZones   int    `json:"owned_zones"`
	Reason       string `json:"reason"`
	Role         string `json:"role"`
	// SpawnCount requests up to N parallel instances in one scale call
	// (capped by remaining SpawnMaxExtra capacity). Default 1.
	SpawnCount int    `json:"spawn_count"`
	PreferNear string `json:"prefer_near"`
	// PreferNears are distinct XOR targets (one per instance). When empty the
	// issuer fans PreferNear into N adjacent slices.
	PreferNears []string `json:"prefer_nears"`
	// TransfersInFlight is the requester's open transfers, when it says so.
	// It is logged with a host-pressure request and decides nothing.
	TransfersInFlight *int `json:"transfers_in_flight"`
}

// scaleRefusal is an answer that ends a scale request before any launch.
type scaleRefusal struct {
	status int
	body   any
}

// scale starts up to three instances for one requester. Phases, in order:
//  1. decode the request and check that this server can launch at all;
//  2. admit it under the lock (machine already relieved, target already
//     asked, cooldowns, capacity) and reserve its launches;
//  3. choose one XOR target per instance;
//  4. launch them in parallel, outside the lock;
//  5. settle the reservation and the cooldowns under the lock;
//  6. answer.
func (s *Server) scale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req scaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if refusal := s.launchDisabled(); refusal != nil {
		writeJSON(w, refusal.status, refusal.body)
		return
	}
	donor, machines := s.pressureDonor(req)
	n, refusal := s.admitScale(req, donor, machines)
	if refusal != nil {
		writeJSON(w, refusal.status, refusal.body)
		return
	}

	preferNear := req.PreferNear
	if preferNear == "" {
		preferNear = req.RequesterID
	}
	ids, tags, firstErr := s.launchNear(scaleTargets(req, preferNear, n), req.RequesterID)
	s.settleScale(req, donor, n, len(ids))

	if len(ids) == 0 {
		// Multi-target hooks can have mixed outcomes. Only a single hook
		// confirming no vnode was dispatched may give a short retry hint.
		if n == 1 && errors.Is(firstErr, errWaitingForCapacity) {
			// Renew the donor's waiting entry before it expires. Spread renewals
			// over the middle third of its lifetime to avoid synchronized waves.
			spread := waitingMemory / 3
			refusal := cooldownRefusal("waiting_for_capacity", spread+rand.N(spread))
			writeJSON(w, refusal.status, refusal.body)
			return
		}
		errMsg := "spawn failed"
		if firstErr != nil {
			errMsg = firstErr.Error()
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errMsg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":         "spawned",
		"instance_id":    ids[0],
		"instance_ids":   ids,
		"spawned":        len(ids),
		"requester_id":   req.RequesterID,
		"prefer_near":    preferNear,
		"prefer_nears":   tags,
		"reason":         req.Reason,
		"local_inserts":  req.LocalInserts,
		"bootstrap_ips":  s.BootstrapIPs,
		"network_pubkey": s.NetworkPublicKey,
	})
}

// launchDisabled refuses when the configuration gives nothing to launch with.
func (s *Server) launchDisabled() *scaleRefusal {
	if s.LaunchTemplateID == "" && s.LocalDir == "" {
		return &scaleRefusal{http.StatusServiceUnavailable, map[string]string{
			"error": "scale disabled or launch template missing",
		}}
	}
	if s.SpawnMaxExtra < 0 || s.SpawnMaxExtra == 0 && s.LocalDir == "" {
		return &scaleRefusal{http.StatusServiceUnavailable, map[string]string{"error": "capacity delegation requires localDir"}}
	}
	return nil
}

func cooldownRefusal(reason string, wait time.Duration) *scaleRefusal {
	return &scaleRefusal{http.StatusTooManyRequests, map[string]any{
		"error": reason, "retry_after_ms": max(int64(1), wait.Milliseconds()),
	}}
}

// admitScale decides under the lock whether the request may launch, and how
// many instances. An admitted request has its target noted as asked and its
// n launches reserved; the caller gives them back with settleScale. donor is
// the requester's machine for a host-pressure request ("" otherwise), and
// machines the facts it was found in.
func (s *Server) admitScale(req scaleReq, donor string, machines []autoscale.MachineFacts) (n int, refusal *scaleRefusal) {
	apiCD := s.GlobalAPICooldown
	if apiCD <= 0 {
		apiCD = 2 * time.Second
	}
	// Per-requester cooldown uses ScaleCooldown (node-side is primary; this
	// stops one hot node from stampeding). Other requesters may proceed.
	requesterCD := s.ScaleCooldown
	if requesterCD <= 0 {
		requesterCD = time.Minute
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if donor != "" && s.coalescedLocked(req, donor, machines, time.Now()) {
		return 0, &scaleRefusal{http.StatusOK, map[string]any{"status": "already_starting", "prefer_near": req.PreferNear, "started": 0, "coalesced": reasonHostPressure, "machine": donor}}
	}
	if req.PreferNear != "" {
		if t, ok := s.asked[req.PreferNear]; ok && time.Since(t) < targetPatience {
			return 0, &scaleRefusal{http.StatusOK, map[string]any{"status": "already_starting", "prefer_near": req.PreferNear, "started": 0}}
		}
	}
	if wait := time.Until(s.lastScale.Add(apiCD)); wait > 0 {
		return 0, cooldownRefusal("api_cooldown", wait)
	}
	if req.RequesterID != "" && s.lastScaleBy != nil {
		if wait := time.Until(s.lastScaleBy[req.RequesterID].Add(requesterCD)); wait > 0 {
			return 0, cooldownRefusal("requester_cooldown", wait)
		}
	}
	remaining, refusal := s.remainingCapacityLocked()
	if refusal != nil {
		return 0, refusal
	}

	if req.PreferNear != "" {
		s.noteAskedLocked(req.PreferNear)
	}
	n = req.SpawnCount
	if n < 1 {
		n = 1
	}
	if n > remaining {
		n = remaining
	}
	if n > 3 {
		n = 3
	}
	s.scaleReserved += n
	if donor != "" {
		s.relief[donor] = &hostRelief{owner: req.RequesterID, asking: true, at: time.Now()}
	}
	return n, nil
}

// remainingCapacityLocked is how many instances one call may still start.
// With capacity delegated to the local hooks (SpawnMaxExtra == 0) it is the
// per-call burst; otherwise it is what SpawnMaxExtra leaves after the live
// inventory and the launches in flight.
func (s *Server) remainingCapacityLocked() (int, *scaleRefusal) {
	if s.SpawnMaxExtra <= 0 {
		return 3, nil // per-call burst; local hooks own fleet capacity when delegated
	}
	running := s.countSpawnedLocked()
	// External terminates leave scaleCount high while inventory is empty.
	if s.scaleCount > running {
		slog.Info("reconcile scaleCount to live inventory",
			"was", s.scaleCount, "running", running)
		s.scaleCount = running
	}
	// Cap is SpawnMaxExtra only — not a target mesh size.
	remaining := s.SpawnMaxExtra - running - s.scaleReserved
	if remaining > 0 {
		return remaining, nil
	}
	// scaleReserved is copied into the answer under the lock: another
	// request's release (scaleReserved -= n) races a read after Unlock.
	return 0, &scaleRefusal{http.StatusConflict, map[string]any{
		"error":    "spawn max reached",
		"running":  running,
		"reserved": s.scaleReserved,
		"max":      s.SpawnMaxExtra,
	}}
}

// noteAskedLocked records the target as asked now and forgets the targets
// asked longer ago than targetPatience.
func (s *Server) noteAskedLocked(target string) {
	if s.asked == nil {
		s.asked = map[string]time.Time{}
	}
	for k, t := range s.asked {
		if time.Since(t) >= targetPatience {
			delete(s.asked, k)
		}
	}
	s.asked[target] = time.Now()
}

// scaleTargets gives one XOR target per instance: those of the request, else
// n adjacent slices around preferNear, padded with preferNear itself.
func scaleTargets(req scaleReq, preferNear string, n int) []string {
	targets := req.PreferNears
	if len(targets) == 0 {
		if built, err := tree.BASE64.PreferNearTargets(preferNear, n); err == nil && len(built) > 0 {
			targets = built
		}
	}
	for len(targets) < n {
		targets = append(targets, preferNear)
	}
	return targets[:n]
}

// launchNear starts one instance per target in parallel. It returns the ids
// started with their targets, in target order, and the first error in that
// order.
func (s *Server) launchNear(targets []string, requester string) (ids, tags []string, firstErr error) {
	type spawnResult struct {
		id  string
		tag string
		err error
	}
	n := len(targets)
	results := make([]spawnResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := targets[i]
			id, err := s.runInstancesNear(tag, requester)
			results[i] = spawnResult{id: id, tag: tag, err: err}
		}(i)
	}
	wg.Wait()

	ids = make([]string, 0, n)
	tags = make([]string, 0, n)
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if r.id != "" {
			ids = append(ids, r.id)
			tags = append(tags, r.tag)
		}
	}
	return ids, tags, firstErr
}

// settleScale gives back the n reserved launches and records what started:
// the cooldowns start only when at least one instance did.
func (s *Server) settleScale(req scaleReq, donor string, n, started int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.relief[donor]; r != nil && r.owner == req.RequesterID {
		r.asking, r.placed, r.at = false, started > 0, time.Now()
	}
	s.scaleReserved -= n
	if s.scaleReserved < 0 {
		s.scaleReserved = 0
	}
	if started == 0 {
		if req.PreferNear != "" {
			// Nothing started (e.g. no free slot yet while a machine boots): clear
			// asked so the donor's retry is not swallowed as "already_starting" for
			// targetPatience (3 min), which starved a whole quadrant (skew 5x).
			delete(s.asked, req.PreferNear)
		}
		return
	}
	s.lastScale = time.Now()
	s.scaleCount += started
	if req.RequesterID != "" {
		if s.lastScaleBy == nil {
			s.lastScaleBy = make(map[string]time.Time)
		}
		s.lastScaleBy[req.RequesterID] = s.lastScale
	}
}

func (s *Server) localMode() bool {
	return s.LocalDir != "" || s.LaunchTemplateID == "local"
}

func (s *Server) runInstancesNear(preferNear, requester string) (string, error) {
	if s.localMode() {
		return s.runLocalSpawnNear(preferNear, requester)
	}
	region := s.Region
	if region == "" {
		region = "eu-west-3"
	}
	// PreferNear is tagged so userdata can set INDEXUS_PREFER_NEAR for XOR placement.
	tags := fmt.Sprintf(
		"ResourceType=instance,Tags=[{Key=Name,Value=%s-spawned},{Key=Project,Value=%s},{Key=Role,Value=spawned}",
		s.ProjectTag, s.ProjectTag,
	)
	if preferNear != "" {
		// Escape is minimal: node names are url64 (no commas/braces).
		tags += fmt.Sprintf(",{Key=PreferNear,Value=%s}", preferNear)
	}
	tags += "]"
	args := []string{
		"ec2", "run-instances",
		"--region", region,
		"--launch-template", "LaunchTemplateId=" + s.LaunchTemplateID + ",Version=$Latest",
		"--tag-specifications", tags,
		"--query", "Instances[0].InstanceId",
		"--output", "text",
	}
	cmd := exec.Command("aws", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run-instances: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if id == "" || !strings.HasPrefix(id, "i-") {
		return "", fmt.Errorf("unexpected instance id %q", id)
	}
	slog.Info("instance spawned", "instance", id, "prefer_near", preferNear)
	return id, nil
}

// A hook may report this temporary outcome only before dispatch to a vnode agent.
// A fleet boot can be in progress independently.
var errWaitingForCapacity = errors.New("waiting for receiver capacity")

func (s *Server) runLocalSpawnNear(preferNear, requester string) (string, error) {
	dir := s.LocalDir
	if dir == "" {
		dir = "ops/local"
	}
	// The machine is chosen here from the agents' resource facts; the hook
	// applies it, or keeps its slot-count rule when there is no decision.
	decision, place := s.decidePlacement(s.machineFacts(dir), requester, preferNear)
	script := filepath.Join(dir, "spawn.sh")
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"INDEXUS_LOCAL_DIR="+dir,
		"INDEXUS_PROJECT="+s.ProjectTag,
		"INDEXUS_BOOTSTRAP_IPS="+strings.Join(s.BootstrapIPs, ","),
		"INDEXUS_P2P_PORT="+fmt.Sprintf("%d", s.P2PPort),
		"INDEXUS_PREFER_NEAR="+preferNear,
	)
	cmd.Env = append(cmd.Env, place...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		decision.Applied = appliedPlacement(string(out))
	}
	s.notePlacement(decision)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 75 && strings.HasPrefix(string(out), "INDEXUS_NO_RECEIVER\n") {
			return "", fmt.Errorf("%w: %s", errWaitingForCapacity, strings.TrimSpace(string(out)))
		}
		return "", fmt.Errorf("local spawn: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	// spawn.sh may print logs then the id on the last line.
	if lines := strings.Split(id, "\n"); len(lines) > 1 {
		id = strings.TrimSpace(lines[len(lines)-1])
	}
	if id == "" || !strings.HasPrefix(id, "local-") {
		return "", fmt.Errorf("unexpected local instance id %q (out=%q)", id, string(out))
	}
	slog.Info("local process spawned", "instance", id)
	return id, nil
}

func (s *Server) countSpawnedLocked() int {
	if s.localMode() {
		return s.countLocalSpawned()
	}
	region := s.Region
	if region == "" {
		region = "eu-west-3"
	}
	cmd := exec.Command("aws", "ec2", "describe-instances",
		"--region", region,
		"--filters",
		"Name=tag:Name,Values="+s.ProjectTag+"-spawned",
		"Name=instance-state-name,Values=pending,running",
		"--query", "length(Reservations[].Instances[])",
		"--output", "text",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("cannot count spawned instances", "err", err, "output", string(out))
		return s.scaleCount
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

func (s *Server) countLocalSpawned() int {
	dir := s.LocalDir
	if dir == "" {
		dir = "ops/local"
	}
	script := filepath.Join(dir, "count.sh")
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "INDEXUS_LOCAL_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("cannot count local processes", "err", err, "output", string(out))
		return s.scaleCount
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

type downscaleReq struct {
	RequesterID string `json:"requester_id"`
	InstanceID  string `json:"instance_id"`
	Reason      string `json:"reason"`
}

func (s *Server) drainLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downscaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InstanceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instance_id required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cooldown := s.DownCooldown
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	// Same holder renewing mid-SoftLeave must not be blocked by DownCooldown.
	renewing := s.drainingID == req.InstanceID
	if !renewing && !s.lastDownscale.IsZero() && time.Since(s.lastDownscale) < cooldown {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":          "downscale cooldown",
			"retry_after_ms": (cooldown - time.Since(s.lastDownscale)).Milliseconds(),
		})
		return
	}
	// Expire stale locks so a crashed SoftLeave cannot block the cluster forever.
	if s.drainingID != "" && s.drainingID != req.InstanceID {
		if s.drainingSince.IsZero() || time.Since(s.drainingSince) > drainLockTTL {
			slog.Warn("expiring stale drain lock", "holder", s.drainingID)
			s.drainingID = ""
			s.drainingSince = time.Time{}
		} else {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "another drain in progress",
				"draining_id": s.drainingID,
			})
			return
		}
	}
	s.drainingID = req.InstanceID
	s.drainingSince = time.Now()
	writeJSON(w, http.StatusOK, map[string]any{"locked": true, "instance_id": req.InstanceID})
}

func (s *Server) drainUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downscaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InstanceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instance_id required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drainingID == req.InstanceID {
		s.drainingID = ""
		s.drainingSince = time.Time{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"unlocked": true})
}

func (s *Server) downscale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downscaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.InstanceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instance_id required"})
		return
	}

	if s.localMode() {
		s.terminateLocal(w, req)
		return
	}

	region := s.Region
	if region == "" {
		region = "eu-west-3"
	}
	// Safety: only terminate Role=spawned instances (never bootstrap).
	roleCmd := exec.Command("aws", "ec2", "describe-instances",
		"--region", region,
		"--instance-ids", req.InstanceID,
		"--query", "Reservations[0].Instances[0].Tags[?Key=='Role'].Value|[0]",
		"--output", "text",
	)
	roleOut, err := roleCmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(roleOut)) != "spawned" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("refuse terminate: role=%q err=%v", strings.TrimSpace(string(roleOut)), err),
		})
		return
	}
	// Prefer holders of the drain lock (SoftLeave completed under lock).
	s.mu.Lock()
	if s.drainingID != "" && s.drainingID != req.InstanceID {
		if s.drainingSince.IsZero() || time.Since(s.drainingSince) > drainLockTTL {
			slog.Warn("expiring stale drain lock", "holder", s.drainingID)
			s.drainingID = ""
			s.drainingSince = time.Time{}
		} else {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "drain lock held by another instance",
				"draining_id": s.drainingID,
			})
			return
		}
	}
	s.mu.Unlock()

	cmd := exec.Command("aws", "ec2", "terminate-instances",
		"--region", region,
		"--instance-ids", req.InstanceID,
		"--query", "TerminatingInstances[0].InstanceId",
		"--output", "text",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v: %s", err, string(out))})
		return
	}
	s.mu.Lock()
	if s.scaleCount > 0 {
		s.scaleCount--
	}
	if s.drainingID == req.InstanceID {
		s.drainingID = ""
		s.drainingSince = time.Time{}
	}
	s.lastDownscale = time.Now()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"action":       "terminated",
		"instance_id":  strings.TrimSpace(string(out)),
		"requester_id": req.RequesterID,
		"reason":       req.Reason,
	})
}

func (s *Server) terminateLocal(w http.ResponseWriter, req downscaleReq) {
	if !strings.HasPrefix(req.InstanceID, "local-") {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "refuse terminate: local mode only accepts local-* ids",
		})
		return
	}
	s.mu.Lock()
	if s.drainingID != "" && s.drainingID != req.InstanceID {
		if s.drainingSince.IsZero() || time.Since(s.drainingSince) > drainLockTTL {
			slog.Warn("expiring stale drain lock", "holder", s.drainingID)
			s.drainingID = ""
			s.drainingSince = time.Time{}
		} else {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "drain lock held by another instance",
				"draining_id": s.drainingID,
			})
			return
		}
	}
	s.mu.Unlock()

	dir := s.LocalDir
	if dir == "" {
		dir = "ops/local"
	}
	script := filepath.Join(dir, "terminate.sh")
	cmd := exec.Command(script, req.InstanceID)
	cmd.Env = append(os.Environ(), "INDEXUS_LOCAL_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("local terminate: %v (%s)", err, strings.TrimSpace(string(out))),
		})
		return
	}
	s.mu.Lock()
	if s.scaleCount > 0 {
		s.scaleCount--
	}
	if s.drainingID == req.InstanceID {
		s.drainingID = ""
		s.drainingSince = time.Time{}
	}
	s.lastDownscale = time.Now()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"action":       "terminated",
		"instance_id":  req.InstanceID,
		"requester_id": req.RequesterID,
		"reason":       req.Reason,
	})
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
