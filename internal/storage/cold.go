package storage

import (
	"bytes"
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sort"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
)

var coldDrainKey = []byte("cold-drain-v1")
var coldPinKey = []byte("cold-pin-v1")
var coldActivityKey = []byte("cold-activity-v1")

// ColdDrain seals intake while ordinary writes retain their existing authority.
// Each piece consumes an entire native token into a stable pinned survivor.
// The source remains a forwarding endpoint after logical retirement.
type ColdPiece struct {
	Operation causal.TransferID `json:",omitzero"`
	Input     causal.Digest
	Region    tree.Region
}
type ColdDrain struct {
	Operation      string
	Source, Target causal.Endpoint
	BaseDigest     [32]byte
	Activity       uint64
	// COLD-2: owned bytes at admission, the survivor's admission fraction and
	// the writes tolerated before a new piece starts.
	Bytes               int64
	MaxCombinedFraction float64
	HeatBudget          uint64
	MaxItems            int64 `json:",omitempty"`
	Pieces              []ColdPiece
	Retirement          *causal.Member
	PinConfirmed        bool
	AbortRequested      bool
	Witnessed           bool
}
type ColdPin struct {
	Operation string
	Source    causal.Endpoint
	Activity  uint64
}
type ColdStatus struct {
	Drain    *ColdDrain
	Pin      *ColdPin
	Activity uint64
}

func coldActivity(tx *Tx) uint64 {
	return tx.number8(meta(coldActivityKey))
}
func noteColdWrite(tx *Tx) error {
	if !coldBusy(tx) {
		return nil
	}
	n := coldActivity(tx)
	if n == ^uint64(0) {
		return ErrSequence
	}
	return tx.put(meta(coldActivityKey), number(n+1))
}
func coldStatus(tx *Tx) (ColdStatus, error) {
	out := ColdStatus{Activity: coldActivity(tx)}
	if raw := tx.value(meta(coldDrainKey)); raw != nil {
		out.Drain = &ColdDrain{}
		if err := json.Unmarshal(raw, out.Drain); err != nil {
			return out, err
		}
	}
	if raw := tx.value(meta(coldPinKey)); raw != nil {
		out.Pin = &ColdPin{}
		if err := json.Unmarshal(raw, out.Pin); err != nil {
			return out, err
		}
	}
	return out, nil
}
func (s *Store) ColdStatus() (ColdStatus, error) { return read(s, coldStatus) }

// updateCold is update with the cold status it reads first.
func (s *Store) updateCold(fn func(*Tx, ColdStatus) error) error {
	return s.update(func(tx *Tx) error {
		status, err := coldStatus(tx)
		if err != nil {
			return err
		}
		return fn(tx, status)
	})
}
func coldBusy(tx *Tx) bool {
	return tx.value(meta(coldDrainKey)) != nil || tx.value(meta(coldPinKey)) != nil
}

// pendingCausal is whether a transfer is not installed yet. The staged
// copies of an installed transfer wait for compactStaging: nothing is pending
// in them, so the staging sessions do not enter the answer (the loop over
// them that stood here returned what the last line returns, 2026-11-26).
func pendingCausal(state CausalState, _ *Tx) bool {
	return !allInstalled(state)
}

// ColdLimits bounds one fold. Admission reads the O(1) byte counters only.
type ColdLimits struct {
	// MaxCombinedFraction admits a fold only if the survivor's owned bytes
	// plus the source's stay under this share of the survivor's capacity.
	MaxCombinedFraction float64
	HeatBudget          uint64
	MaxItems            int64 `json:",omitempty"`
}

// ColdOffer is what a survivor checks before it pins.
type ColdOffer struct {
	Bytes               int64
	MaxCombinedFraction float64
	// Items the source owns; MaxItems, when positive, bounds the survivor's
	// items after the fold (REM-124).
	Items, MaxItems int64
}

// BeginColdDrain is local, atomic, and does not trust a planner as permission.
// Initially only a pair fold is accepted: removing this vnode gives all its
// current cell and native grants to the same survivor in the supplied view.
func (s *Store) BeginColdDrain(operation string, target causal.Endpoint, limits ColdLimits) (ColdDrain, error) {
	var out ColdDrain
	err := s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Drain != nil {
			if status.Drain.Operation != operation || status.Drain.Target != target {
				return ErrConflict
			}
			out = *status.Drain
			return nil
		}
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		if operation == "" || !target.Valid() || target.VNode == s.node || state.Root.Name == s.node {
			return ErrConflict
		}
		pruneTentativeAdmissions(state, causal.Owner{VNode: s.node, Storage: storageID(tx)})
		if status.Pin != nil || state.Moving != nil || pendingCausal(state, tx) {
			return ErrNotReady
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			return err
		}
		view := replica.Current()
		if !view.HasName(s.node) || !view.HasName(target.VNode) {
			return ErrNotReady
		}
		from, err := replica.Endpoint(s.node)
		if err != nil {
			return err
		}
		currentTarget, err := replica.Endpoint(target.VNode)
		if err != nil {
			return err
		}
		if from.Storage != storageID(tx) || currentTarget != target {
			return ErrNotOwner
		}
		base := view.Digest
		out = ColdDrain{Operation: operation, Source: from, Target: target, BaseDigest: base, Activity: status.Activity, MaxCombinedFraction: limits.MaxCombinedFraction, HeatBudget: limits.HeatBudget, MaxItems: limits.MaxItems}
		// Placement chooses a survivor; only actual installed local rights
		// determine what is ceded. Sparse delegated exceptions migrate through
		// the current directory, not through a false Native==XOR-cell test.
		for _, right := range state.Permissions.Native {
			out.Pieces = append(out.Pieces, ColdPiece{Input: right.ID, Region: right.Region})
		}

		usage, err := s.storedDataUsage(tx)
		if err != nil {
			return err
		}
		out.Bytes = usage.Owned
		sort.Slice(out.Pieces, func(i, j int) bool { return bytes.Compare(out.Pieces[i].Input[:], out.Pieces[j].Input[:]) < 0 })
		if err := writeCausal(tx, state); err != nil {
			return err
		}
		return tx.putJSON(meta(coldDrainKey), out)
	})
	return out, err
}

// PinColdSurvivor prevents this disk from moving/retiring until its source has
// either aborted or delivered a retirement record. It never blocks its writes.
func (s *Store) PinColdSurvivor(operation string, source causal.Endpoint, offer ColdOffer) (ColdPin, error) {
	var out ColdPin
	var items int64
	if offer.MaxItems > 0 {
		totals, err := s.PlanningTotals()
		if err != nil {
			return out, err
		}
		items = totals.Items
	}
	err := s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Pin != nil {
			if status.Pin.Operation != operation || status.Pin.Source != source {
				return ErrConflict
			}
			out = *status.Pin
			return nil
		}
		if status.Drain != nil {
			return ErrNotReady
		}
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		if state.Moving != nil || pendingCausal(state, tx) {
			return ErrNotReady
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			return err
		}
		if !source.Valid() || source.VNode == s.node || operation == "" || offer.Bytes < 0 || !replica.Current().HasName(s.node) || !replica.Current().HasName(source.VNode) {
			return ErrNotReady
		}
		self, err := replica.Endpoint(s.node)
		if err != nil {
			return err
		}
		other, err := replica.Endpoint(source.VNode)
		if err != nil {
			return err
		}
		if self.Storage != storageID(tx) || source != other {
			return ErrNotOwner
		}
		if offer.MaxCombinedFraction > 0 {
			usage, err := s.storedDataUsage(tx)
			if err != nil {
				return err
			}
			// COLD-2 hysteresis: a split fires near 90% of capacity, so a
			// fold must leave the survivor far below it.
			if float64(usage.Owned+offer.Bytes) > offer.MaxCombinedFraction*float64(s.capacity) {
				return ErrDataCapacity
			}
		}
		// REM-124: a survivor over the item threshold would split again at
		// once, moving back what the fold just moved.
		if offer.MaxItems > 0 && items+offer.Items > offer.MaxItems {
			return ErrDataCapacity
		}
		out = ColdPin{operation, source, status.Activity}
		return tx.putJSON(meta(coldPinKey), out)
	})
	return out, err
}
func (s *Store) ReleaseColdPin(operation string, source causal.Endpoint) error {
	return s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Pin == nil || status.Pin.Operation != operation {
			// Another operation may have pinned the survivor after a lost release ACK.
			// The old pin is absent; never remove its successor.
			return nil
		}
		if status.Pin.Source != source {
			return ErrConflict
		}
		return tx.del(meta(coldPinKey))
	})
}

// AbortColdDrain never revokes an emitted transfer. Finish pending pieces first,
// then discard only the plan; normal reconciliation can use the installed facts.
func (s *Store) AbortColdDrain() error {
	return s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Drain == nil {
			return nil
		}
		if status.Drain.Retirement != nil {
			return ErrNotReady
		}
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		for _, piece := range status.Drain.Pieces {
			if tr, ok := state.Transfers[piece.Operation]; ok && tr.Phase != causal.TransferInstalled {
				return ErrNotReady
			}
		}
		return tx.del(meta(coldDrainKey))
	})
}
func (s *Store) PublishColdRetirement() (causal.Member, error) {
	var out causal.Member
	err := s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Drain == nil {
			return ErrNotReady
		}
		work := status.Drain
		if work.Retirement != nil {
			out = *work.Retirement
			return nil
		}
		if !work.PinConfirmed {
			return ErrNotReady
		}
		if work.AbortRequested {
			return protocol.ErrColdPlanPreempted
		}
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			return err
		}
		// Every piece is installed by now (checked below): heat no longer
		// preempts, since aborting would only send the data back (COLD-2).
		if replica.Current().Digest != work.BaseDigest {
			return protocol.ErrColdPlanPreempted
		}
		if pendingCausal(state, tx) {
			return ErrNotReady
		}
		id, ok := replica.Current().IDOf(s.node)
		if !ok {
			return ErrNotReady
		}
		if len(state.Permissions.Native) != 0 {
			return ErrNotReady
		}
		for _, t := range state.Transfers {
			if t.Phase != causal.TransferInstalled {
				return ErrNotReady
			}
		}
		out = causal.Member{VNode: tree.VNodeDescriptor{Name: s.node, ID: id}, Endpoint: work.Source, Retired: true, Redirect: work.Target}
		if err := replica.Merge(causal.MembershipDelta{Members: []causal.Member{out}}); err != nil {
			return err
		}
		if err := writeMembership(tx, &state, replica.Export()); err != nil {
			return err
		}

		work.Retirement = &out
		if err := writeCausal(tx, state); err != nil {
			return err
		}
		return tx.putJSON(meta(coldDrainKey), work)
	})
	return out, err
}

// A witness is durable before it releases its pin. Normal anti-entropy carries
// the immutable proof and all surviving placement endpoints; no global vote.
func (s *Store) WitnessColdRetirement(operation string, source causal.Endpoint, record causal.Member) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		if !record.Retired || record.Endpoint != source {
			return ErrConflict
		}
		status, e := coldStatus(tx)
		if e != nil {
			return e
		}
		matching := status.Pin != nil && status.Pin.Operation == operation && status.Pin.Source == source
		if !matching {
			for _, known := range state.Membership.Members {
				if known == record {
					return nil
				}
			}
			return ErrConflict
		}
		m, e := state.MembershipReplica()
		if e != nil {
			return e
		}
		if e = m.Merge(causal.MembershipDelta{Members: []causal.Member{record}}); e != nil {
			return e
		}
		if e = writeMembership(tx, &state, m.Export()); e != nil {
			return e
		}
		if e = writeCausal(tx, state); e != nil {
			return e
		}
		return tx.del(meta(coldPinKey))
	})
}
func (s *Store) CompleteColdRetirement() error {
	return s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Drain == nil || status.Drain.Retirement == nil {
			return ErrNotReady
		}
		status.Drain.Witnessed = true
		return tx.putJSON(meta(coldDrainKey), status.Drain)
	})
}

func (s *Store) ConfirmColdPin(pin ColdPin) error {
	return s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Drain == nil || status.Drain.Operation != pin.Operation || status.Drain.Source != pin.Source {
			return ErrConflict
		}
		status.Drain.PinConfirmed = true
		return tx.putJSON(meta(coldDrainKey), status.Drain)
	})
}
func (s *Store) RequestColdAbort() error {
	return s.updateCold(func(tx *Tx, status ColdStatus) error {
		if status.Drain == nil {
			return nil
		}
		if status.Drain.Retirement != nil {
			return ErrNotReady
		}
		status.Drain.AbortRequested = true
		return tx.putJSON(meta(coldDrainKey), status.Drain)
	})
}
