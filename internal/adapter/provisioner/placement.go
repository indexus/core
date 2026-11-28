package provisioner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// The provisioner decides on which machine a new vnode starts, from the
// resource facts the machine agents report (autoscale.Place). The hooks only
// report (facts.sh) and apply (spawn.sh). Without facts nothing is decided
// here and spawn.sh keeps its slot-count rule.

// placementMemory is how long a machine just chosen counts as hosting a
// joining vnode before its agent reports it: spawn.sh first accounts the
// fleet, which takes seconds, and two scale-ups decided in that time must not
// land on the same machine.
const placementMemory = 15 * time.Second

// waitingMemory is how long a request that found no machine counts as pending
// without renewal. A capacity-wait reply schedules a renewal inside this window.
const waitingMemory = placementMemory

// warmingPatience is how long an empty machine that just registered counts as
// supply to come while only its CPU keeps it from being a target. The CPU of
// a start falls within the agent's ten-second window; a machine still hot
// after a minute is not coming, and counts for nothing.
const warmingPatience = time.Minute

// bootWave limits outstanding machine starts while a shortage is observed.
// Repeated requests in the same observation interval share a window. A shortage
// that persists grows the window, gradually while the fleet has reuse headroom.
type bootWave struct {
	window           int
	raised, observed time.Time
}

func (w *bootWave) observe(now time.Time, reserve autoscale.Reserve) int {
	need := reserve.Boots()
	if need == 0 {
		*w = bootWave{}
		return 0
	}
	switch {
	case w.window == 0 || now.Sub(w.observed) >= warmingPatience:
		w.window = 1
		w.raised = now
	case now.Sub(w.raised) >= placementMemory:
		if reserve.ReuseHeadroom {
			w.window++
		} else {
			w.window *= 2
		}
		w.raised = now
	}
	w.observed = now
	w.window = min(w.window, need+reserve.Warming)
	return w.window
}

// placementHistory bounds the decisions kept for GET /v1/placement.
const placementHistory = 32

type incomingCopy struct {
	source string
	rate   float64
}

// placementRecord is one decision as GET /v1/placement shows it.
type placementRecord struct {
	WriteRate  float64   `json:"write_rate,omitempty"`
	ReverseOf  string    `json:"reverse_of,omitempty"` // skipped receiver that just sent comparable traffic to this donor
	Recheck    bool      `json:"from_wait,omitempty"`
	capMachine string    // reserved while the hook determines whether the machine cap is reached
	At         time.Time `json:"at"`
	Requester  string    `json:"requester_id"`
	PreferNear string    `json:"prefer_near"`
	Machines   int       `json:"machines"`
	autoscale.Placement
	// Applied is what the hook did: the kind it placed with ("cap" for the
	// fallback at the machine cap), "" when nothing started.
	Applied string `json:"applied"`
	// Reserve is the ready reserve handed to the hook, nil when unknown.
	Reserve    *autoscale.Reserve `json:"reserve,omitempty"`
	BootWindow int                `json:"boot_window,omitempty"`
	// Backlog includes all unexpired, unplaced vnode requests. Reserve.Pending
	// counts the offers their source machines can use concurrently.
	Backlog int `json:"backlog_vnodes"`
}

// A server serves one configured facts hook. Concurrent callers share only
// its in-flight read. Reservations and placement remain per request.
type fleetSurvey struct {
	mu     sync.Mutex
	active *factsRead
}

type factsRead struct {
	done     chan struct{}
	machines []autoscale.MachineFacts
}

func (f *fleetSurvey) read(probe func() []autoscale.MachineFacts) []autoscale.MachineFacts {
	f.mu.Lock()
	if current := f.active; current != nil {
		f.mu.Unlock()
		<-current.done
		return current.machines
	}
	current := &factsRead{done: make(chan struct{})}
	f.active = current
	f.mu.Unlock()

	current.machines = probe()
	f.mu.Lock()
	f.active = nil
	close(current.done)
	f.mu.Unlock()
	return current.machines
}

func (s *Server) machineFacts(dir string) []autoscale.MachineFacts {
	return s.survey.read(func() []autoscale.MachineFacts { return machineFacts(dir) })
}

// machineFacts runs the facts hook: one JSON line per machine. It is nil
// when the hook is missing or fails, or when no agent reports.
func machineFacts(dir string) []autoscale.MachineFacts {
	script := filepath.Join(dir, "facts.sh")
	if _, err := os.Stat(script); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(), "INDEXUS_LOCAL_DIR="+dir)
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("cannot read machine facts", "err", err)
		return nil
	}
	return parseMachineFacts(out)
}

// parseMachineFacts reads the lines of the facts hook; a line that is not
// {"agent": ..., "facts": {...}} is skipped.
func parseMachineFacts(out []byte) []autoscale.MachineFacts {
	var machines []autoscale.MachineFacts
	lines := bufio.NewScanner(bytes.NewReader(out))
	for lines.Scan() {
		var row struct {
			Agent string                 `json:"agent"`
			Facts autoscale.MachineFacts `json:"facts"`
		}
		if json.Unmarshal(lines.Bytes(), &row) != nil || row.Agent == "" {
			continue
		}
		row.Facts.Machine = row.Agent
		machines = append(machines, row.Facts)
	}
	return machines
}

// decidePlacement chooses the machine of the vnode requester asked for and
// returns the decision with the environment that hands it to spawn.sh. The
// environment is nil when there is nothing to hand over: no resource-aware hook is available. A resource refusal is explicit so the
// hook cannot replace it with an unchecked free slot.
func (s *Server) decidePlacement(machines []autoscale.MachineFacts, requester, preferNear string) (placementRecord, []string) {
	return s.planPlacement(machines, requester, preferNear, 0, false)
}

func (s *Server) planPlacement(machines []autoscale.MachineFacts, requester, preferNear string, rate float64, readyOnly bool) (placementRecord, []string) {
	record := placementRecord{WriteRate: rate, Recheck: readyOnly, At: time.Now(), Requester: requester, PreferNear: preferNear, Machines: len(machines)}
	if len(machines) == 0 {
		record.Placement = autoscale.Placement{Kind: autoscale.PlaceSlots, Reason: "no resource facts: slot-count rule of the hook"}
		return record, nil
	}
	// Reserve on a private copy of the rows. Their nested identities and
	// item counts remain immutable throughout the placement and relief paths.
	machines = slices.Clone(machines)
	s.mu.Lock()
	defer s.mu.Unlock()
	for target, at := range s.placed {
		if record.At.Sub(at) >= placementMemory {
			delete(s.placed, target)
			delete(s.incoming, target)
		}
	}
	for i, m := range machines {
		if _, ok := s.placed[m.Machine]; ok {
			// The pending reservation cannot itself justify a second join.
			machines[i].Free = 0
		}
	}
	donor := autoscale.MachineOf(machines, requester)
	record.Placement = autoscale.Place(machines, requester, true)
	// Concurrent half-splits in opposite directions largely cancel when their
	// net relief is at most half the gross movement: for positive a and b,
	// |a-b| <= (a+b)/2 exactly when min(a,b) >= max(a,b)/3. Try another
	// destination while the first offer is live.
	// The unchanged cap fallback can still use the original host.
	if incoming, ok := s.incoming[donor]; ok && incoming.source == record.Machine &&
		rate > 0 && min(rate, incoming.rate) >= max(rate, incoming.rate)/3 {
		for i := range machines {
			if machines[i].Machine == record.Machine {
				record.ReverseOf = record.Machine
				free := machines[i].Free
				machines[i].Free = 0
				record.Placement = autoscale.Place(machines, requester, true)
				machines[i].Free = free
				break
			}
		}
	}
	// A recheck only consumes an offer. Advancing bootWave or renewing the
	// backlog on each poll would turn one request into many capacity signals.
	if readyOnly && (record.Machine == "" || record.Kind == autoscale.PlaceSlots) {
		return record, nil
	}
	relief := s.relief[donor]
	hostRelief := relief != nil && relief.asking && relief.owner == requester
	if hostRelief {
		relief.dest = record.Machine
	}
	var atCap autoscale.Placement
	if record.Kind == autoscale.PlaceNew && record.Machine == "" {
		if hostRelief {
			atCap = autoscale.RemoteReliefAtCap(machines, donor)
		} else {
			atCap = autoscale.Place(machines, requester, false)
		}
	}
	record.capMachine = atCap.Machine
	reserved := record.reservedMachine()
	if reserved != "" {
		if s.placed == nil {
			s.placed = map[string]time.Time{}
		}
		s.placed[reserved] = record.At
		delete(s.incoming, reserved)
		if record.Machine != "" && donor != "" && donor != reserved && rate > 0 {
			if s.incoming == nil {
				s.incoming = make(map[string]incomingCopy)
			}
			s.incoming[reserved] = incomingCopy{source: donor, rate: rate}
		}
	}
	if record.Kind == autoscale.PlaceSlots {
		return record, nil
	}
	kind := record.Kind
	if kind == "" {
		kind = "refuse"
	}
	// A host can answer /facts before every resource meter is ready. Its
	// identity belongs to this survey even when the reserve is unknown.
	agents := make([]string, len(machines))
	for i, m := range machines {
		agents[i] = m.Machine
	}
	sampled, _ := json.Marshal(agents)
	env := []string{
		"INDEXUS_PLACE_KIND=" + kind,
		"INDEXUS_RESERVE_AGENTS=" + string(sampled),
		"INDEXUS_PLACE_AGENT=" + record.Machine,
		"INDEXUS_PLACE_REASON=" + record.Reason,
		"INDEXUS_PLACE_MACHINES=" + strconv.Itoa(len(machines)),
		"INDEXUS_PLACE_CAP_AGENT=" + atCap.Machine,
		"INDEXUS_PLACE_CAP_REASON=" + atCap.Reason,
	}
	pending := s.pendingLocked(machines, requester, preferNear, record)
	record.Backlog = len(s.waiting)
	unavailable := record.Machine
	if unavailable == "" {
		// This donor still has no receiver. Its excluded source cannot
		// satisfy the shortage while the opposing placement is reserved.
		unavailable = record.ReverseOf
	}
	if reserve, ok := autoscale.ReadyReserve(machines, unavailable, s.ReadyReservePct, pending, s.freshLocked(machines, record.At)); ok {
		reserve.WaitingRemote = record.Kind == autoscale.PlaceNew && record.Machine == ""
		record.Reserve = &reserve
		record.BootWindow = s.bootWave.observe(time.Now(), reserve)
		detail := fmt.Sprintf("floor=%d,pending=%d,ready=%d,warming=%d", reserve.Floor, reserve.Pending, reserve.Ready, reserve.Warming)
		if reserve.ReuseHeadroom {
			detail += ",reuse_headroom=true"
		}
		if record.Backlog != pending {
			detail += fmt.Sprintf(",backlog_vnodes=%d,source_lanes=%d", record.Backlog, pending)
		}
		env = append(env, "INDEXUS_READY_RESERVE_DEFICIT="+strconv.Itoa(reserve.Boots()), "INDEXUS_RESERVE_DETAIL="+detail,
			"INDEXUS_BOOT_ALLOWANCE="+strconv.Itoa(max(0, record.BootWindow-reserve.Warming)))
	}
	return record, env
}

func (r placementRecord) reservedMachine() string {
	if r.Machine != "" {
		return r.Machine
	}
	return r.capMachine
}

// Only a definitive pre-dispatch answer releases the receiver reservation
// and its transfer direction before facts catch up.
func (s *Server) releaseUndispatched(record placementRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if target := record.reservedMachine(); target != "" && s.placed[target] == record.At {
		delete(s.placed, target)
		delete(s.incoming, target)
	}
}

// pendingLocked notes whether the request found a machine and returns how
// many source copies the waiting vnodes can actually drive. Keep every
// requester until placed or expired so later ticks can launch the next wave.
func (s *Server) pendingLocked(machines []autoscale.MachineFacts, requester, preferNear string, record placementRecord) int {
	if s.waiting == nil {
		s.waiting = map[string]time.Time{}
	}
	key := requester
	if key == "" {
		key = "anonymous:" + preferNear
	}
	delete(s.waiting, key)
	if record.Kind == autoscale.PlaceNew && record.Machine == "" {
		s.waiting[key] = record.At
	}
	for k, at := range s.waiting {
		if record.At.Sub(at) >= waitingMemory {
			delete(s.waiting, k)
		}
	}
	if len(s.waiting) < 2 {
		return len(s.waiting)
	}
	// An omitted, unbounded or ambiguous source gets one offer per requester.
	// Work is linear in the reported vnodes and the (bounded-age) backlog.
	owners := make(map[string]*autoscale.MachineFacts, len(s.waiting))
	for i := range machines {
		for _, name := range machines[i].VNodes {
			if _, waiting := s.waiting[name]; !waiting {
				continue
			}
			if _, seen := owners[name]; seen {
				owners[name] = nil
			} else {
				owners[name] = &machines[i]
			}
		}
	}
	requests := make(map[*autoscale.MachineFacts]int)
	offers := 0
	for name := range s.waiting {
		m := owners[name]
		if m == nil || m.CopySlots <= 0 {
			offers++
			continue
		}
		requests[m]++
		if requests[m] <= m.CopySlots {
			offers++
		}
	}
	return offers
}

// freshLocked is the machines that first reported less than warmingPatience
// ago; it forgets the machines that report no more.
func (s *Server) freshLocked(machines []autoscale.MachineFacts, now time.Time) map[string]bool {
	seen := make(map[string]time.Time, len(machines))
	fresh := map[string]bool{}
	for _, m := range machines {
		at, ok := s.seen[m.Machine]
		if !ok {
			at = now
		}
		seen[m.Machine] = at
		fresh[m.Machine] = now.Sub(at) < warmingPatience
	}
	s.seen = seen
	return fresh
}

// appliedPlacement reads "placed kind=K agent=A" from the hook's output.
func appliedPlacement(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "placed kind="); ok {
			kind, _, _ := strings.Cut(rest, " ")
			return kind
		}
	}
	return ""
}

// notePlacement logs a decision and keeps it for GET /v1/placement.
func (s *Server) notePlacement(record placementRecord) {
	slog.Info("placement decided", "kind", record.Kind, "machine", record.Machine, "reason", record.Reason,
		"applied", record.Applied, "requester", record.Requester, "prefer_near", record.PreferNear, "machines", record.Machines)
	decided := record.Kind
	switch {
	case decided == "":
		decided = "none"
	case decided == autoscale.PlaceNew && record.Machine == "":
		decided = "new_wait"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.placementDecided == nil {
		s.placementDecided, s.placementApplied = map[string]int{}, map[string]int{}
	}
	s.placementDecided[decided]++
	if record.Applied != "" {
		s.placementApplied[record.Applied]++
	}
	s.placements = append(s.placements, record)
	if len(s.placements) > placementHistory {
		s.placements = s.placements[1:]
	}
}

// placement serves the placement metrics: decisions and applications counted
// by kind, and the last decisions with their reason.
func (s *Server) placement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	body := map[string]any{"decided": copyCounts(s.placementDecided), "applied": copyCounts(s.placementApplied), "last": append([]placementRecord{}, s.placements...)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, body)
}

func copyCounts(counts map[string]int) map[string]int {
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	return out
}
