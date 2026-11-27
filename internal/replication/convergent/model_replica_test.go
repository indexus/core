package convergent

import "errors"

// Member names a replica of a zone: a vnode, or its machine.
type Member string

type Kind uint8

const (
	Heartbeat      Kind = iota // owner -> replica: Sent, Delta.Vector, Prune
	HeartbeatReply             // replica -> owner: Sent echoed, Delta of what the owner misses
	Push                       // owner -> replica: Delta
	PushAck                    // replica -> owner: Delta.Vector
	Handover                   // former member -> owner: everything it holds
	HandoverAck                // owner -> former member: Delta.Vector
)

type Message struct {
	Kind     Kind
	From, To Member
	Sent     int64 // owner's local time of the heartbeat
	Delta    Delta
	Prune    *Vector // deleted versions this vector covers may be dropped
}

type Durability uint8

const (
	Replicated Durability = iota // on the owner and at least one replica
	Local                        // on one store only
)

type Mode uint8

const (
	Healthy Mode = iota
	Fallback
)

// Ack is the answer to a client write.
type Ack struct {
	RequestID  string
	Result     Version
	Durability Durability
	Mode       Mode
	At         int64 // local time of the ACK
}

var (
	ErrRequestConflict = errors.New("convergent: request ID reused for another command")
)

type peer struct {
	vector Vector // merged from every report: a lower bound of its state
	fresh  int64  // Sent of the newest heartbeat it answered; -1 if none
}

type pending struct {
	rid    string
	result Version
	issued int64
}

// Replica is one member of R(z) for one zone, or a former member handing
// its data over. It is driven by a transport and a clock: every method takes
// the local time; messages accumulate in Outbox and ACKs in Acks.
type Replica struct {
	Self    Member
	Inc     Incarnation
	Members []Member // R(z); Members[0] is the owner
	Cfg     Config
	State   *State // durable

	heard    int64 // local time the owner was last heard
	fallback bool
	lastBeat int64
	peers    map[Member]*peer
	waiting  []pending
	leaving  map[Member]bool // former members whose data the owner still waits for
	handed   bool            // a former member whose data the owner confirmed
	pruned   *Vector         // last prune vector, sent with every push and heartbeat

	Outbox []Message
	Acks   []Ack
	// FallbackEntries counts entries into fallback, for tests.
	FallbackEntries int
}

// Open starts a replica on a durable state at local time now. After a crash,
// state is the disk content; everything else is rebuilt.
func Open(self Member, inc Incarnation, members []Member, cfg Config, state *State, now int64) *Replica {
	if state == nil {
		state = NewState()
	}
	r := &Replica{Self: self, Inc: inc, Cfg: cfg, State: state, heard: now, lastBeat: now - cfg.Beat}
	r.setMembers(members)
	return r
}

func (r *Replica) setMembers(members []Member) {
	r.Members = members
	r.peers = map[Member]*peer{}
	r.leaving = map[Member]bool{}
	for _, m := range members[1:] {
		r.peers[m] = &peer{vector: NewVector(), fresh: -1}
	}
}

func (r *Replica) IsOwner() bool    { return r.Self == r.Members[0] }
func (r *Replica) InFallback() bool { return r.fallback }

// Member reports whether this store is in R(z).
func (r *Replica) Member() bool {
	for _, m := range r.Members {
		if m == r.Self {
			return true
		}
	}
	return false
}

// HandedOver reports, for a former member, that the owner holds its data.
func (r *Replica) HandedOver() bool { return r.handed }

// Reconfigure applies a new R(z). Members that leave hand their data to the
// new owner; until the owner has it, its zone is not clean. A retired member
// lost its disk (section 7): nothing is awaited from it.
func (r *Replica) Reconfigure(now int64, members []Member, retired map[Member]bool) {
	old := r.Members
	for _, w := range r.waiting {
		r.ack(now, w.rid, w.result, Local)
	}
	r.waiting = nil
	for _, m := range old {
		if retired[m] && m != r.Self {
			r.State.AwaitRetired[m] = true
		}
	}
	r.setMembers(members)
	r.heard, r.fallback, r.lastBeat, r.handed = now, false, now-r.Cfg.Beat, false
	if !r.IsOwner() {
		return
	}
	for _, m := range old {
		if m != r.Self && !retired[m] && !contains(members, m) {
			r.leaving[m] = true
		}
	}
}

// Forget records an operator's confirmation that a retired member's disk
// is destroyed: nothing can come back from it, so pruning may resume.
func (r *Replica) Forget(m Member) { delete(r.State.AwaitRetired, m) }

func contains(ms []Member, m Member) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

func (r *Replica) send(m Message) {
	m.From = r.Self
	r.Outbox = append(r.Outbox, m)
}

// Write handles a client write. Healthy: only the owner accepts, and the ACK
// waits for a replica's copy or for Cfg.Ack. Fallback: a replica accepts and
// ACKs at once, with local durability.
func (r *Replica) Write(now int64, rid, id string, deleted bool, value int64) error {
	if !r.Member() || (!r.IsOwner() && !r.fallback) {
		return ErrRedirect
	}
	command := contentDigest(id, deleted, value)
	if prior, ok := r.State.Receipts[rid]; ok {
		if prior.Command != command {
			return ErrRequestConflict
		}
		r.ack(now, rid, prior.Result, Local)
		return nil
	}
	ver := r.State.Clock + 1
	dot := Dot{r.Inc, r.State.Vector.Base[r.Inc] + 1}
	v := NewVersion(id, ver, deleted, value, dot)
	r.State.Apply(v)
	r.State.ApplyReceipt(Receipt{RequestID: rid, Command: command, Result: v})
	if r.fallback {
		r.ack(now, rid, v, Local)
		return nil
	}
	r.waiting = append(r.waiting, pending{rid, v, now})
	for _, m := range r.Members[1:] {
		r.send(Message{Kind: Push, To: m, Delta: r.State.DeltaSince(r.peers[m].vector), Prune: r.pruned})
	}
	return nil
}

func (r *Replica) ack(now int64, rid string, v Version, d Durability) {
	mode := Healthy
	if r.fallback {
		mode = Fallback
	}
	r.Acks = append(r.Acks, Ack{RequestID: rid, Result: v, Durability: d, Mode: mode, At: now})
}

// Tick advances timers: heartbeats, late ACKs and pruning on the owner,
// suspicion on a replica, handover on a former member.
func (r *Replica) Tick(now int64) {
	switch {
	case !r.Member():
		if !r.handed && now-r.lastBeat >= r.Cfg.Beat {
			r.lastBeat = now
			r.send(Message{Kind: Handover, To: r.Members[0], Delta: r.State.DeltaSince(NewVector())})
		}
	case r.IsOwner():
		if now-r.lastBeat >= r.Cfg.Beat {
			r.lastBeat = now
			if prune := r.prunable(now); prune != nil {
				r.State.Prune(*prune)
				r.pruned = prune
			}
			for _, m := range r.Members[1:] {
				r.send(Message{Kind: Heartbeat, To: m, Sent: now, Delta: Delta{Vector: r.State.Vector.Clone()}, Prune: r.pruned})
			}
		}
		r.release(now)
	default:
		if !r.fallback && now-r.heard >= r.Cfg.Suspect {
			r.fallback = true
			r.FallbackEntries++
		}
	}
}

// prunable returns the vector under which deleted versions may be dropped
// everywhere: the zone is clean and every member holds all the owner holds.
// Then no version concurrent with a pruned delete exists that some member
// has not already seen, so none can arrive later and resurrect a record.
func (r *Replica) prunable(now int64) *Vector {
	if injected == PruneWithoutAll {
		for _, p := range r.peers {
			if r.State.Vector.LessOrEqual(p.vector) {
				v := r.State.Vector.Clone()
				return &v
			}
		}
		return nil
	}
	if !r.Clean(now) || len(r.State.AwaitRetired) > 0 {
		return nil
	}
	for _, p := range r.peers {
		if !r.State.Vector.LessOrEqual(p.vector) {
			return nil
		}
	}
	v := r.State.Vector.Clone()
	return &v
}

// release ACKs waiting writes that a replica holds, or that waited Cfg.Ack.
func (r *Replica) release(now int64) {
	kept := r.waiting[:0]
	for _, w := range r.waiting {
		copied := false
		for _, m := range r.Members[1:] {
			if r.peers[m].vector.Covers(w.result.Dot) {
				copied = true
			}
		}
		switch {
		case copied:
			r.ack(now, w.rid, w.result, Replicated)
		case now-w.issued >= r.Cfg.Ack:
			r.ack(now, w.rid, w.result, Local)
		default:
			kept = append(kept, w)
		}
	}
	r.waiting = kept
}

// Receive handles one message. Messages from a store that is not the owner
// this replica knows, or not a member the owner knows, are ignored.
func (r *Replica) Receive(now int64, m Message) {
	switch m.Kind {
	case Heartbeat, Push:
		if !r.Member() || r.IsOwner() || m.From != r.Members[0] {
			return
		}
		r.heard, r.fallback = now, false
		// Prune before merging: a write made after the owner pruned may
		// restart a pruned ID at version 1.
		if m.Prune != nil && m.Prune.LessOrEqual(r.State.Vector) {
			r.State.Prune(*m.Prune)
		}
		if m.Kind == Push {
			r.State.Merge(m.Delta)
			r.send(Message{Kind: PushAck, To: m.From, Delta: Delta{Vector: r.State.Vector.Clone()}})
			return
		}
		// Repair: send the owner what it misses, fallback writes included.
		r.send(Message{Kind: HeartbeatReply, To: m.From, Sent: m.Sent, Delta: r.State.DeltaSince(m.Delta.Vector)})
	case HeartbeatReply, PushAck:
		p := r.peers[m.From]
		if !r.IsOwner() || p == nil {
			return
		}
		if m.Kind == HeartbeatReply {
			r.State.Merge(m.Delta)
			if m.Sent > p.fresh {
				p.fresh = m.Sent
			}
			// The replica may miss owner writes; send them now.
			r.send(Message{Kind: Push, To: m.From, Delta: r.State.DeltaSince(m.Delta.Vector), Prune: r.pruned})
		}
		p.vector.Merge(m.Delta.Vector)
		r.release(now)
	case Handover:
		if !r.Member() || !r.IsOwner() {
			return
		}
		r.State.Merge(m.Delta)
		delete(r.leaving, m.From)
		delete(r.State.AwaitRetired, m.From)
		r.send(Message{Kind: HandoverAck, To: m.From, Delta: Delta{Vector: r.State.Vector.Clone()}})
	case HandoverAck:
		if !r.Member() && m.From == r.Members[0] && r.State.Vector.LessOrEqual(m.Delta.Vector) {
			r.handed = true
		}
	}
}

// Clean reports whether a read on the owner is exact (section 5.4): no
// former member's data is awaited, every replica answered a heartbeat sent
// less than Suspect/2 ago, and the owner holds everything that replica
// reported. A replica that answered at time t cannot enter fallback before
// t+Suspect, and the answer carried every write it had made.
func (r *Replica) Clean(now int64) bool {
	if !r.Member() || !r.IsOwner() || (len(r.leaving) > 0 && injected != CleanIgnoresLeavers) {
		return false
	}
	for _, m := range sortedMembers(r.peers) {
		p := r.peers[m]
		if injected != CleanWithoutFresh && (p.fresh < 0 || now-p.fresh >= r.Cfg.Suspect/2) {
			return false
		}
		if !p.vector.LessOrEqual(r.State.Vector) {
			return false
		}
	}
	return true
}

// CanTransfer reports whether a split, fold, MOVE or handoff of the zone may
// start (section 5.5): authority does not merge, so only a clean zone moves.
func (r *Replica) CanTransfer(now int64) bool { return r.Clean(now) }

// Read returns the zone state to serve and whether it is exact. A replica
// in fallback serves a degraded read; other replicas redirect.
func (r *Replica) Read(now int64) (*State, bool, error) {
	switch {
	case !r.Member():
		return nil, false, ErrRedirect
	case r.IsOwner():
		return r.State, r.Clean(now), nil
	case r.fallback:
		return r.State, false, nil
	}
	return nil, false, ErrRedirect
}

// TakeOutbox returns and clears pending messages and ACKs.
func (r *Replica) TakeOutbox() ([]Message, []Ack) {
	m, a := r.Outbox, r.Acks
	r.Outbox, r.Acks = nil, nil
	return m, a
}
