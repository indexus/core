package convergent

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The simulated network loses, duplicates, delays and reorders messages,
// cuts links by partition sides, crashes processes (the disk survives) and
// pauses them (SIGSTOP). Each node has its own clock rate. Time advances in
// steps of 10 ms. Everything is drawn from one seed.

const step = 10

type simNode struct {
	name   Member
	inc    Incarnation
	rep    *Replica
	disk   *State
	up     bool
	paused bool
	rate   float64
	offset int64

	lastMembers []Member
}

type flight struct {
	at int64
	m  Message
}

type clientAck struct {
	Ack
	real int64
	by   Member
}

type event struct {
	at int64
	do func()
}

type world struct {
	t       int64
	rng     *rand.Rand
	cfg     Config
	members []Member
	nodes   map[Member]*simNode
	side    map[Member]int
	net     []flight
	drop    float64
	dup     float64
	delay   int64

	lost   map[Member]bool
	events []event
	// exempt marks acknowledgements a scenario allows to miss: writes on a
	// lost disk, or by an incarnation retired while it was alive.
	exempt func(a clientAck) bool
	// exemptRead marks acknowledgements exact reads may miss but repair
	// must still merge (A1 holds).
	exemptRead func(a clientAck) bool
	writes     bool
	ids        int
	ridSeq     int
	acks       []clientAck
	exact      int // exact reads checked
	degraded   int // degraded reads served
	failures   []string
}

func newWorld(seed uint64) *world {
	w := &world{
		rng:        rand.New(rand.NewPCG(seed, 7)),
		cfg:        DefaultConfig(),
		members:    []Member{"o", "r1", "r2"},
		nodes:      map[Member]*simNode{},
		side:       map[Member]int{},
		lost:       map[Member]bool{},
		exempt:     func(clientAck) bool { return false },
		exemptRead: func(clientAck) bool { return false },
		drop:       0.02,
		dup:        0.02,
		delay:      40,
		writes:     true,
		ids:        30,
	}
	for _, m := range w.members {
		w.addNode(m, w.members)
	}
	return w
}

func (w *world) addNode(m Member, members []Member) {
	n := &simNode{name: m, inc: Incarnation(string(m) + "#1"), up: true, rate: 0.95 + 0.1*w.rng.Float64(), offset: int64(w.rng.IntN(100000))}
	n.disk = NewState()
	w.nodes[m] = n
	n.rep = Open(m, n.inc, members, w.cfg, n.disk, w.local(n))
}

func (w *world) at(t int64, do func()) { w.events = append(w.events, event{t, do}) }

// reconfigure publishes a new R(z) to clients at once and to each live node
// after its own random delay, as gossip would.
func (w *world) reconfigure(members []Member, retired map[Member]bool) {
	w.members = members
	for _, m := range members {
		if w.nodes[m] == nil {
			w.addNode(m, members)
		}
	}
	for _, name := range w.names() {
		n := w.nodes[name]
		w.at(w.t+int64(w.rng.IntN(2000)), func() {
			if n.up && !w.lost[name] && w.side[name] == 0 {
				n.rep.Reconfigure(w.local(n), members, retired)
			} else {
				w.pendingConfig(name, members, retired)
			}
		})
	}
}

// pendingConfig retries for a node that cannot hear gossip yet.
func (w *world) pendingConfig(name Member, members []Member, retired map[Member]bool) {
	w.at(w.t+step, func() {
		n := w.nodes[name]
		if w.lost[name] {
			return
		}
		if n.up && !n.paused && w.side[name] == 0 {
			n.rep.Reconfigure(w.local(n), members, retired)
			return
		}
		w.pendingConfig(name, members, retired)
	})
}

func (w *world) local(n *simNode) int64 { return n.offset + int64(float64(w.t)*n.rate) }

func (w *world) fail(format string, args ...any) {
	if len(w.failures) < 5 {
		w.failures = append(w.failures, fmt.Sprintf("t=%dms: ", w.t)+fmt.Sprintf(format, args...))
	}
}

func (w *world) available(m Member, side int) bool {
	n := w.nodes[m]
	return n.up && !n.paused && w.side[m] == side
}

func (w *world) crash(m Member) {
	n := w.nodes[m]
	n.lastMembers = n.rep.Members // the configuration is durable with the disk
	n.up, n.rep = false, nil      // the disk stays
}

func (w *world) restart(m Member) {
	n := w.nodes[m]
	members := n.lastMembers
	n.up = true
	n.rep = Open(m, n.inc, members, w.cfg, n.disk, w.local(n))
}

func (w *world) heal() {
	for _, m := range w.names() {
		n := w.nodes[m]
		if w.lost[m] {
			continue
		}
		w.side[m] = 0
		if !n.up {
			w.restart(m)
		}
		n.paused = false
	}
}

// loseDisk crashes a node for good.
func (w *world) loseDisk(m Member) {
	w.crash(m)
	w.lost[m] = true
}

func (w *world) sides() []int {
	var out []int
	for _, m := range w.names() {
		if w.lost[m] {
			continue
		}
		if !slices.Contains(out, w.side[m]) {
			out = append(out, w.side[m])
		}
	}
	slices.Sort(out)
	return out
}

// write sends one client write from a side: try R(z) in order, skip
// redirects. Returns the replica that accepted it, if any.
// candidates is the order a client tries: the newest R(z), then any other
// node, which may still believe an older configuration.
func (w *world) candidates() []Member {
	out := slices.Clone(w.members)
	for _, m := range w.names() {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

func (w *world) write(side int, rid, id string, deleted bool, value int64) Member {
	for _, m := range w.candidates() {
		if !w.available(m, side) {
			continue
		}
		err := w.nodes[m].rep.Write(w.local(w.nodes[m]), rid, id, deleted, value)
		if errors.Is(err, ErrRedirect) {
			continue
		}
		if err != nil && !errors.Is(err, ErrRequestConflict) {
			w.fail("write %s on %s: %v", rid, m, err)
		}
		return m
	}
	return ""
}

func (w *world) randomWrite(side int) {
	w.ridSeq++
	w.write(side, fmt.Sprintf("q%d", w.ridSeq), fmt.Sprintf("k%d", w.rng.IntN(w.ids)), w.rng.IntN(10) == 0, int64(w.rng.IntN(100)))
}

// read checks A4 on exact reads: they hold every write acknowledged before.
func (w *world) read(side int) {
	for _, m := range w.candidates() {
		if !w.available(m, side) {
			continue
		}
		s, exact, err := w.nodes[m].rep.Read(w.local(w.nodes[m]))
		if errors.Is(err, ErrRedirect) {
			continue
		}
		if !exact {
			w.degraded++
			return
		}
		w.exact++
		for _, a := range w.acks {
			if a.real < w.t && !w.exempt(a) && !w.exemptRead(a) && !s.Has(a.Result) {
				w.fail("A4: exact read on %s misses %s acked at %dms (mode %d)", m, a.RequestID, a.real, a.Mode)
				return
			}
		}
		return
	}
}

func (w *world) step() {
	w.t += step
	due := w.events
	w.events = nil
	for _, e := range due {
		if e.at <= w.t {
			e.do()
		} else {
			w.events = append(w.events, e)
		}
	}
	// Deliver.
	var later []flight
	for _, f := range w.net {
		if f.at > w.t {
			later = append(later, f)
			continue
		}
		to := w.nodes[f.m.To]
		switch {
		case !to.up || w.side[f.m.From] != w.side[f.m.To]:
			// lost
		case to.paused:
			later = append(later, flight{w.t + step, f.m})
		default:
			to.rep.Receive(w.local(to), f.m)
		}
	}
	w.net = later
	// Timers.
	for _, m := range w.names() {
		if n := w.nodes[m]; n.up && !n.paused {
			n.rep.Tick(w.local(n))
		}
	}
	// Clients.
	for _, side := range w.sides() {
		if w.writes && w.rng.Float64() < 0.3 {
			w.randomWrite(side)
		}
		if w.rng.Float64() < 0.1 {
			w.read(side)
		}
	}
	// Send.
	for _, m := range w.names() {
		n := w.nodes[m]
		if !n.up {
			continue
		}
		msgs, acks := n.rep.TakeOutbox()
		for _, a := range acks {
			w.acks = append(w.acks, clientAck{a, w.t, m})
		}
		for _, msg := range msgs {
			if w.rng.Float64() < w.drop {
				continue
			}
			copies := 1
			if w.rng.Float64() < w.dup {
				copies = 2
			}
			for range copies {
				w.net = append(w.net, flight{w.t + 1 + int64(w.rng.IntN(int(w.delay))), msg})
			}
		}
	}
}

// names lists every node in a fixed order, for determinism.
func (w *world) names() []Member {
	out := make([]Member, 0, len(w.nodes))
	for m := range w.nodes {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

func (w *world) runUntil(t int64) {
	for w.t < t {
		w.step()
	}
}

// settle heals everything, stops writes and lets repair finish, then checks
// A1 (no acknowledged write lost), A2 (identical replicas) and A3 (exact
// aggregates), and that the zone is clean again.
func (w *world) settle() {
	w.heal()
	w.writes = false
	w.runUntil(w.t + 20000)
	owner := w.nodes[w.members[0]]
	for _, a := range w.acks {
		if !w.exempt(a) && !owner.disk.Has(a.Result) {
			w.fail("A1: acknowledged %s lost (mode %d, durability %d)", a.RequestID, a.Mode, a.Durability)
			break
		}
	}
	for _, m := range w.members[1:] {
		if !w.nodes[m].disk.SameData(owner.disk) {
			w.fail("A2: %s differs from the owner", m)
		}
	}
	for _, m := range w.members {
		if !w.nodes[m].rep.Member() {
			w.fail("%s did not apply the last configuration", m)
		}
		d := w.nodes[m].disk
		if live, sum := d.Recount(); live != d.Live || sum != d.Sum {
			w.fail("A3: %s aggregates %d/%d, records give %d/%d", m, d.Live, d.Sum, live, sum)
		}
	}
	// Liveness: the zone becomes clean again. One lost reply can make it
	// briefly unclean at any instant, so wait for it rather than sample.
	for end := w.t + 10000; !owner.rep.Clean(w.local(owner)) && w.t < end; {
		w.step()
	}
	if !owner.rep.Clean(w.local(owner)) {
		w.fail("owner not clean 10s after repair")
	}
	for m, n := range w.nodes {
		if n.up && !n.rep.Member() && !n.rep.HandedOver() {
			w.fail("former member %s did not hand its data over", m)
		}
	}
	for _, m := range w.members {
		for _, v := range w.nodes[m].disk.Records {
			if v.Deleted {
				w.fail("A2: deleted %s on %s not pruned after repair", v.ID, m)
				return
			}
		}
	}
}

func (w *world) fallbackEntries() int {
	n := 0
	for _, m := range w.members[1:] {
		if r := w.nodes[m].rep; r != nil {
			n += r.FallbackEntries
		}
	}
	return n
}

func (w *world) acksIn(mode Mode, from, to int64) int {
	n := 0
	for _, a := range w.acks {
		if a.Mode == mode && a.real >= from && a.real < to {
			n++
		}
	}
	return n
}

type scenario struct {
	name string
	run  func(w *world)
}

var scenarios = []scenario{
	{"S1 no fault", func(w *world) {
		w.runUntil(30000)
		w.settle()
		if n := w.fallbackEntries(); n != 0 {
			w.fail("S1: %d fallback entries without fault", n)
		}
		if o := w.nodes["o"].disk.Overwrites; o != 0 {
			w.fail("S1: %d overwrites with one writer", o)
		}
	}},
	{"S2 owner crash 30s", func(w *world) {
		w.runUntil(5000)
		w.crash("o")
		w.runUntil(35000)
		if w.acksIn(Fallback, 5000, 35000) == 0 {
			w.fail("S2: no write accepted while the owner was down")
		}
		w.restart("o")
		w.runUntil(45000)
		w.settle()
	}},
	{"S3 owner isolated", func(w *world) {
		w.runUntil(5000)
		w.side["o"] = 1
		w.runUntil(20000)
		// S12: one request ID, two commands, one on each side.
		w.write(1, "dup", "k-dup-a", false, 1)
		w.write(0, "dup", "k-dup-b", false, 2)
		w.runUntil(35000)
		if w.acksIn(Fallback, 5000, 35000) == 0 || w.acksIn(Healthy, 5000, 35000) == 0 {
			w.fail("S3: both sides should accept writes")
		}
		w.settle()
		if w.nodes["o"].disk.RequestConflicts == 0 {
			w.fail("S12: request conflict not counted")
		}
	}},
	{"S4 three-way split", func(w *world) {
		w.runUntil(5000)
		w.side["r1"], w.side["r2"] = 1, 2
		w.runUntil(35000)
		w.settle()
	}},
	{"S5 owner paused 10s", func(w *world) {
		w.runUntil(5000)
		w.nodes["o"].paused = true
		w.runUntil(15000)
		w.nodes["o"].paused = false
		w.runUntil(25000)
		w.settle()
	}},
	{"S6 replica crash during repair", func(w *world) {
		w.runUntil(5000)
		w.side["o"] = 1
		w.runUntil(25000)
		w.side["o"] = 0
		w.runUntil(25000 + int64(w.rng.IntN(1500)))
		w.crash("r1")
		w.runUntil(w.t + 3000)
		w.restart("r1")
		w.runUntil(w.t + 5000)
		w.settle()
	}},
	{"S8 owner disk lost, retired", func(w *world) {
		w.runUntil(10000)
		lostAt := w.t
		w.loseDisk("o")
		// Writes acknowledged only by the lost disk may be gone.
		w.exempt = func(a clientAck) bool { return a.by == "o" && a.Durability == Local }
		w.runUntil(15000)
		w.reconfigure([]Member{"r1", "r2", "r3"}, map[Member]bool{"o": true})
		w.runUntil(20000)
		// The operator confirms the disk is destroyed.
		for _, m := range w.names() {
			if n := w.nodes[m]; n.up {
				n.rep.Forget("o")
			}
		}
		w.runUntil(30000)
		if w.acksIn(Healthy, lostAt, 30000) == 0 {
			w.fail("S8: the next XOR vnode never served as owner")
		}
		w.settle()
	}},
	{"S9 retired owner returns", func(w *world) {
		w.runUntil(5000)
		w.side["o"] = 1
		w.exemptRead = func(a clientAck) bool { return a.by == "o" && a.real >= 5000 }
		w.runUntil(12000)
		retiredAt := w.t
		// An operator error: o is alive behind a partition. Its own
		// acknowledgements after retirement cannot be in exact reads of the
		// new owner, but must still be merged (A1).
		w.reconfigure([]Member{"r1", "r2", "r3"}, map[Member]bool{"o": true})
		w.runUntil(30000)
		w.settle()
		for _, a := range w.acks {
			if a.by == "o" && a.real > retiredAt && !w.nodes["r1"].disk.Has(a.Result) {
				w.fail("S9: write acknowledged by the retired owner not merged")
				break
			}
		}
		if _, _, err := w.nodes["o"].rep.Read(w.local(w.nodes["o"])); err == nil {
			w.fail("S9: retired owner still serves reads")
		}
	}},
	{"S10 join during fallback", func(w *world) {
		w.runUntil(5000)
		w.side["r2"] = 1
		w.runUntil(12000)
		// r2 leaves R(z) while it holds fallback writes; r3 joins empty.
		w.reconfigure([]Member{"o", "r1", "r3"}, nil)
		w.runUntil(35000)
		w.settle()
	}},
	{"S11 transfer only when clean", func(w *world) {
		w.runUntil(5000)
		w.side["o"] = 1
		w.runUntil(20000)
		o := w.nodes["o"]
		if o.rep.CanTransfer(w.local(o)) {
			w.fail("S11: transfer allowed while the owner is cut off from its replicas")
		}
		w.settle()
		if !o.rep.CanTransfer(w.local(o)) {
			w.fail("S11: transfer refused after repair")
		}
	}},
	{"S7 lossy asymmetric links", func(w *world) {
		w.drop, w.delay = 0.3, 400
		w.runUntil(40000)
		w.drop, w.delay = 0.02, 40
		w.settle()
	}},
}

func runScenario(sc scenario, seed uint64) *world {
	w := newWorld(seed)
	sc.run(w)
	if w.t < 1000 { // a scenario that stopped early has already failed
		return w
	}
	if w.exact == 0 {
		w.fail("no exact read was checked")
	}
	return w
}

// seeds is 4 by default, 20 with INDEXUS_LONG=1; CONVERGENT_SEEDS widens
// the sweep. Each seed of each scenario is a whole simulated run.
var seeds = func() uint64 {
	if n, err := strconv.Atoi(os.Getenv("CONVERGENT_SEEDS")); err == nil && n > 0 {
		return uint64(n)
	}
	if os.Getenv("INDEXUS_LONG") == "1" {
		return 20
	}
	return 4
}()

func TestScenarios(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(strings.Fields(sc.name)[0], func(t *testing.T) {
			t.Parallel()
			acks, exact, degraded := 0, 0, 0
			for seed := range seeds {
				w := runScenario(sc, seed)
				if len(w.failures) > 0 {
					t.Fatalf("%s, seed %d:\n  %s", sc.name, seed, strings.Join(w.failures, "\n  "))
				}
				acks, exact, degraded = acks+len(w.acks), exact+w.exact, degraded+w.degraded
			}
			t.Logf("%s: %d seeds, %d acks, %d exact reads checked, %d degraded reads", sc.name, seeds, acks, exact, degraded)
		})
	}
}

// Each defect must make at least one scenario fail.
func TestScenariosDetectDefects(t *testing.T) {
	for _, c := range []struct {
		name  string
		fault Fault
	}{
		{"clean without a fresh reply", CleanWithoutFresh},
		{"max-counter vector", VectorTakesMax},
		{"no digest rule", NoDigestRule},
		{"prune after one replica", PruneWithoutAll},
		{"clean while a leaver's data is awaited", CleanIgnoresLeavers},
	} {
		t.Run(c.name, func(t *testing.T) {
			withFault(t, c.fault, func() {
				for _, sc := range scenarios {
					for seed := range seeds {
						if w := runScenario(sc, seed); len(w.failures) > 0 {
							t.Logf("detected by %s, seed %d: %s", sc.name, seed, w.failures[0])
							return
						}
					}
				}
				t.Fatal("no scenario detected the defect")
			})
		})
	}
}
