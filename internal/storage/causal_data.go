package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync/atomic"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// This format intentionally has no migration path: no deployed old disks exist.
var causalKey = []byte("local-permissions-v1")
var membershipKey = []byte("membership-v1")
var controlVersionKey = []byte("local-permissions-version")
var controlValueReads atomic.Int64

func controlVersion(tx *Tx) []byte { return tx.value(meta(controlVersionKey)) }

type ZoneState struct {
	Zone                                   tree.ZoneKey
	Frozen, Sealed, Transferred, Installed bool
	Acked                                  bool `json:",omitempty"`
	// Lot is the lot row that holds the zone's transfer: its proof and, on
	// the source, the acknowledgement of its install (zone_lots.go). Zero:
	// the zone is not transferred, or has a ZoneTransfer row of its own.
	Lot uint64 `json:",omitempty"`
}
type CausalTransfer struct {
	causal.PermissionTransfer
	Snapshots []Snapshot `json:",omitempty"`
	// Discovery is data progress, not an authority decision.
	Discovered bool `json:",omitempty"`
}
type CausalState struct {
	Root             tree.VNodeDescriptor
	Genesis          causal.Genesis
	Membership       causal.MembershipDelta `json:",omitzero"`
	MembershipDigest causal.Digest
	Permissions      causal.LocalPermissions
	Moving           *causal.Endpoint `json:",omitempty"`
	Transfers        map[causal.TransferID]CausalTransfer
}

func (s CausalState) MembershipReplica() (*causal.Membership, error) {
	m, e := causal.NewMembership(s.Root, s.Genesis)
	if e == nil {
		e = m.Merge(s.Membership)
	}
	return m, e
}
func (s CausalState) cloneControl() CausalState {
	c := s
	c.Permissions = s.Permissions.Clone()
	if s.Moving != nil {
		v := *s.Moving
		c.Moving = &v
	}
	c.Transfers = make(map[causal.TransferID]CausalTransfer, len(s.Transfers))
	for id, t := range s.Transfers {
		t.Snapshots = append([]Snapshot(nil), t.Snapshots...)
		c.Transfers[id] = t
	}
	return c
}

type decodedControl struct {
	version []byte
	state   CausalState
}

func cachedControl(tx *Tx, version []byte) *CausalState {
	if tx.owner == nil || version == nil {
		return nil
	}
	for i := range tx.owner.controlCache {
		if c := tx.owner.controlCache[i].Load(); c != nil && bytes.Equal(c.version, version) {
			return &c.state
		}
	}
	return nil
}
func keepControl(tx *Tx, version []byte, state CausalState) {
	if tx.owner == nil {
		return
	}
	tx.owner.controlCache[1].Store(tx.owner.controlCache[0].Load())
	tx.owner.controlCache[0].Store(&decodedControl{bytes.Clone(version), state.cloneControl()})
}

// A membership delta is immutable inside storage. Internal control snapshots
// borrow it by content digest; public exports detach its member slice.
type decodedMembership struct {
	digest causal.Digest
	delta  causal.MembershipDelta
}

func keepMembership(tx *Tx, digest causal.Digest, delta causal.MembershipDelta) {
	if tx.owner == nil {
		return
	}
	cached := tx.owner.membershipCache[:]
	for i := len(cached) - 1; i > 0; i-- {
		cached[i].Store(cached[i-1].Load())
	}
	cached[0].Store(&decodedMembership{digest, delta})
}

func readMembership(tx *Tx, digest causal.Digest) (causal.MembershipDelta, error) {
	if tx.owner != nil {
		for i := range tx.owner.membershipCache {
			if c := tx.owner.membershipCache[i].Load(); c != nil && c.digest == digest {
				return c.delta, nil
			}
		}
	}
	raw := tx.value(meta(membershipKey))
	if raw == nil || causal.Digest(sha256.Sum256(raw)) != digest {
		return causal.MembershipDelta{}, ErrConflict
	}
	var delta causal.MembershipDelta
	if e := json.Unmarshal(raw, &delta); e != nil {
		return delta, e
	}
	keepMembership(tx, digest, delta)
	return delta, nil
}

// writeMembership changes the immutable membership blob and its control
// reference in the same transaction. Local fences/receipts call writeCausal
// only: they never encode, hash, compare or rewrite all network participants.
func writeMembership(tx *Tx, state *CausalState, delta causal.MembershipDelta) error {
	raw, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	digest := causal.Digest(sha256.Sum256(raw))
	if digest != state.MembershipDigest {
		if err = tx.put(meta(membershipKey), raw); err != nil {
			return err
		}
	}
	state.Membership, state.MembershipDigest = delta, digest
	keepMembership(tx, digest, delta)
	return nil
}

func readCausalControlVersion(tx *Tx) (CausalState, []byte, error) {
	var state CausalState
	version := controlVersion(tx)
	if cached := cachedControl(tx, version); cached != nil {
		state = cached.cloneControl()
	} else {
		raw := tx.value(meta(causalKey))
		if raw == nil {
			return state, nil, ErrNotReady
		}
		controlValueReads.Add(1)
		if e := json.Unmarshal(raw, &state); e != nil {
			return state, nil, e
		}
		sum := sha256.Sum256(raw)
		if !bytes.Equal(sum[:], version) {
			return state, nil, ErrConflict
		}
		keepControl(tx, version, state)
	}
	var e error
	state.Membership, e = readMembership(tx, state.MembershipDigest)
	return state, version, e
}

func readCausalControl(tx *Tx) (CausalState, error) {
	s, _, e := readCausalControlVersion(tx)
	return s, e
}
func readCausal(tx *Tx) (CausalState, error) { return readCausalControl(tx) }
func writeCausalDigest(tx *Tx, state CausalState) (causal.Digest, error) {
	if state.MembershipDigest == (causal.Digest{}) {
		return causal.Digest{}, ErrConflict
	}
	state.Membership = causal.MembershipDelta{}
	control, e := json.Marshal(state)
	if e != nil {
		return causal.Digest{}, e
	}
	if e = tx.put(meta(causalKey), control); e != nil {
		return causal.Digest{}, e
	}
	version := sha256.Sum256(control)
	e = tx.put(meta(controlVersionKey), version[:])
	if e == nil {
		keepControl(tx, version[:], state)
	}
	return state.MembershipDigest, e
}
func writeCausal(tx *Tx, state CausalState) error { _, e := writeCausalDigest(tx, state); return e }
func (s *Store) writeCausalChecked(tx *Tx, state CausalState) error {
	if e := writeCausal(tx, state); e != nil {
		return e
	}
	return s.checkCapacity(tx)
}
func (s *Store) CausalState() (CausalState, error) {
	state, err := read(s, readCausal)
	state.Membership.Members = append([]causal.Member(nil), state.Membership.Members...)
	return state, err
}
func (s *Store) Endpoint(host string) (causal.Endpoint, error) {
	if host != "" {
		if current := s.contactHost.Load(); current == nil || *current != host {
			contact := host
			s.contactHost.Store(&contact)
		}
	}
	// Every acknowledged write and every summary contribution names this
	// endpoint: between commits of the control state it reads no row.
	if c := s.committedCausal(); c != nil {
		return causal.Endpoint{Owner: causal.Owner{VNode: s.node, Storage: c.storageID}, Host: host, Epoch: c.state.Permissions.EndpointEpoch}, nil
	}
	return read(s, func(tx *Tx) (causal.Endpoint, error) {
		epoch := uint64(0)
		var identity string
		if controlVersion(tx) != nil {
			snapshot, e := s.causalSnapshot(tx)
			if e != nil {
				return causal.Endpoint{}, e
			}
			epoch = snapshot.state.Permissions.EndpointEpoch
			identity = snapshot.storageID
		} else {
			if tx.value(meta(causalKey)) != nil {
				return causal.Endpoint{}, ErrConflict
			}
			identity = storageID(tx)
		}
		return causal.Endpoint{Owner: causal.Owner{VNode: s.node, Storage: identity}, Host: host, Epoch: epoch}, nil
	})
}
func (s *Store) InitCausal(root tree.VNodeDescriptor, g causal.Genesis) error {
	return s.initCausal(root, g, false)
}
func (s *Store) InitCausalReceiver(root tree.VNodeDescriptor, g causal.Genesis) error {
	return s.initCausal(root, g, true)
}
func (s *Store) initCausal(root tree.VNodeDescriptor, g causal.Genesis, receiver bool) error {
	return s.update(func(tx *Tx) error {
		if tx.value(meta(causalKey)) != nil {
			old, e := readCausal(tx)
			if e != nil {
				return e
			}
			if old.Root != root || old.Genesis != g {
				return ErrConflict
			}
			return nil
		}
		if g.Cluster != s.cluster {
			return ErrWrongCluster
		}
		m, e := causal.NewMembership(root, g)
		if e != nil {
			return e
		}
		populated, e := hasZones(tx)
		if e != nil {
			return e
		}
		if populated || tx.value(meta(viewKey)) != nil {
			return ErrConflict
		}
		state := CausalState{Root: root, Genesis: g, Transfers: map[causal.TransferID]CausalTransfer{}}
		if e = writeMembership(tx, &state, m.Export()); e != nil {
			return e
		}
		if !receiver && g.Holder.VNode == s.node {
			if g.Holder.Storage != storageID(tx) {
				return ErrMissingData
			}
			state.Permissions.Native = []causal.Right{g.Right()}
			state.Permissions.EndpointEpoch = g.Holder.Epoch
		}
		if e = tx.putJSON(meta(viewKey), encodeView(m.Current())); e != nil {
			return e
		}
		return writeCausal(tx, state)
	})
}
func (s *Store) MergeMembership(delta causal.MembershipDelta) error {
	s.mergeMu.Lock()
	defer s.mergeMu.Unlock()
	stale := errors.New("membership changed during preparation")
	for {
		state, e := s.CausalState()
		if e != nil {
			return e
		}
		m, e := state.MembershipReplica()
		if e != nil {
			return e
		}
		before := m.Digest()
		if e = m.Merge(delta); e != nil {
			return e
		}
		if m.Digest() == before {
			return nil
		}
		next := m.Export()
		view := m.Current()
		viewRaw, e := json.Marshal(encodeView(view))
		if e != nil {
			return e
		}
		expected := state.MembershipDigest
		e = s.update(func(tx *Tx) error {
			current, err := readCausal(tx)
			if err != nil {
				return err
			}
			if current.MembershipDigest != expected {
				return stale
			}
			if e := writeMembership(tx, &current, next); e != nil {
				return e
			}
			if e := prunePendingSpawns(tx, view); e != nil {
				return e
			}
			if e := tx.put(meta(viewKey), viewRaw); e != nil {
				return e
			}
			return writeCausal(tx, current)
		})
		if errors.Is(e, stale) {
			continue
		}
		return e
	}
}

func (s *Store) Membership() (tree.VNodeDescriptor, causal.Genesis, causal.MembershipDelta, error) {
	state, e := s.CausalState()
	return state.Root, state.Genesis, state.Membership, e
}
func (s *Store) CausalInitialized() (bool, error) {
	return read(s, func(tx *Tx) (bool, error) { return tx.value(meta(causalKey)) != nil, nil })
}

const MaxReceiverAdmissions = 128

func (s *Store) ReserveCausal(c causal.Cession) (causal.Cession, error) {
	var result causal.Cession
	e := s.updateCausal(func(tx *Tx, state CausalState) error {
		if c.Cluster != s.cluster {
			return ErrWrongCluster
		}
		self := causal.Owner{VNode: s.node, Storage: storageID(tx)}
		// A fresh vnode can reuse a departed receiver's address. Refuse its
		// predecessor's initial offer so the donor can release the unfenced input.
		if c.Ticket == 0 && c.Target.Owner != self && c.ValidOffer() {
			return ErrReceiverClosed
		}
		if receiverClosed(tx, state) || !admissionEpochOpen(state, c.Target, self) {
			return ErrReceiverClosed
		}
		// Bound current admission state, including adversarial old zero-ticket
		// headers. Only tentative slots may be evicted; confirmed slots never expire.
		if _, exists := state.Transfers[c.ID]; !exists && c.Ticket == 0 {
			count := 0
			var victim causal.TransferID
			var oldest uint64
			for id, t := range state.Transfers {
				if t.Cession.Target.Storage != storageID(tx) || t.Phase >= causal.TransferInstalled {
					continue
				}
				count++
				if t.Phase == causal.TransferReserved && (oldest == 0 || t.Cession.Ticket < oldest) {
					victim, oldest = id, t.Cession.Ticket
				}
			}
			if count >= MaxReceiverAdmissions {
				if oldest == 0 {
					return ErrDataCapacity
				}
				delete(state.Transfers, victim)
			}
		}
		old, ok := state.Transfers[c.ID]
		var current *causal.PermissionTransfer
		if ok {
			current = &old.PermissionTransfer
		}
		p, op, status, e := state.Permissions.Reserve(c, current, self)
		if e != nil {
			return e
		}
		if status == causal.TicketClosed {
			return ErrNotReady
		}
		result = op.Cession
		state.Permissions = p
		state.Transfers[c.ID] = CausalTransfer{PermissionTransfer: op}
		if ok {
			old.PermissionTransfer = op
			state.Transfers[c.ID] = old
		}
		return writeCausal(tx, state)
	})
	return result, e
}
func (s *Store) BindCausal(id causal.TransferID, ticket uint64) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, e := s.outgoingTransfer(state, id)
		if e != nil {
			return e
		}
		t.PermissionTransfer, e = causal.BindTicket(t.PermissionTransfer, ticket)
		if e != nil {
			return e
		}
		state.Transfers[id] = t
		return writeCausal(tx, state)
	})
}
func (s *Store) ConfirmCausal(id causal.TransferID) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, e := s.outgoingTransfer(state, id)
		if e != nil {
			return e
		}
		t.PermissionTransfer, e = causal.Confirm(t.PermissionTransfer, t.Cession)
		if e != nil {
			return e
		}
		state.Transfers[id] = t
		return writeCausal(tx, state)
	})
}
func (s *Store) PreloadCausal(offer CausalTransfer) (map[string]uint64, error) {
	progress := map[string]uint64{}
	c := offer.Cession
	e := s.updateCausal(func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[c.ID]
		if !ok || t.Phase == causal.TransferReserved {
			if receiverClosed(tx, state) || !admissionEpochOpen(state, c.Target, causal.Owner{VNode: s.node, Storage: storageID(tx)}) {
				return ErrReceiverClosed
			}
		}
		if !ok {
			return ErrNotReady
		}
		status, e := state.Permissions.Inspect(c, &t.PermissionTransfer, causal.Owner{VNode: s.node, Storage: storageID(tx)})
		if e != nil {
			return e
		}
		if status != causal.TicketOpen {
			return ErrNotReady
		}
		if receiverClosed(tx, state) && t.Phase == causal.TransferReserved {
			return ErrReceiverClosed
		}
		if !coversAll(c, offer.Snapshots) {
			return ErrConflict
		}
		if e = s.prepareSnapshots(tx, c.ID.String(), c.Source.VNode, offer.Snapshots, progress); e != nil {
			return e
		}
		first := t.Phase == causal.TransferReserved
		t.PermissionTransfer, e = causal.Confirm(t.PermissionTransfer, c)
		if e != nil {
			return e
		}
		if first {
			if e = replaceReadDelegation(tx, c.Region, &c.Source, c.Ticket); e != nil {
				return e
			}
		}
		state.Transfers[c.ID] = t
		return s.writeCausalChecked(tx, state)
	})
	return progress, e
}
func (s *Store) StageCausal(offer CausalTransfer) error {
	c := offer.Cession
	if len(offer.Snapshots) != 0 {
		return ErrConflict
	}
	if c.Cluster != s.cluster {
		return ErrWrongCluster
	}
	for range 4 {
		digest, version, e := s.receivedRegionDigestPaged(c.ID)
		if errors.Is(e, errZoneStatesChanged) {
			continue
		}
		if e != nil {
			return e
		}
		e = s.updateCausal(func(tx *Tx, state CausalState) error {
			t, ok := state.Transfers[c.ID]
			var current *causal.PermissionTransfer
			if ok {
				current = &t.PermissionTransfer
			}
			status, e := state.Permissions.Inspect(c, current, causal.Owner{VNode: s.node, Storage: storageID(tx)})
			if e != nil || status == causal.TicketClosed {
				return e
			}
			if t.Phase < causal.TransferReady {
				return ErrNotReady
			}
			if zoneStatesVersion(tx, c.ID) != version {
				return errZoneStatesChanged
			}
			if digest != c.DataDigest {
				return ErrConflict
			}
			t.PermissionTransfer, e = causal.Seal(t.PermissionTransfer, c)
			if e != nil {
				return e
			}
			state.Transfers[c.ID] = t
			return s.writeCausalChecked(tx, state)
		})
		if errors.Is(e, errZoneStatesChanged) {
			continue
		}
		return e
	}
	return ErrNotReady
}
func (s *Store) EmitCausal(id causal.TransferID) (causal.Cession, error) {
	var out causal.Cession
	e := s.updateCausal(func(tx *Tx, state CausalState) error {
		t, e := s.outgoingTransfer(state, id)
		if e != nil {
			return e
		}
		if t.Phase < causal.TransferSealed {
			return ErrNotReady
		}
		out = t.Cession
		if t.Phase >= causal.TransferIssued {
			return nil
		}
		state.Permissions, t.PermissionTransfer, e = state.Permissions.Emit(t.PermissionTransfer, causal.Owner{VNode: s.node, Storage: storageID(tx)})
		if e != nil {
			return e
		}
		state.Transfers[id] = t
		if e = replaceReadDelegation(tx, out.Region, &out.Target, out.ID.Sequence); e != nil {
			return e
		}
		return writeCausal(tx, state)
	})
	return out, e
}
func (s *Store) InstallCausal(c causal.Cession) error {
	if c.Cluster != s.cluster {
		return ErrWrongCluster
	}
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[c.ID]
		var current *causal.PermissionTransfer
		if ok {
			current = &t.PermissionTransfer
		}
		p, op, status, e := state.Permissions.Install(c, current, causal.Owner{VNode: s.node, Storage: storageID(tx)})
		if e != nil {
			return e
		}
		if status == causal.TicketClosed && op.Cession.ID != (causal.TransferID{}) {
			state.Permissions = p
			t.PermissionTransfer = op
			state.Transfers[c.ID] = t
			if e = replaceReadDelegation(tx, c.Region, nil, 0); e != nil {
				return e
			}
			if e = tx.del(stagingRows(c.ID.String())); e != nil {
				return e
			}
			return s.writeCausalChecked(tx, state)
		}
		return nil
	})
}
func (s *Store) CompleteCausal(id causal.TransferID) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, e := s.outgoingTransfer(state, id)
		if e != nil {
			return e
		}
		if t.Phase != causal.TransferIssued && t.Phase != causal.TransferInstalled {
			return ErrNotReady
		}
		t.Phase = causal.TransferInstalled
		state.Transfers[id] = t
		return writeCausal(tx, state)
	})
}
func (s *Store) AbortCausal(id causal.TransferID) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[id]
		if !ok {
			return nil
		}
		if t.Phase == causal.TransferCancelled {
			return nil
		}
		if !causal.CanCancel(t.PermissionTransfer) {
			return ErrNotReady
		}
		if t.Cession.Source.Storage != storageID(tx) {
			return ErrNotOwner
		}
		t.Phase = causal.TransferCancelled
		state.Transfers[id] = t
		if e := dropTransferSnapshots(tx, id); e != nil {
			return e
		}
		return writeCausal(tx, state)
	})
}
func (s *Store) FinishAbortCausal(id causal.TransferID) error {
	if e := s.compactZoneRows(id); e != nil {
		return e
	}
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[id]
		if !ok {
			return nil
		}
		if t.Phase != causal.TransferCancelled || t.Cession.Source.Storage != storageID(tx) {
			return ErrNotReady
		}
		delete(state.Transfers, id)
		return writeCausal(tx, state)
	})
}

// CancelCausal is sent only after the source durably aborts pre-first-fence.
func (s *Store) CancelCausal(c causal.Cession) error {
	if c.Cluster != s.cluster {
		return ErrWrongCluster
	}
	cleanup := false
	err := s.updateCausal(func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[c.ID]
		self := causal.Owner{VNode: s.node, Storage: storageID(tx)}
		if ok && t.Phase == causal.TransferCancelled {
			if t.Cession.Target.Owner != self || t.Cession.Binding() != c.Binding() || t.Cession.Ticket != c.Ticket {
				return ErrConflict
			}
			cleanup = true
			return nil
		}
		var current *causal.PermissionTransfer
		if ok {
			current = &t.PermissionTransfer
		}
		status, e := state.Permissions.Inspect(c, current, self)
		if e != nil || status == causal.TicketClosed {
			return e
		}
		if !causal.CanCancel(t.PermissionTransfer) {
			return ErrNotReady
		}
		t.Phase = causal.TransferCancelled
		state.Transfers[c.ID] = t
		cleanup = true
		if e = tx.del(stagingRows(c.ID.String())); e != nil {
			return e
		}
		return writeCausal(tx, state)
	})
	if err != nil || !cleanup {
		return err
	}
	if err = s.compactStaging(c.ID.String()); err != nil {
		return err
	}
	if err = s.compactZoneRows(c.ID); err != nil {
		return err
	}
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[c.ID]
		if !ok {
			return nil
		}
		if t.Phase != causal.TransferCancelled || t.Cession.Digest() != c.Digest() {
			return ErrConflict
		}
		delete(state.Transfers, c.ID)
		return writeCausal(tx, state)
	})
}
func (s *Store) Rights(region tree.Region) ([]causal.Right, error) {
	return read(s, func(tx *Tx) ([]causal.Right, error) {
		state, e := readCausal(tx)
		if e != nil {
			return nil, e
		}
		var out []causal.Right
		for _, r := range state.Permissions.Native {
			if _, yes := r.Region.Intersect(region); yes {
				out = append(out, r)
			}
		}
		return out, nil
	})
}

// CollectTransferred removes what an installed transfer left behind, in
// bounded commits: its staging, its snapshot rows (and, at the source, the
// live rows of the zones it ceded), its zone rows, then the transfer itself.
// Every step can be repeated after a crash.
func (s *Store) CollectTransferred(id causal.TransferID) error {
	return s.collectOnce(id, s.collectTransferred)
}

type collectionResult struct {
	done chan struct{}
	err  error
}

// The handoff and recovery loops can both see the same installed transfer.
// Only overlapping calls share a result. The next caller can retry any error.
func (s *Store) collectOnce(id causal.TransferID, collect func(causal.TransferID) error) error {
	// If the collector panics, waiters wake with a retryable result.
	work := &collectionResult{done: make(chan struct{}), err: ErrNotReady}
	if active, loaded := s.collecting.LoadOrStore(id, work); loaded {
		work = active.(*collectionResult)
		<-work.done
		return work.err
	}
	defer func() {
		s.collecting.Delete(id)
		close(work.done)
	}()
	work.err = collect(id)
	return work.err
}

func (s *Store) collectTransferred(id causal.TransferID) error {
	if e := s.compactStaging(id.String()); e != nil {
		return e
	}
	for from := ""; ; {
		paths, e := s.transferRowPage(id, from)
		if e != nil {
			return e
		}
		if len(paths) == 0 {
			break
		}
		from = paths[len(paths)-1]
		if e = s.collectRows(id, paths); e != nil {
			return e
		}
	}
	if e := s.compactZoneRows(id); e != nil {
		return e
	}
	return s.forgetTransfer(id)
}

// transferRowPage is up to 256 snapshot rows of the transfer after from.
func (s *Store) transferRowPage(id causal.TransferID, from string) (paths []string, err error) {
	prefix := transferRows(id)
	err = s.view(func(tx *Tx) error {
		return tx.scanRows(prefix, from, false, func(path string, _ []byte) bool {
			if path == prefix || path == from {
				return true
			}
			paths = append(paths, path)
			return len(paths) < 256
		})
	})
	return paths, err
}

// collectRows deletes one page of snapshot rows in one commit. Installed
// snapshot rows are immutable: the known key is deleted without decoding its
// payload again under the writer.
func (s *Store) collectRows(id causal.TransferID, paths []string) error {
	return s.updateCausalKind(commitCollect, func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[id]
		if !ok || t.Phase != causal.TransferInstalled {
			return ErrNotReady
		}
		cached, e := s.causalSnapshot(tx)
		if e != nil {
			return e
		}
		source := t.Cession.Source.Storage == storageID(tx)
		for _, path := range paths {
			if source {
				if e := s.dropCededZone(tx, cached, path[len(transferRows(id)):]); e != nil {
					return e
				}
			}
			if e := tx.set(path, nil); e != nil {
				return e
			}
		}
		return nil
	})
}

// dropCededZone deletes the live rows of a zone this disk no longer owns;
// a zone it may still write (taken back since) is left alone.
func (s *Store) dropCededZone(tx *Tx, cached *causalSnapshot, escaped string) error {
	z, e := zoneFromBytes(unescape(escaped))
	if e != nil {
		return e
	}
	if e = s.writableIn(tx, cached, z); !errors.Is(e, ErrNotOwner) {
		return nil
	}
	return s.dropLiveZone(tx, zoneBytes(z))
}

// forgetTransfer removes a collected transfer from the control state.
func (s *Store) forgetTransfer(id causal.TransferID) error {
	return s.updateCausalKind(commitCollect, func(tx *Tx, state CausalState) error {
		t, ok := state.Transfers[id]
		if !ok {
			return nil
		}
		if t.Phase != causal.TransferInstalled {
			return ErrNotReady
		}
		// While a cold drain can resume its pieces, the installed operation is
		// its durable completion receipt. Transfer rows have already been freed.
		if raw := tx.value(meta(coldDrainKey)); raw != nil {
			var drain ColdDrain
			if err := json.Unmarshal(raw, &drain); err != nil {
				return err
			}
			if drain.Retirement == nil {
				for _, piece := range drain.Pieces {
					if piece.Operation == id {
						return nil
					}
				}
			}
		}
		delete(state.Transfers, id)
		return writeCausal(tx, state)
	})
}

func (s *Store) BeginCausalMove(target causal.Endpoint) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		if coldBusy(tx) {
			return ErrNotReady
		}
		pruneTentativeAdmissions(state, causal.Owner{VNode: s.node, Storage: storageID(tx)})
		if pendingCausal(state, tx) {
			return ErrNotReady
		}
		if !target.Valid() || target.VNode != s.node || target.Storage == storageID(tx) {
			return ErrConflict
		}
		if state.Moving != nil {
			if *state.Moving == target {
				return nil
			}
			return ErrNotReady
		}
		m, e := state.MembershipReplica()
		if e != nil {
			return e
		}
		self, e := m.Endpoint(s.node)
		if e != nil {
			return e
		}
		if self.Epoch == ^uint64(0) {
			return ErrConflict
		}
		if self.Storage != storageID(tx) || target.Epoch != self.Epoch+1 {
			return ErrNotOwner
		}
		state.Moving = &target
		return writeCausal(tx, state)
	})
}
func (s *Store) PublishCausalMove() (causal.Member, error) {
	var out causal.Member
	e := s.updateCausal(func(tx *Tx, state CausalState) error {
		if state.Moving == nil || len(state.Permissions.Native) != 0 || !allInstalled(state) {
			return ErrNotReady
		}
		m, e := state.MembershipReplica()
		if e != nil {
			return e
		}
		for _, member := range m.Export().Members {
			if member.VNode.Name == s.node {
				out = member
			}
		}
		out.Endpoint = *state.Moving
		if e = m.Merge(causal.MembershipDelta{Members: []causal.Member{out}}); e != nil {
			return e
		}
		if e = writeMembership(tx, &state, m.Export()); e != nil {
			return e
		}
		state.Moving = nil
		return writeCausal(tx, state)
	})
	return out, e
}

func (s *Store) dropLiveZone(tx *Tx, zone []byte) error {
	z, e := zoneFromBytes(zone)
	if e != nil {
		return e
	}
	if e = tx.dropZone(z); e != nil {
		return e
	}
	return tx.drop(key(historyBucket, zone))
}
func zoneFromBytes(raw []byte) (tree.ZoneKey, error) {
	collection, location, ok := bytes.Cut(raw, []byte{0})
	if !ok {
		return tree.ZoneKey{}, ErrConflict
	}
	return tree.NewZoneKey(string(collection), string(location))
}

// PublishActivation checks ACT-1 on the publishing disk; other honest peers
// merge only this compact routing record, never an authority chain.
//
// In one transaction: the vnode must be this store's and the endpoint this
// disk's; an already active vnode may only advance its contact; a first
// activation needs something to serve (ACT-1); then the member is merged and
// published.
func (s *Store) PublishActivation(vnode tree.VNodeDescriptor, endpoint causal.Endpoint) (causal.Member, error) {
	out := causal.Member{VNode: vnode, Endpoint: endpoint}
	e := s.updateCausal(func(tx *Tx, state CausalState) error {
		if vnode.Name != s.node || !s.localHolder(tx, endpoint) {
			return ErrNotOwner
		}
		active, e := activeMayPublish(tx, state, vnode.Name, endpoint)
		if e != nil {
			return e
		}
		// Existing identity + same disk may advance its contact without
		// draining confirmed slots. ACT-1 applies to its first activation.
		ready := active || len(state.Permissions.Native) > 0
		if !ready {
			if ready, e = receivedInstalledZone(tx, state, endpoint.Owner); e != nil {
				return e
			}
		}
		if !ready {
			return ErrNotReady
		}
		return publishMember(tx, state, out)
	})
	return out, e
}

// activeMayPublish reports whether the vnode is already an active member,
// and then checks that it may publish endpoint: on the same disk, at an
// epoch that does not go back, and, when the endpoint changes, neither
// moving nor draining.
func activeMayPublish(tx *Tx, state CausalState, name string, endpoint causal.Endpoint) (active bool, err error) {
	m, e := state.MembershipReplica()
	if e != nil {
		return false, e
	}
	if !m.Current().HasName(name) {
		return false, nil
	}
	current, e := m.Endpoint(name)
	if e != nil {
		return false, e
	}
	if current.Owner != endpoint.Owner || endpoint.Epoch < current.Epoch || endpoint.Epoch == current.Epoch && current != endpoint {
		return false, ErrNotReady
	}
	if current == endpoint {
		return true, nil
	}
	status, e := coldStatus(tx)
	if e != nil {
		return false, e
	}
	if state.Moving != nil || status.Drain != nil {
		return false, ErrNotReady
	}
	return true, nil
}

// receivedInstalledZone reports whether a transfer towards owner, moving or
// later, has installed at least one of its zones here.
func receivedInstalledZone(tx *Tx, state CausalState, owner causal.Owner) (bool, error) {
	for id, t := range state.Transfers {
		if t.Cession.Target.Owner != owner || t.Phase < causal.TransferMoving {
			continue
		}
		installed := false
		if e := eachZoneState(tx, state, id, func(st ZoneState) bool { installed = st.Installed; return !installed }); e != nil {
			return false, e
		}
		if installed {
			return true, nil
		}
	}
	return false, nil
}

// publishMember merges the member into the membership and writes it with the
// view it gives and the endpoint epoch it reaches.
func publishMember(tx *Tx, state CausalState, member causal.Member) error {
	m, e := state.MembershipReplica()
	if e != nil {
		return e
	}
	if e = m.Merge(causal.MembershipDelta{Members: []causal.Member{member}}); e != nil {
		return e
	}
	if e = writeMembership(tx, &state, m.Export()); e != nil {
		return e
	}
	state.Permissions.EndpointEpoch = max(state.Permissions.EndpointEpoch, member.Endpoint.Epoch)
	if e = tx.putJSON(meta(viewKey), encodeView(m.Current())); e != nil {
		return e
	}
	return writeCausal(tx, state)
}

// A moved endpoint never reopens its old intake. Future physical returns use
// a strictly newer endpoint epoch; installed rights and writes ignore this gate.
func admissionEpochOpen(state CausalState, target causal.Endpoint, self causal.Owner) bool {
	for _, member := range state.Membership.Members {
		if member.VNode.Name != self.VNode {
			continue
		}
		if member.Retired {
			return false
		}
		return member.Endpoint.Owner == self || member.Endpoint.Epoch < target.Epoch
	}
	return true
}

// Header-only tentative slots can be discarded atomically before closing
// intake. A confirmed preload changes the phase in the same data transaction.
func pruneTentativeAdmissions(state CausalState, self causal.Owner) {
	for id, t := range state.Transfers {
		if t.Cession.Target.Owner == self && t.Phase == causal.TransferReserved {
			delete(state.Transfers, id)
		}
	}
}
