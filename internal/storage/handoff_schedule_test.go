package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// Transaction-boundary scheduler over REAL Store methods, not a second handoff
// implementation. One seeded RNG selects deliveries, lost replies, power cuts
// and identities. Background collection is quiesced after each delivered call.
// This does not explore the Go scheduler or model CPU/physical disk latency.
type handoffDisk struct{ *testkit.PowerFS }

func (d handoffDisk) Create(n string) (zonelog.File, error) { return d.PowerFS.Create(n) }
func (d handoffDisk) Open(n string) (zonelog.File, error)   { return d.PowerFS.Open(n) }

type scheduledNode struct {
	fs     *testkit.PowerFS
	store  *Store
	holder causal.Endpoint
	path   string
}
type scheduledMove struct {
	id                    causal.TransferID
	cell, from, to, phase int
	token                 causal.Digest
	offer                 CausalTransfer
	keys                  []tree.ZoneKey
	snaps                 []Snapshot
	proofs                []causal.ZoneTransfer
	cert                  causal.Cession
}
type scheduleStats struct {
	Steps, Writes, EarlyWrites, Transfers, Drops, LostReplies, Crashes, IOCuts, Restarts, Replays, Compactions int
}
type handoffSchedule struct {
	t                     testing.TB
	rng                   *rand.Rand
	seed                  int64
	nodes                 []scheduledNode
	zones                 []tree.ZoneKey
	owners, acked, issued []int
	moves                 []*scheduledMove
	finished              []*scheduledMove
	blocked               [][]bool
	root                  tree.VNodeDescriptor
	genesis               causal.Genesis
	serial                int
	stats                 scheduleStats
	trace                 []string
}

func newHandoffSchedule(t testing.TB, seed int64, count int) *handoffSchedule {
	t.Helper()
	s := &handoffSchedule{t: t, rng: rand.New(rand.NewSource(seed)), seed: seed,
		nodes: make([]scheduledNode, count), zones: make([]tree.ZoneKey, count), owners: make([]int, count), acked: make([]int, count), issued: make([]int, count), blocked: make([][]bool, count)}
	base := t.TempDir()
	t.Cleanup(func() {
		for i := range s.nodes {
			if s.nodes[i].store != nil {
				_ = s.nodes[i].store.Close()
			}
		}
	})
	for i := range s.nodes {
		s.nodes[i] = scheduledNode{fs: testkit.NewPowerFS(), path: filepath.Join(base, fmt.Sprintf("node-%02d", i))}
		s.blocked[i] = make([]bool, count)
		s.open(i)
	}
	s.root = tree.VNodeDescriptor{Name: s.nodes[0].holder.VNode, ID: tree.ID{1}}
	s.genesis = causal.Genesis{Cluster: "schedule", Holder: s.nodes[0].holder}
	for i := range s.nodes {
		s.must(s.nodes[i].store.InitCausal(s.root, s.genesis))
	}
	for _, location := range tree.ChildCandidates(tree.BASE64, "@") {
		z, err := tree.NewZoneKey("0000000000000001", location)
		s.must(err)
		cell := int(z.ID[0]) / (256 / count)
		if s.zones[cell].Collection == "" {
			s.zones[cell] = z
		}
	}
	for i, z := range s.zones {
		if z.Collection == "" {
			t.Fatalf("no zone in cell %d", i)
		}
	}
	// Bootstrap actual local rights with pairwise tickets, without gossip or
	// global proof state. Empty address space moves through the same pipeline.
	for i := 1; i < count; i++ {
		s.bootstrapTransfer(i)
		s.owners[i] = i
	}
	for cell := range s.zones {
		s.write(cell)
	}
	return s
}
func (s *handoffSchedule) region(cell int) tree.Region {
	return tree.Region{Mask: tree.ID{byte(256 - 256/len(s.nodes))}, Value: tree.ID{byte(cell * (256 / len(s.nodes)))}}
}
func (s *handoffSchedule) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatalf("seed=%d step=%d: %v\n%v", s.seed, s.stats.Steps, err, s.trace)
	}
}
func (s *handoffSchedule) log(format string, args ...any) {
	s.trace = append(s.trace, fmt.Sprintf("%05d ", s.stats.Steps)+fmt.Sprintf(format, args...))
}
func (s *handoffSchedule) open(i int) {
	n := &s.nodes[i]
	store, err := OpenWithOptions(n.path, fmt.Sprintf("n%02d", i), "schedule", 1<<30, Options{FS: handoffDisk{n.fs}, Entropy: s.rng, MemoryBytes: 1 << 30})
	s.must(err)
	n.store = store
	h, err := store.Endpoint(fmt.Sprintf("host-%02d", i))
	s.must(err)
	if n.holder.Valid() && n.holder != h {
		s.t.Fatal("recovery changed the durable disk incarnation")
	}
	n.holder = h
}
func (s *handoffSchedule) crash(i int) {
	n := &s.nodes[i]
	if n.store == nil {
		return
	}
	after := n.fs.Crash(s.rng, true)
	// Close must not add a flush to the already captured power-loss image.
	n.fs.FailAt = n.fs.Ops + 1
	_ = n.store.Close()
	n.store = nil
	n.fs = after
	s.stats.Crashes++
	s.log("crash node=%d", i)
}
func (s *handoffSchedule) write(cell int) {
	i := s.owners[cell]
	if s.nodes[i].store == nil {
		return
	}
	s.issued[cell]++
	v := s.issued[cell]
	r := Record{Collection: s.zones[cell].Collection, Location: s.zones[cell].Location, ID: "register", Metrics: []float64{float64(v)}}
	_, err := s.nodes[i].store.Write(fmt.Sprintf("write:%d:%d", cell, v), r)
	if err == nil {
		s.acked[cell] = v
		s.stats.Writes++
		for _, p := range s.moves {
			if p.cell == cell && p.to == i && p.phase >= 8 && p.phase < 15 {
				s.stats.EarlyWrites++
				break
			}
		}
		s.log("ACK cell=%d node=%d value=%d", cell, i, v)
		return
	}
	if !errors.Is(err, ErrFrozen) && !errors.Is(err, ErrNotOwner) && !errors.Is(err, ErrNotReady) {
		s.must(err)
	}
}
func (s *handoffSchedule) newMove() {
	cell := s.rng.Intn(len(s.nodes))
	for _, p := range s.moves {
		if p.cell == cell {
			return
		}
	}
	from := s.owners[cell]
	to := s.rng.Intn(len(s.nodes))
	if from == to || s.nodes[from].store == nil || s.nodes[to].store == nil {
		return
	}
	s.serial++
	p := &scheduledMove{cell: cell, from: from, to: to}
	s.moves = append(s.moves, p)
	s.log("begin op=%s cell=%d %d->%d", p.id, cell, from, to)
}
func wireClone[T any](value T) T {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var clone T
	if err = json.Unmarshal(raw, &clone); err != nil {
		panic(err)
	}
	return clone
}

// One action is one actual API call/transaction boundary. Drops happen before
// dispatch; lost replies happen AFTER the real callee has returned durably.
func (s *handoffSchedule) deliver(p *scheduledMove, faults bool) {
	if s.nodes[p.from].store == nil || s.nodes[p.to].store == nil {
		return
	}
	remote := p.phase == 1 || p.phase == 3 || p.phase == 6 || p.phase == 8 || p.phase == 12 || p.phase == 14
	if remote && s.blocked[p.from][p.to] {
		s.stats.Drops++
		s.log("partition op=%s phase=%d", p.id, p.phase)
		return
	}
	if remote && faults && s.rng.Intn(12) == 0 {
		s.stats.Drops++
		s.log("drop op=%s phase=%d", p.id, p.phase)
		return
	}
	i := p.from
	if remote {
		i = p.to
	}
	n := &s.nodes[i]
	if faults && s.rng.Intn(13) == 0 {
		n.fs.FailAt = n.fs.Ops + 1 + s.rng.Intn(6)
	}
	src, dst := s.nodes[p.from].store, s.nodes[p.to].store
	var err error
	switch p.phase {
	case 0:
		if p.token == (causal.Digest{}) {
			state, e := src.CausalState()
			err = e
			if e == nil {
				right, ok := state.Permissions.At(s.zones[p.cell].ID)
				if !ok {
					err = ErrNotOwner
				} else {
					p.token = right.ID
				}
			}
		}
		if err == nil {
			p.offer, err = src.BeginCausal(context.Background(), p.token, s.nodes[p.to].holder, s.region(p.cell))
			if err == nil {
				p.id = p.offer.Cession.ID
			}
		}
	case 1:
		var c causal.Cession
		c, err = dst.ReserveCausal(p.offer.Cession)
		if err == nil {
			p.offer.Cession = c
		}
	case 2:
		err = src.BindCausal(p.id, p.offer.Cession.Ticket)
	case 4:
		err = src.ConfirmCausal(p.id)
	case 3:
		var states []ZoneState
		states, err = src.TransferZones(p.id)
		if err == nil {
			p.keys = nil
			p.offer.Snapshots = nil
			for _, st := range states {
				p.keys = append(p.keys, st.Zone)
				var snap Snapshot
				snap, err = src.Snapshot(st.Zone)
				if err != nil {
					break
				}
				p.offer.Snapshots = append(p.offer.Snapshots, snap)
			}
		}
		if err == nil {
			_, err = dst.PreloadCausal(wireClone(p.offer))
		}
	case 5:
		p.snaps, err = src.FreezeZones(p.id, p.keys)
	case 6:
		err = dst.StageZones(p.id, s.nodes[p.from].holder.VNode, wireClone(p.snaps))
	case 7:
		p.proofs, err = src.EmitZoneTransfers(p.id, p.keys)
	case 8:
		err = dst.InstallZones(p.id, wireClone(p.proofs))
		if err == nil && len(p.keys) > 0 {
			s.owners[p.cell] = p.to
		}
	case 9:
		err = src.AckZoneInstalls(p.id, p.keys)
	case 10:
		_, err = src.FreezeRegion(p.id)
	case 11:
		p.offer, err = src.SealCausalRegion(p.id)
	case 12:
		err = dst.StageCausal(wireClone(p.offer))
	case 13:
		p.cert, err = src.EmitCausal(p.id)
	case 14:
		err = dst.InstallCausal(wireClone(p.cert))
		if err == nil {
			s.owners[p.cell] = p.to
		}
	case 15:
		err = src.CompleteCausal(p.id)
	case 16:
		err = src.CollectTransferred(p.id)
		if err == nil {
			err = dst.CollectTransferred(p.id)
		}
	}
	cut := n.fs.FailAt > 0 && n.fs.Ops >= n.fs.FailAt
	n.fs.FailAt = 0
	s.log("deliver op=%s phase=%d error=%v cut=%t", p.id, p.phase, err, cut)
	if cut {
		s.stats.IOCuts++
		s.crash(i)
		return
	}
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, causal.ErrConflict) || errors.Is(err, ErrNotReady) || errors.Is(err, causal.ErrDependency) {
			// Another operation may consume a parent before this reservation.
			if p.phase == 0 {
				p.token = causal.Digest{}
			}
			return
		}
		s.must(err)
	}
	if remote && (s.blocked[p.to][p.from] || faults && s.rng.Intn(7) == 0) {
		s.stats.LostReplies++
		s.log("lost-reply op=%s phase=%d", p.id, p.phase)
		return
	}
	p.phase++
	if p.phase == 17 {
		copy := *p
		s.finished = append(s.finished, &copy)
		s.stats.Transfers++
	}
}

func (s *handoffSchedule) replay() {
	if len(s.finished) == 0 {
		return
	}
	p := s.finished[s.rng.Intn(len(s.finished))]
	node := s.nodes[p.to].store
	if node == nil {
		return
	}
	var err error
	switch s.rng.Intn(3) {
	case 0:
		err = node.InstallCausal(wireClone(p.cert))
	case 1:
		err = node.StageZones(p.id, s.nodes[p.from].holder.VNode, wireClone(p.snaps))
	case 2:
		err = node.InstallZones(p.id, wireClone(p.proofs))
	}
	if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, causal.ErrConflict) && !errors.Is(err, ErrNotReady) && !errors.Is(err, causal.ErrDependency) {
		s.must(err)
	}
	s.stats.Replays++
	s.log("replay op=%s error=%v", p.id, err)
}
func (s *handoffSchedule) check(requireAvailable bool) {
	s.t.Helper()
	for cell, zone := range s.zones {
		writers := 0
		for i, n := range s.nodes {
			if n.store == nil {
				continue
			}
			err := n.store.view(func(tx *Tx) error { return n.store.causalWritable(tx, zone) })
			if err != nil {
				if !errors.Is(err, ErrNotOwner) && !errors.Is(err, ErrNotReady) && !errors.Is(err, ErrFrozen) {
					s.must(err)
				}
				continue
			}
			writers++
			if writers > 1 {
				s.t.Fatalf("seed=%d step=%d cell=%d: two writers\n%v", s.seed, s.stats.Steps, cell, s.trace)
			}
			if s.acked[cell] > 0 {
				snap, e := n.store.Snapshot(zone)
				s.must(e)
				if len(snap.Records) != 1 || len(snap.Records[0].Metrics) != 1 || snap.Records[0].Metrics[0] < float64(s.acked[cell]) || snap.Records[0].Metrics[0] > float64(s.issued[cell]) {
					s.t.Fatalf("seed=%d step=%d cell=%d writer=%d: earlier ACK %d missing: %+v\n%v", s.seed, s.stats.Steps, cell, i, s.acked[cell], snap.Records, s.trace)
				}
			}
		}
		if requireAvailable && writers != 1 {
			s.t.Fatalf("seed=%d step=%d cell=%d: %d writers after recovery", s.seed, s.stats.Steps, cell, writers)
		}
	}
}
func (s *handoffSchedule) tick(faults bool) {
	s.tickChoice(uint8(s.rng.Intn(10)), faults)
}
func (s *handoffSchedule) tickChoice(choice uint8, faults bool) {
	s.stats.Steps++
	switch choice % 10 {
	case 0:
		s.newMove()
	case 1, 2, 3, 4:
		if len(s.moves) > 0 {
			s.deliver(s.moves[s.rng.Intn(len(s.moves))], faults)
		}
	case 5:
		s.write(s.rng.Intn(len(s.zones)))
	case 6:
		i := s.rng.Intn(len(s.nodes))
		if s.nodes[i].store == nil {
			s.open(i)
			s.stats.Restarts++
		} else if faults {
			s.crash(i)
		}
	case 7:
		s.replay()
	case 8:
		i := s.rng.Intn(len(s.nodes))
		if s.nodes[i].store != nil {
			err := s.compact(i)
			if err == nil {
				s.stats.Compactions++
			} else if !errors.Is(err, ErrNotReady) && !errors.Is(err, ErrReadChanged) && !errors.Is(err, causal.ErrDependency) {
				s.must(err)
			}
			s.log("compact node=%d error=%v", i, err)
		}
	case 9:
		if faults {
			i, j := s.rng.Intn(len(s.nodes)), s.rng.Intn(len(s.nodes))
			s.blocked[i][j] = !s.blocked[i][j]
			s.log("link %d->%d blocked=%t", i, j, s.blocked[i][j])
		}
	}
	s.removeFinished()
	s.check(false)
}
func (s *handoffSchedule) removeFinished() {
	kept := s.moves[:0]
	for _, p := range s.moves {
		if p.phase < 17 {
			kept = append(kept, p)
		}
	}
	s.moves = kept
}
func (s *handoffSchedule) drain() {
	for i := range s.nodes {
		if s.nodes[i].store == nil {
			s.open(i)
			s.stats.Restarts++
		}
		clear(s.blocked[i])
	}
	s.check(false) // Check old ACKs BEFORE issuing any replacement writes.
	for round, bound := 0, 20*(len(s.moves)+1); len(s.moves) > 0 && round < bound; round++ {
		for _, p := range s.moves {
			s.stats.Steps++
			s.deliver(p, false)
			s.check(false)
		}
		s.removeFinished()
	}
	if len(s.moves) > 0 {
		s.t.Fatalf("seed=%d: did not close %d operations after faults stopped\n%v", s.seed, len(s.moves), s.trace)
	}
	s.check(true)
	for i, n := range s.nodes {
		frozen, err := n.store.FrozenOutgoing()
		s.must(err)
		if len(frozen) != 0 {
			s.t.Fatalf("node=%d retains frozen operations after recovery", i)
		}
		s.must(s.compact(i))
	}
	s.check(true)
	s.checkReadDelegations()
	for cell := range s.zones {
		s.write(cell)
	}
	s.check(true)
}

func (s *handoffSchedule) digest() string {
	raw, _ := json.Marshal(s.trace)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *handoffSchedule) report() {
	s.t.Helper()
	s.t.Logf("seed=%d transcript=%s stats=%+v", s.seed, s.digest(), s.stats)
	if path := os.Getenv("INDEXUS_HANDOFF_TRACE"); path != "" {
		raw, err := json.MarshalIndent(struct {
			Seed   int64
			Nodes  int
			Digest string
			Stats  scheduleStats
			Trace  []string
		}{s.seed, len(s.nodes), s.digest(), s.stats, s.trace}, "", "  ")
		s.must(err)
		s.must(os.WriteFile(path, append(raw, '\n'), 0600))
	}
}

func TestHandoffScheduleReplaysSeed(t *testing.T) {
	seed := int64(25)
	if raw := os.Getenv("INDEXUS_HANDOFF_SEED"); raw != "" {
		var err error
		seed, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	var want string
	for repeat := range 2 {
		s := newHandoffSchedule(t, seed, 4)
		for range 500 {
			s.tick(true)
		}
		s.drain()
		digest := s.digest()
		if repeat == 0 {
			want = digest
		} else if digest != want {
			t.Fatalf("same seed, different transcript: %s != %s", digest, want)
		}
		s.report()
	}
}

func TestHandoffSchedule64(t *testing.T) {
	if os.Getenv("INDEXUS_HANDOFF_64") != "1" {
		t.Skip("opt-in 64-disk fault schedule: INDEXUS_HANDOFF_64=1")
	}
	s := newHandoffSchedule(t, 64, 64)
	for range 3000 {
		s.tick(true)
	}
	s.drain()
	if s.stats.EarlyWrites == 0 {
		t.Fatal("receiver never wrote before regional completion")
	}
	s.report()
}

func TestHandoffScheduleSeeds(t *testing.T) {
	seeds := 32
	if raw := os.Getenv("INDEXUS_HANDOFF_SEEDS"); raw != "" {
		var err error
		seeds, err = strconv.Atoi(raw)
		if err != nil || seeds < 1 {
			t.Fatal("INDEXUS_HANDOFF_SEEDS must be positive")
		}
	}
	if testing.Short() {
		seeds = 4
	}
	var coverage scheduleStats
	for seed := int64(0); seed < int64(seeds); seed++ {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			s := newHandoffSchedule(t, seed, 4)
			for range 500 {
				s.tick(true)
			}
			s.drain()
			coverage.IOCuts += s.stats.IOCuts
			coverage.LostReplies += s.stats.LostReplies
			coverage.Transfers += s.stats.Transfers
			coverage.Replays += s.stats.Replays
			coverage.EarlyWrites += s.stats.EarlyWrites
		})
	}
	t.Logf("coverage across %d seeds: %+v", seeds, coverage)
	if !testing.Short() && (coverage.IOCuts == 0 || coverage.LostReplies == 0 || coverage.Transfers == 0 || coverage.Replays == 0 || coverage.EarlyWrites == 0) {
		t.Fatal("vacuous campaign fault coverage")
	}
}

func FuzzHandoffSchedule(f *testing.F) {
	f.Add(uint64(25), []byte{0, 1, 2, 5, 3, 7, 4, 6, 8, 9, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add(uint64(0), []byte{0, 0, 0, 0, 1, 2, 3, 4, 1, 2, 3, 4, 6, 7, 8})
	f.Fuzz(func(t *testing.T, seed uint64, choices []byte) {
		if len(choices) > 256 {
			choices = choices[:256]
		}
		s := newHandoffSchedule(t, int64(seed), 4)
		for _, choice := range choices {
			s.tickChoice(choice, true)
		}
		s.drain()
	})
}

func (s *handoffSchedule) bootstrapTransfer(target int) {
	src, dst := s.nodes[0].store, s.nodes[target].store
	state, e := src.CausalState()
	s.must(e)
	r, ok := state.Permissions.At(s.region(target).Value)
	if !ok {
		s.t.Fatal("missing bootstrap right")
	}
	op, e := src.BeginCausal(context.Background(), r.ID, s.nodes[target].holder, s.region(target))
	s.must(e)
	c, e := dst.ReserveCausal(op.Cession)
	s.must(e)
	s.must(src.BindCausal(c.ID, c.Ticket))
	op.Cession = c
	_, e = dst.PreloadCausal(op)
	s.must(e)
	s.must(src.ConfirmCausal(c.ID))
	_, e = src.ForceFreezeRegion(c.ID)
	s.must(e)
	op, e = src.SealCausalRegion(c.ID)
	s.must(e)
	s.must(dst.StageCausal(op))
	c, e = src.EmitCausal(c.ID)
	s.must(e)
	s.must(dst.InstallCausal(c))
	s.must(src.CompleteCausal(c.ID))
	s.must(src.CollectTransferred(c.ID))
	s.must(dst.CollectTransferred(c.ID))
}
func (s *handoffSchedule) compact(i int) error {
	state, e := s.nodes[i].store.CausalState()
	if e != nil {
		return e
	}
	for id, t := range state.Transfers {
		if t.Phase == causal.TransferInstalled {
			if e = s.nodes[i].store.CollectTransferred(id); e != nil {
				return e
			}
		}
	}
	return nil
}

// Start from the original root contact with no global ownership view. Current
// scoped successors must still reach the actual installed writer after receipt
// collection and physical returns. Empty/incomplete coverage is never zero.
func (s *handoffSchedule) checkReadDelegations() {
	byDisk := map[string]int{}
	for i, n := range s.nodes {
		byDisk[n.holder.Storage] = i
	}
	for cell, z := range s.zones {
		queue := []int{0}
		seen := map[int]bool{}
		reached := false
		for len(queue) > 0 {
			i := queue[0]
			queue = queue[1:]
			if seen[i] {
				continue
			}
			seen[i] = true
			n := s.nodes[i]
			s.must(n.store.view(func(tx *Tx) error {
				c, e := n.store.causalSnapshot(tx)
				if e != nil {
					return e
				}
				if e = n.store.writableIn(tx, c, z); e == nil {
					reached = true
				}
				refs, e := inventoryReferrals(tx, c, tree.Point(z.ID), nil)
				if e != nil {
					return e
				}
				for _, ref := range refs {
					j, ok := byDisk[ref.Endpoint.Storage]
					if !ok {
						s.t.Fatal("successor points to absent disk")
					}
					queue = append(queue, j)
				}
				return nil
			}))
		}
		if !reached {
			s.t.Fatalf("cell=%d: compacted successor directory did not reach installed writer", cell)
		}
	}
}

func TestReceiverCancellationCrashCutsRetainCleanup(t *testing.T) {
	cuts := 0
	for cut := 1; cut <= 32; cut++ {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			s := newHandoffSchedule(t, int64(900+cut), 2)
			src, dst := s.nodes[0].store, s.nodes[1].store
			state, e := src.CausalState()
			s.must(e)
			right, ok := state.Permissions.At(s.zones[0].ID)
			if !ok {
				t.Fatal("missing source input")
			}
			op, e := src.BeginCausal(context.Background(), right.ID, s.nodes[1].holder, s.region(0))
			s.must(e)
			c, e := dst.ReserveCausal(op.Cession)
			s.must(e)
			s.must(src.BindCausal(c.ID, c.Ticket))
			op.Cession = c
			snap, e := src.Snapshot(s.zones[0])
			s.must(e)
			op.Snapshots = []Snapshot{snap}
			_, e = dst.PreloadCausal(op)
			s.must(e)
			s.must(src.ConfirmCausal(c.ID))
			s.must(src.AbortCausal(c.ID))
			n := &s.nodes[1]
			n.fs.FailAt = n.fs.Ops + cut
			if e = dst.CancelCausal(c); e != nil {
				cuts++
			}
			s.crash(1)
			s.open(1)
			recovered := s.nodes[1].store
			recoveredState, recoveryErr := recovered.CausalState()
			s.must(recoveryErr)
			if marker, ok := recoveredState.Transfers[c.ID]; ok && marker.Phase == causal.TransferCancelled {
				before := recovered.LocalTransferRevision()
				s.must(recovered.StageZones(c.ID, src.node, []Snapshot{snap}))
				seal, sealErr := snap.Seal()
				s.must(sealErr)
				s.must(recovered.CertifyZones(c.ID, src.node, []ZoneSeal{seal}))
				if recovered.LocalTransferRevision() != before {
					t.Fatal("closed cancellation marker accepted a staging mutation")
				}
			}
			s.must(s.nodes[1].store.CancelCausal(c))
			s.must(src.FinishAbortCausal(c.ID))
			for _, node := range s.nodes {
				s.must(node.store.view(func(tx *Tx) error {
					current, e := readCausal(tx)
					if e != nil {
						return e
					}
					if _, ok := current.Transfers[c.ID]; ok {
						t.Fatal("cancel cleanup marker survived successful retry")
					}
					if tx.any(stagingRows(c.ID.String())) || tx.any(zoneStatesRows(c.ID)) {
						t.Fatal("cancel payload/discovery garbage survived crash recovery")
					}
					return nil
				}))
			}
			s.check(true)
		})
	}
	if cuts == 0 {
		t.Fatal("no cancellation I/O cuts exercised")
	}
}
