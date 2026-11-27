package mesh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
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

// membershipWire retains one sent/received version per peer, not a history of
// certificates or versions. Eviction, lost replies and restart resend a body.
// The mutex protects small scheduling/receipt maps; mergeMu never guards data.
type membershipWire struct {
	snapshot func() (*MembershipEnvelope, error)
	merge    func(MembershipEnvelope) error
	mergeMu  sync.Mutex
	mu       sync.Mutex
	peers    map[string]membershipReceipt
	order    []string
	pending  map[string]bool
	closed   bool
	ctx      context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
	slots    chan struct{}
}

func newMembershipWire(snapshot func() (*MembershipEnvelope, error), merge func(MembershipEnvelope) error) *membershipWire {
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

func (w *membershipWire) packet(known causal.Digest, full bool) (MembershipPacket, error) {
	e, err := w.snapshot()
	if err != nil {
		return MembershipPacket{}, err
	}
	p := MembershipPacket{Known: known}
	// A client without a routing view can bootstrap by asking for the body.
	if e == nil {
		return p, nil
	}
	copy := e.clone()
	raw, err := json.Marshal(copy)
	if err != nil {
		return MembershipPacket{}, err
	}
	p.Version = sha256.Sum256(raw)
	metrics.Gossip.MembershipBytes.Store(int64(len(raw)))
	metrics.Gossip.Version.Store(hex.EncodeToString(p.Version[:])) // observation, never an authority barrier
	if full {
		p.Body = &copy
	}
	return p, nil
}

func (w *membershipWire) accept(ctx context.Context, peer string, p MembershipPacket) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.Version == (causal.Digest{}) {
		if p.Body != nil {
			return false, causal.ErrConflict
		}
		return false, nil
	}
	w.mergeMu.Lock()
	defer w.mergeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.Body == nil {
		return w.receipt(peer).received != p.Version, nil
	}
	body := p.Body.clone()
	if causal.Hash(body) != p.Version {
		return false, causal.ErrConflict
	}
	if w.receipt(peer).received != p.Version {
		if err := w.merge(body); err != nil {
			return false, err
		}
		w.remember(peer, causal.Digest{}, p.Version)
	}
	return false, nil
}

// receive handles a control request. A missing reference after restart asks for
// a body; it never delays or changes the outcome of a separate data request.
func (w *membershipWire) receive(ctx context.Context, peer string, p MembershipPacket) (MembershipReply, error) {
	need, err := w.accept(ctx, peer, p)
	if err != nil {
		return MembershipReply{}, err
	}
	r := MembershipReply{NeedBody: need}
	if !need {
		r.Accepted = p.Version
	}
	r.Packet, err = w.packet(p.Version, true)
	if r.Packet.Version == p.Known {
		r.Packet.Body = nil
	}
	observeMembershipPacket(r.Packet)
	return r, err
}

// exchange sends at most two control requests. Only an explicit NeedBody can
// cause the retry; transport errors leave receipts unchanged for a later round.
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
	if p.Version != r.sent {
		p, err = w.packet(r.received, true)
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
		need, err := w.accept(ctx, peer, out.Packet)
		if err != nil {
			return err
		}
		if need {
			return causal.ErrDependency
		} // peer must honor Known on its reply
		if out.Accepted == p.Version {
			w.remember(peer, out.Accepted, causal.Digest{})
		}
		if !out.NeedBody {
			return nil
		}
		if attempt != 0 || p.Body != nil {
			return causal.ErrDependency
		}
		p, err = w.packet(w.receipt(peer).received, true)
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
	if p.Body != nil {
		metrics.Gossip.FullPackets.Add(1)
	} else {
		metrics.Gossip.ReferencePackets.Add(1)
	}
}
