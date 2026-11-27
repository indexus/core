package storage

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func (s *Store) BeginCausal(ctx context.Context, input causal.Digest, target causal.Endpoint, region tree.Region) (CausalTransfer, error) {
	return s.beginCausal(ctx, input, target, region, false)
}

// BeginCausalPiece partitions and pins its selected right in one durable commit.
// Local coalescence must never invalidate a piece between those two decisions.
func (s *Store) BeginCausalPiece(ctx context.Context, input causal.Digest, target causal.Endpoint, region tree.Region) (CausalTransfer, error) {
	return s.beginCausal(ctx, input, target, region, true)
}

// beginCausal reserves a transfer of region to target in one durable commit,
// then lists the zones it moves, a page per commit. Both halves are
// repeatable: a second call with the same input returns the same transfer.
func (s *Store) beginCausal(ctx context.Context, input causal.Digest, target causal.Endpoint, region tree.Region, partition bool) (CausalTransfer, error) {
	result, err := s.reserveTransfer(input, target, region, partition)
	if err != nil || result.Discovered || result.Phase >= causal.TransferSealed {
		return result, err
	}
	return s.discoverZones(ctx, result, region)
}

// reserveTransfer is the commit that decides a transfer: the transfer already
// begun from this input if there is one, otherwise a new one when the cold
// state allows it.
func (s *Store) reserveTransfer(input causal.Digest, target causal.Endpoint, region tree.Region, partition bool) (result CausalTransfer, err error) {
	err = s.updateCausal(func(tx *Tx, state CausalState) error {
		if begun, found, e := begunTransfer(tx, state, input, target, region); found || e != nil {
			result = begun
			return e
		}
		drain, e := drainAllowing(tx, input, target, region)
		if e != nil {
			return e
		}
		source, e := s.sourceEndpoint(tx, state)
		if e != nil {
			return e
		}
		if partition {
			if coldBusy(tx) {
				return ErrNotReady
			}
			var selected causal.Right
			if state.Permissions, selected, e = state.Permissions.Partition(s.cluster, input, region, source.Owner); e != nil {
				return e
			}
			input = selected.ID
		}
		p, op, e := state.Permissions.Begin(s.cluster, input, region, source, target)
		if e != nil {
			return e
		}
		state.Permissions = p
		result = CausalTransfer{PermissionTransfer: op}
		state.Transfers[op.Cession.ID] = result
		if e = noteDrainOperation(tx, drain, input, op.Cession.ID); e != nil {
			return e
		}
		return writeCausal(tx, state)
	})
	return result, err
}

// begunTransfer finds the unfinished transfer this disk began from input.
// The same target and region make the call a repeat; anything else must wait.
func begunTransfer(tx *Tx, state CausalState, input causal.Digest, target causal.Endpoint, region tree.Region) (CausalTransfer, bool, error) {
	for _, t := range state.Transfers {
		if t.Cession.Source.Storage != storageID(tx) || t.Phase >= causal.TransferInstalled || t.Cession.Input.ID != input {
			continue
		}
		if t.Cession.Target == target && t.Cession.Region == region {
			return t, true, nil
		}
		return CausalTransfer{}, false, ErrNotReady
	}
	return CausalTransfer{}, false, nil
}

// drainAllowing refuses a transfer the cold state forbids: a pinned disk
// moves nothing, a draining disk only the pieces of its drain. It returns
// the drain in progress, nil when there is none.
func drainAllowing(tx *Tx, input causal.Digest, target causal.Endpoint, region tree.Region) (*ColdDrain, error) {
	status, e := coldStatus(tx)
	if e != nil {
		return nil, e
	}
	if status.Pin != nil {
		return nil, ErrNotReady
	}
	if status.Drain == nil {
		return nil, nil
	}
	for _, piece := range status.Drain.Pieces {
		if piece.Input == input && piece.Region == region && status.Drain.Target == target {
			return status.Drain, nil
		}
	}
	return nil, ErrNotReady
}

// noteDrainOperation records on the drain's piece the transfer that moves it.
func noteDrainOperation(tx *Tx, drain *ColdDrain, input causal.Digest, operation causal.TransferID) error {
	if drain == nil {
		return nil
	}
	for i := range drain.Pieces {
		if drain.Pieces[i].Input == input {
			drain.Pieces[i].Operation = operation
		}
	}
	return tx.putJSON(meta(coldDrainKey), drain)
}

// sourceEndpoint is this disk as the source of a cession: its membership
// endpoint when the membership names this storage, otherwise its own host
// (the contact host when one is set).
func (s *Store) sourceEndpoint(tx *Tx, state CausalState) (causal.Endpoint, error) {
	source := causal.Endpoint{Owner: causal.Owner{VNode: s.node, Storage: storageID(tx)}, Epoch: state.Permissions.EndpointEpoch}
	cached, e := s.causalSnapshot(tx)
	if e != nil {
		return source, e
	}
	source.Host = localHost(cached)
	if contact := s.contactHost.Load(); contact != nil {
		source.Host = *contact
	}
	m, e := state.MembershipReplica()
	if e != nil {
		return source, e
	}
	if h, e := m.Endpoint(s.node); e == nil && h.Storage == source.Storage {
		return h, nil
	}
	return source, nil
}

// discoverZones gives every zone of region that has rows a zone state under
// the transfer, 512 zones per commit, then marks the transfer discovered.
func (s *Store) discoverZones(ctx context.Context, result CausalTransfer, region tree.Region) (CausalTransfer, error) {
	operation := result.Cession.ID
	var from *tree.ZoneKey
	for {
		var page []tree.ZoneKey
		next, done, err := s.spatialZonePage(ctx, region, from, 512, rowsNone, func(_ *Tx, z tree.ZoneKey, _ []*row) error { page = append(page, z); return nil })
		if err != nil {
			return result, err
		}
		if len(page) > 0 {
			if err = s.noteDiscovered(operation, page, &result); err != nil {
				return result, err
			}
		}
		if done {
			return s.closeDiscovery(operation, result)
		}
		from = next
	}
}

// noteDiscovered commits the zone states of one page. A transfer another
// call discovered meanwhile is copied to result and left as it is.
func (s *Store) noteDiscovered(operation causal.TransferID, page []tree.ZoneKey, result *CausalTransfer) error {
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		current, exists := state.Transfers[operation]
		if !exists || current.Phase == causal.TransferCancelled {
			return ErrNotReady
		}
		if current.Discovered {
			*result = current
			return nil
		}
		for _, z := range page {
			_, known, err := zoneStateOf(tx, state, operation, z)
			if err != nil {
				return err
			}
			if known {
				continue
			}
			if err := putZoneState(tx, operation, ZoneState{Zone: z}); err != nil {
				return err
			}
		}
		return nil
	})
}

// closeDiscovery marks the transfer discovered: its zone list is complete.
func (s *Store) closeDiscovery(operation causal.TransferID, result CausalTransfer) (CausalTransfer, error) {
	err := s.updateCausal(func(tx *Tx, state CausalState) error {
		tr, exists := state.Transfers[operation]
		if !exists || tr.Phase == causal.TransferCancelled {
			return ErrNotReady
		}
		tr.Discovered = true
		state.Transfers[operation] = tr
		result = tr
		return writeCausal(tx, state)
	})
	result.Discovered = err == nil
	return result, err
}

// fenceGranularRegion closes discovery once every known zone has installed and
// been ACKed, unless force is true (bounded fallback under unbounded new-zone
// creation). Readiness is scanned in bounded read pages and validated in O(1)
// against zoneStatesVersion inside the writer transaction.
func (s *Store) fenceGranularRegion(operation causal.TransferID, force bool) (CausalTransfer, error) {
	var out CausalTransfer
	if err := s.view(func(tx *Tx) error {
		cached, err := s.causalSnapshot(tx)
		if err != nil {
			return err
		}
		out, err = s.outgoingTransfer(cached.state, operation)
		return err
	}); err != nil || out.Phase >= causal.TransferSealed {
		return out, err
	}
	var ver uint64
	if !force {
		ready := true
		var err error
		ver, err = s.eachZoneStatePaged(operation, func(_ *Tx, _ CausalState, st ZoneState) (bool, error) {
			ready = st.Transferred && st.Acked
			return ready, nil
		})
		if errors.Is(err, errZoneStatesChanged) || !ready {
			return out, ErrNotReady
		}
		if err != nil {
			return out, err
		}
	}
	err := s.updateCausal(func(tx *Tx, state CausalState) error {
		var err error
		out, err = s.outgoingTransfer(state, operation)
		if !out.Discovered || out.Phase < causal.TransferReady {
			return ErrNotReady
		}
		if err != nil || out.Phase >= causal.TransferSealed {
			return err
		}
		if !force && zoneStatesVersion(tx, operation) != ver {
			return ErrNotReady
		}
		out.Phase = causal.TransferSealed
		state.Transfers[operation] = out
		return writeCausal(tx, state)
	})
	return out, err
}

// ForceFreezeRegion fences a granular region even if late-born zones remain,
// so unbounded zone creation cannot starve regional closure indefinitely.
func (s *Store) ForceFreezeRegion(operation causal.TransferID) (CausalTransfer, error) {
	return s.fenceGranularRegion(operation, true)
}

// SealCausalRegion consolidates immutable per-zone proofs after the last flip.
// Proofs are scanned in bounded read pages and hashed outside any store lock,
// then committed after an O(1) version check. It never rereads zone payloads.
func (s *Store) SealCausalRegion(operation causal.TransferID) (CausalTransfer, error) {
	var out CausalTransfer
	for range 4 {
		if err := s.view(func(tx *Tx) error {
			cached, err := s.causalSnapshot(tx)
			if err != nil {
				return err
			}
			out, err = s.outgoingTransfer(cached.state, operation)
			if err != nil || out.Phase >= causal.TransferIssued {
				return err
			}
			if out.Phase < causal.TransferSealed {
				return ErrNotReady
			}
			return nil
		}); err != nil || out.Phase >= causal.TransferIssued {
			return out, err
		}
		seals := make([]zoneSeal, 0)
		var barrier uint64
		ver, err := s.eachZoneStatePaged(operation, func(tx *Tx, state CausalState, st ZoneState) (bool, error) {
			if !st.Transferred || !st.Acked {
				return false, ErrNotReady
			}
			zt, ok, err := zoneTransferOf(tx, state, operation, st.Zone)
			if err != nil || !ok {
				return false, errors.Join(err, ErrNotReady)
			}
			seals = append(seals, zoneSeal{Zone: st.Zone, Digest: zt.DataDigest})
			barrier = max(barrier, zt.Seq)
			return true, nil
		})
		if errors.Is(err, errZoneStatesChanged) {
			continue
		}
		if err != nil {
			return out, err
		}
		digest := regionDigest(seals)
		err = s.updateCausal(func(tx *Tx, state CausalState) error {
			current, err := s.outgoingTransfer(state, operation)
			if err != nil {
				return err
			}
			if current.Phase >= causal.TransferIssued {
				out = current
				return nil
			}
			if current.Phase < causal.TransferSealed {
				return ErrNotReady
			}
			if zoneStatesVersion(tx, operation) != ver {
				return errZoneStatesChanged
			}
			current.Cession.DataDigest, current.Cession.Barrier = digest, barrier
			state.Transfers[operation] = current
			out = current
			return writeCausal(tx, state)
		})
		if errors.Is(err, errZoneStatesChanged) {
			continue
		}
		return out, err
	}
	return out, ErrNotReady
}
