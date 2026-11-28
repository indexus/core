package mesh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// MembershipEnvelope carries routing information only. Receiving it never
// installs a right, confirms an admission ticket or permits an ACK.
type MembershipEnvelope struct {
	Root    tree.VNodeDescriptor
	Genesis causal.Genesis
	Delta   causal.MembershipDelta
}

func (e MembershipEnvelope) clone() MembershipEnvelope {
	e.Delta.Members = slices.Clone(e.Delta.Members)
	return e
}

// MembershipPacket belongs exclusively to the membership control RPC. Data
// requests do not carry a packet or wait for one to be accepted.
type MembershipPacket struct {
	Version causal.Digest       `json:",omitzero"`
	Known   causal.Digest       `json:",omitzero"`
	Base    causal.Digest       `json:",omitzero"` // nonzero when Body carries changes from this version
	Body    *MembershipEnvelope `json:",omitempty"`
}

type MembershipReply struct {
	Accepted causal.Digest `json:",omitzero"`
	NeedBody bool          `json:",omitempty"`
	Packet   MembershipPacket
}

type membershipSend func(context.Context, string, MembershipPacket) (MembershipReply, error)

const (
	membershipPeerLimit    = 4096
	membershipSyncLimit    = 8
	membershipSyncTimeout  = 5 * time.Second
	membershipHintCooldown = time.Second
)

var errMembershipBusy = errors.New("membership exchange capacity reached")

type membershipReceipt struct {
	sent, received causal.Digest
	hinted         time.Time
}

type membershipKey struct {
	root     tree.VNodeDescriptor
	genesis  causal.Genesis
	revision causal.Digest
}

// export must read the same immutable revision as key.
type localMembership struct {
	key    membershipKey
	export func() causal.MembershipDelta
}

type preparedMembership struct {
	key     membershipKey
	version causal.Digest
	body    *MembershipEnvelope
	base    causal.Digest
	changes *MembershipEnvelope
}

func (local localMembership) envelope() *MembershipEnvelope {
	return &MembershipEnvelope{Root: local.key.root, Genesis: local.key.genesis, Delta: local.export()}
}

// membershipWire retains one sent/received version per peer, not a history of
// certificates or versions. Eviction, lost replies and restart resend a body.
// The mutex protects small scheduling/receipt maps; mergeMu never guards data.
type membershipWire struct {
	snapshot  func() (localMembership, error)
	merge     func(MembershipEnvelope) error
	prepareMu sync.Mutex
	prepared  atomic.Pointer[preparedMembership]
	mergeMu   sync.Mutex
	mu        sync.Mutex
	peers     map[string]membershipReceipt
	order     []string
	pending   map[string]bool
	closed    bool
	ctx       context.Context
	stop      context.CancelFunc
	wg        sync.WaitGroup
	slots     chan struct{}
}

func newMembershipWire(snapshot func() (localMembership, error), merge func(MembershipEnvelope) error) *membershipWire {
	ctx, stop := context.WithCancel(context.Background())
	return &membershipWire{snapshot: snapshot, merge: merge, ctx: ctx, stop: stop,
		peers: make(map[string]membershipReceipt), pending: make(map[string]bool),
		slots: make(chan struct{}, membershipSyncLimit)}
}

func (w *membershipWire) receipt(peer string) membershipReceipt {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peers[peer]
}

func (w *membershipWire) putPeer(peer string, r membershipReceipt) {
	if _, exists := w.peers[peer]; !exists {
		if len(w.order) == membershipPeerLimit {
			delete(w.peers, w.order[0])
			copy(w.order, w.order[1:])
			w.order = w.order[:len(w.order)-1]
		}
		w.order = append(w.order, peer)
	}
	w.peers[peer] = r
}
func (w *membershipWire) remember(peer string, sent, received causal.Digest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.peers[peer]
	if sent != (causal.Digest{}) {
		r.sent = sent
	}
	if received != (causal.Digest{}) {
		r.received = received
	}
	w.putPeer(peer, r)
}

// Keep one complete immutable revision and at most one transition shared by
// all peers. A receipt never retains another copy of the inventory.
func (w *membershipWire) localSnapshot() (*preparedMembership, error) {
	local, err := w.snapshot()
	if err != nil || local.export == nil {
		return nil, err
	}
	cached := w.prepared.Load()
	if cached != nil && cached.key == local.key {
		return cached, nil
	}
	w.prepareMu.Lock()
	defer w.prepareMu.Unlock()
	cached = w.prepared.Load()
	if cached != nil && cached.key == local.key {
		return cached, nil
	}
	body := local.envelope()
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	next := &preparedMembership{key: local.key, version: sha256.Sum256(raw), body: body}
	if cached != nil {
		next.changes = membershipChanges(cached.body, body)
		if next.changes != nil {
			next.base = cached.version
		}
	}
	w.prepared.Store(next)
	metrics.Gossip.MembershipBytes.Store(int64(len(raw)))
	metrics.Gossip.Version.Store(hex.EncodeToString(next.version[:]))
	return next, nil
}

func (w *membershipWire) packetAgainst(known, base causal.Digest, body bool) (MembershipPacket, error) {
	p := MembershipPacket{Known: known}
	local, err := w.localSnapshot()
	if err != nil || local == nil {
		return p, err
	}
	p.Version = local.version
	if body {
		src := local.body
		if local.changes != nil && local.base == base {
			p.Base = base
			src = local.changes
		}
		copy := src.clone()
		p.Body = &copy
	}
	return p, nil
}

func (w *membershipWire) packet(known causal.Digest, full bool) (MembershipPacket, error) {
	return w.packetAgainst(known, causal.Digest{}, full)
}

func (w *membershipWire) accept(ctx context.Context, peer string, p MembershipPacket) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.Base != (causal.Digest{}) && (p.Body == nil || p.Base == p.Version) {
		return false, causal.ErrConflict
	}
	if p.Version == (causal.Digest{}) {
		if p.Body != nil {
			return false, causal.ErrConflict
		}
		return false, nil
	}
	if p.Body == nil {
		if w.receipt(peer).received == p.Version {
			return false, nil
		}
		// A peer can learn our current version from a reply before we have
		// received that version from it. Check the local digest in that case.
		if p.Known == p.Version {
			local, err := w.localSnapshot()
			if err != nil {
				return false, err
			}
			if local != nil && local.version == p.Version {
				w.remember(peer, causal.Digest{}, p.Version)
				return false, nil
			}
		}
		return true, nil
	}
	w.mergeMu.Lock()
	defer w.mergeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	body := p.Body.clone()
	verification := &body
	if p.Base != (causal.Digest{}) {
		local, err := w.localSnapshot()
		if err != nil {
			return false, err
		}
		if local == nil || local.version != p.Base {
			return true, nil
		}
		verification, err = expandMembership(local.body, &body)
		if err != nil {
			return false, err
		}
	}
	if causal.Hash(verification) != p.Version {
		return false, causal.ErrConflict
	}
	if w.receipt(peer).received != p.Version {
		if err := w.mergeUnlessCurrent(p.Version, body); err != nil {
			return false, err
		}
		w.remember(peer, causal.Digest{}, p.Version)
	}
	return false, nil
}

// Different neighbors can send the same complete inventory. After checking
// the packet digest, reuse our prepared version only while its durable
// revision and cluster identity are still current.
func (w *membershipWire) mergeUnlessCurrent(version causal.Digest, body MembershipEnvelope) error {
	if cached := w.prepared.Load(); cached != nil && cached.version == version {
		local, err := w.snapshot()
		if err != nil {
			return err
		}
		if local.export != nil && local.key == cached.key {
			return nil
		}
	}
	return w.merge(body)
}

// receive handles a control request. A missing reference after restart asks for
// a body; it never delays or changes the outcome of a separate data request.
func (w *membershipWire) receive(ctx context.Context, peer string, p MembershipPacket) (MembershipReply, error) {
	need, err := w.accept(ctx, peer, p)
	if err != nil {
		return MembershipReply{}, err
	}
	if need {
		// The caller will send a full repair. Its merged view can differ
		// from this one, so defer our inventory until that request.
		return MembershipReply{NeedBody: true}, nil
	}
	r := MembershipReply{Accepted: p.Version}
	r.Packet, err = w.packet(p.Version, false)
	sameAfterMerge := p.Body != nil && r.Packet.Version == p.Version
	if err == nil && r.Packet.Version != p.Known && !sameAfterMerge {
		r.Packet, err = w.packetAgainst(p.Version, p.Known, true)
	}
	observeMembershipPacket(r.Packet)
	return r, err
}

// exchange sends at most two control requests. A missing reference or patch base
// can trigger one full repair. Transport errors return to the caller.
func (w *membershipWire) exchange(ctx context.Context, peer string, send membershipSend) error {
	select {
	case w.slots <- struct{}{}:
		defer func() { <-w.slots }()
	default:
		return errMembershipBusy
	}
	r := w.receipt(peer)
	p, err := w.packet(r.received, false)
	if err != nil {
		return err
	}
	if p.Version != r.sent && p.Version != r.received {
		p, err = w.packetAgainst(r.received, r.received, true)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		observeMembershipPacket(p)
		out, err := send(ctx, peer, p)
		if err != nil {
			return err
		}
		if out.NeedBody && out.Accepted != (causal.Digest{}) || !out.NeedBody && out.Accepted != p.Version {
			return causal.ErrConflict
		}
		if out.Accepted == p.Version {
			w.remember(peer, out.Accepted, causal.Digest{})
		}
		// A merge that produced exactly our advertised revision can be
		// acknowledged in both directions without echoing the inventory.
		if !out.NeedBody && p.Body != nil && out.Packet.Body == nil && out.Packet.Base == (causal.Digest{}) && out.Packet.Version == p.Version {
			w.remember(peer, causal.Digest{}, p.Version)
			return nil
		}
		need, err := w.accept(ctx, peer, out.Packet)
		if err != nil {
			return err
		}
		if !out.NeedBody && !need {
			return nil
		}
		if attempt != 0 || (need && out.Packet.Base == (causal.Digest{})) || (out.NeedBody && p.Body != nil && p.Base == (causal.Digest{})) {
			return causal.ErrDependency
		}
		// The peers may have merged different inventories during the first
		// request. Request a complete reply as well as sending a full body.
		p, err = w.packet(causal.Digest{}, true)
		if err != nil {
			return err
		}
	}
	return causal.ErrDependency
}

// schedule is a nonblocking hint, suitable for a successful write's epilogue.
// Saturation drops the hint: periodic anti-entropy remains responsible for
// eventual dissemination. At most eight peers have work in flight at once.
func (w *membershipWire) schedule(peer string, send membershipSend) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	if w.pending[peer] {
		w.mu.Unlock()
		return true
	}
	r := w.peers[peer]
	now := time.Now()
	// Routing retry pacing only: this clock neither expires a reservation nor
	// changes any permission. Explicit periodic Sync bypasses the hint cooldown.
	if len(w.pending) == membershipSyncLimit || !r.hinted.IsZero() && now.Sub(r.hinted) < membershipHintCooldown {
		metrics.Gossip.AsyncSyncDropped.Add(1)
		w.mu.Unlock()
		return false
	}
	r.hinted = now
	w.putPeer(peer, r)
	w.pending[peer] = true
	metrics.Gossip.AsyncSyncs.Add(1)
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		ctx, cancel := context.WithTimeout(w.ctx, membershipSyncTimeout)
		defer cancel()
		if err := w.exchange(ctx, peer, send); err != nil && w.ctx.Err() == nil {
			metrics.Gossip.AsyncSyncFailures.Add(1)
		}
		w.mu.Lock()
		delete(w.pending, peer)
		w.mu.Unlock()
	}()
	return true
}

func (w *membershipWire) close() {
	w.mu.Lock()
	w.closed = true
	w.stop()
	w.mu.Unlock()
	w.wg.Wait()
}

func observeMembershipPacket(p MembershipPacket) {
	if p.Version == (causal.Digest{}) {
		return
	}
	if p.Base != (causal.Digest{}) {
		metrics.Gossip.DeltaPackets.Add(1)
	} else if p.Body != nil {
		metrics.Gossip.FullPackets.Add(1)
	} else {
		metrics.Gossip.ReferencePackets.Add(1)
	}
}
