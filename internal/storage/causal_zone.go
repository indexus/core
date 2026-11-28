package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"strings"
)

func isRootLocation(loc string) bool { return loc == "" || loc == tree.BASE64.Root() }
func restageSnapshot(tx *Tx, operation, source string, snap Snapshot) error {
	existing, header, err := stagedFrom(tx, operation, snap.Zone, source)
	if err != nil && err != ErrNotReady {
		return err
	}
	if err == nil {
		// Certification binds source, bytes and receipt ledger irreversibly, even
		// before installation. A duplicate may acknowledge it, never replace it.
		if header.Certified != nil {
			seal, err := snap.Seal()
			if err != nil {
				return err
			}
			if *header.Certified != seal {
				return ErrConflict
			}
			return nil
		}
		// An uncertified warm copy can advance to the final snapshot, but a late
		// snapshot must not discard mutations already mirrored at the receiver.
		if snap.Seq < existing.seq(tx) {
			return ErrSequence
		}
		if err := tx.drop(string(existing)); err != nil {
			return err
		}
	}
	return stageSnapshot(tx, operation, source, snap)
}

// Batch operations are atomic; an empty batch is a side-effect-free no-op.
// Prepared bytes belong to one immutable MVCC snapshot. Sequence and state
// revalidation below precede all writes; a conflict rolls the entire lot back.
type preparedFence struct {
	snap     Snapshot
	seal     ZoneSeal
	raw      []byte
	state    []byte
	original ZoneState
}

var errFenceChanged = errors.New("zone changed during fence preparation")

const fencePrepareAttempts = 3

func (s *Store) prepareFence(operation causal.TransferID, zones []tree.ZoneKey) ([]preparedFence, error) {
	return read(s, func(tx *Tx) ([]preparedFence, error) {
		state, err := readCausal(tx)
		if err != nil {
			return nil, err
		}
		transfer, err := s.outgoingTransfer(state, operation)
		if err != nil {
			return nil, err
		}
		if !transfer.Discovered || transfer.Phase < causal.TransferReady {
			return nil, ErrNotReady
		}
		if transfer.Phase >= causal.TransferIssued {
			return nil, ErrConflict
		}
		out := make([]preparedFence, len(zones))
		for i, zone := range zones {
			if !covers(transfer.Cession, zone.ID) {
				return nil, ErrConflict
			}
			out[i].state = bytes.Clone(tx.value(zoneStatePath(operation, zone)))
			if out[i].state != nil {
				if err := json.Unmarshal(out[i].state, &out[i].original); err != nil {
					return nil, err
				}
			}
			if out[i].original.Frozen {
				out[i].snap, err = fencedSnapshot(tx, operation, zone)
			} else {
				out[i].snap, err = s.liveSnapshot(context.Background(), tx, zone, true)
			}
			if err != nil {
				return nil, err
			}
			out[i].seal, err = out[i].snap.Seal()
			if err != nil {
				return nil, err
			}
			out[i].raw, err = json.Marshal(out[i].snap)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	})
}

func (s *Store) freezePreparedTx(tx *Tx, state CausalState, operation causal.TransferID, prepared []preparedFence) error {
	transfer, err := s.outgoingTransfer(state, operation)
	if err != nil {
		return err
	}
	if !transfer.Discovered || transfer.Phase < causal.TransferReady {
		return ErrNotReady
	}
	if transfer.Phase >= causal.TransferIssued {
		return ErrConflict
	}
	for _, p := range prepared {
		zone := p.snap.Zone
		if !covers(transfer.Cession, zone.ID) {
			return ErrConflict
		}
		if !bytes.Equal(tx.value(zoneStatePath(operation, zone)), p.state) || liveZone(zone).seq(tx) != p.snap.Seq {
			return errFenceChanged
		}
		// Raw state and sequence were just revalidated under the writer.
		// Reuse the state decoded during preparation in the read transaction.
		st := p.original
		if st.Frozen {
			continue
		}
		if ready, err := zoneReady(tx, state, operation, zone); err != nil || !ready {
			return errors.Join(err, ErrNotReady)
		}
		st.Zone, st.Frozen = zone, true
		if err := putZoneState(tx, operation, st); err != nil {
			return err
		}
		if err := tx.put(transferRows(operation)+element(zoneBytes(zone)), p.raw); err != nil {
			return err
		}
	}
	// The first fence persists the phase transition. Later lots change
	// only their own rows; their authority metadata is already durable.
	if transfer.Phase == causal.TransferMoving && len(transfer.Snapshots) == 0 {
		return nil
	}
	transfer.PermissionTransfer, err = causal.StartMoving(transfer.PermissionTransfer)
	if err != nil {
		return err
	}
	transfer.Snapshots = nil
	state.Transfers[operation] = transfer
	return writeCausal(tx, state)
}

func (s *Store) FreezeZones(operation causal.TransferID, zones []tree.ZoneKey) ([]Snapshot, error) {
	snaps, _, _, err := s.fencePrepared(operation, zones, false)
	return snaps, err
}

func (s *Store) fencePrepared(operation causal.TransferID, zones []tree.ZoneKey, emit bool) ([]Snapshot, []ZoneSeal, []causal.ZoneTransfer, error) {
	if len(zones) == 0 {
		return nil, nil, nil, nil
	}
	release := s.beginFencePreparation(zones)
	defer release()
	for attempt := 0; attempt < fencePrepareAttempts; attempt++ {
		prepared, err := s.prepareFence(operation, zones)
		if err != nil {
			return nil, nil, nil, err
		}
		snaps, seals := make([]Snapshot, len(prepared)), make([]ZoneSeal, len(prepared))
		for i, p := range prepared {
			snaps[i], seals[i] = p.snap, p.seal
		}
		zts := make([]causal.ZoneTransfer, len(zones))
		err = s.updateCausalKind(commitFence, func(tx *Tx, state CausalState) error {
			if err := s.freezePreparedTx(tx, state, operation, prepared); err != nil {
				return err
			}
			if emit {
				return s.emitPreparedZoneTransfersTx(tx, state, operation, zones, zts, seals)
			}
			return nil
		})
		if errors.Is(err, errFenceChanged) {
			continue
		}
		return snaps, seals, zts, err
	}
	// No fallback that reads payloads under the writer: the caller can retry a
	// fresh lot. No fence is committed by an unsuccessful optimistic attempt.
	return nil, nil, nil, errors.Join(ErrNotReady, errFenceChanged)
}

func (s *Store) EmitZoneTransfers(operation causal.TransferID, zones []tree.ZoneKey) ([]causal.ZoneTransfer, error) {
	if len(zones) == 0 {
		return nil, nil
	}
	out := make([]causal.ZoneTransfer, len(zones))
	err := s.updateCausal(func(tx *Tx, state CausalState) error {
		return s.emitZoneTransfersTx(tx, state, operation, zones, out)
	})
	return out, err
}

func (s *Store) emitZoneTransfersTx(tx *Tx, state CausalState, operation causal.TransferID, zones []tree.ZoneKey, out []causal.ZoneTransfer) error {
	return s.emitPreparedZoneTransfersTx(tx, state, operation, zones, out, nil)
}

func (s *Store) emitPreparedZoneTransfersTx(tx *Tx, state CausalState, operation causal.TransferID, zones []tree.ZoneKey, out []causal.ZoneTransfer, seals []ZoneSeal) error {
	transfer, err := s.outgoingTransfer(state, operation)
	if err != nil {
		return err
	}
	lot := nextZoneLot(tx, operation)
	var row zoneLot
	for i, zone := range zones {
		st, ok, err := zoneStateOf(tx, state, operation, zone)
		if err != nil {
			return err
		}
		if !ok || !st.Frozen {
			return ErrNotReady
		}
		if ready, err := zoneReady(tx, state, operation, zone); err != nil || !ready {
			return errors.Join(err, ErrNotReady)
		}
		var seal ZoneSeal
		if seals != nil {
			seal = seals[i]
		} else {
			snap, err := transferSnapshot(tx, operation, zone)
			if err != nil {
				return err
			}
			seal, err = snap.Seal()
			if err != nil {
				return err
			}
		}
		barrier := seal.Seq
		zt := causal.ZoneTransfer{Parent: transfer.Cession.Input.ID, Operation: operation, Ticket: transfer.Cession.Ticket, Zone: zone, From: transfer.Cession.Source, To: transfer.Cession.Target, Seq: barrier, DataDigest: causal.Digest(seal.Digest)}
		if st.Transferred {
			existing, ok, err := zoneTransferOf(tx, state, operation, zone)
			if err != nil {
				return err
			}
			if ok {
				zt = existing
			}
			out[i] = zt
			continue
		}
		// The fact stays on this disk and the receiver's (XFER-KEYS-1),
		// not in the knowledge: a batch writes O(batch), and its proofs
		// one row, the lot's.
		if err := validZoneTransfer(zt); err != nil {
			return err
		}
		row.Zones = append(row.Zones, lotZone{Zone: zone, Seq: zt.Seq, DataDigest: zt.DataDigest})
		st.Transferred = true
		st.Zone = zone
		st.Lot = lot
		if err := putZoneState(tx, operation, st); err != nil {
			return err
		}
		out[i] = zt
	}
	if len(row.Zones) == 0 {
		return nil
	}
	return putZoneLot(tx, operation, lot, row) // rows only: the control document does not change
}

func (s *Store) StageZones(operation causal.TransferID, source string, snaps []Snapshot) error {
	if len(snaps) == 0 {
		return nil
	}
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		return s.stageZonesTx(tx, state, operation, source, snaps)
	})
}

func (s *Store) stageZonesTx(tx *Tx, state CausalState, operation causal.TransferID, source string, snaps []Snapshot) error {
	if t, ok := state.Transfers[operation]; ok && t.Phase == causal.TransferCancelled {
		return nil
	}

	if t, ok := state.Transfers[operation]; !ok || t.Phase < causal.TransferReady {
		return ErrNotReady
	}
	// After the region install every zone of the operation is mounted and
	// its per-zone states are compacted away: a delayed copy is moot.
	if state.Transfers[operation].Phase == causal.TransferInstalled {
		return nil
	}
	if receiverClosed(tx, state) {
		allInstalled := true
		for _, snap := range snaps {
			st, _, err := zoneStateOf(tx, state, operation, snap.Zone)
			if err != nil {
				return err
			}
			if !st.Installed {
				allInstalled = false
			}
		}
		if allInstalled {
			return nil
		}
		return ErrReceiverClosed
	}
	changed := false
	for _, snap := range snaps {
		st, _, err := zoneStateOf(tx, state, operation, snap.Zone)
		if err != nil {
			return err
		}
		// Installation is irreversible. A delayed copy must not replace the
		// certified base, or recreate staging after regional completion.
		if st.Installed {
			continue
		}
		if err := restageSnapshot(tx, operation.String(), source, snap); err != nil {
			return err
		}
		if err := certifySnapshot(tx, operation.String(), source, snap); err != nil {
			return err
		}
		st.Zone = snap.Zone
		st.Sealed = true
		if err := putZoneState(tx, operation, st); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return s.checkCapacity(tx)
}

// CertifyZones checks the already durable copy against the donor's final seals.
// No record or receipt crosses the wire again during this step.
func (s *Store) CertifyZones(operation causal.TransferID, source string, seals []ZoneSeal) error {
	if len(seals) == 0 {
		return nil
	}
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		return s.certifyZonesTx(tx, state, operation, source, seals)
	})
}

func (s *Store) certifyZonesTx(tx *Tx, state CausalState, operation causal.TransferID, source string, seals []ZoneSeal) error {
	if t, ok := state.Transfers[operation]; ok && t.Phase == causal.TransferCancelled {
		return nil
	}

	if t, ok := state.Transfers[operation]; !ok || t.Phase < causal.TransferReady {
		return ErrNotReady
	}
	if state.Transfers[operation].Phase == causal.TransferInstalled {
		return nil
	}
	changed := false
	for _, seal := range seals {
		st, _, err := zoneStateOf(tx, state, operation, seal.Zone)
		if err != nil {
			return err
		}
		if st.Installed {
			continue
		}
		if receiverClosed(tx, state) {
			return ErrReceiverClosed
		}
		if err := certifyZone(tx, operation.String(), source, seal); err != nil {
			return err
		}
		st.Zone, st.Sealed = seal.Zone, true
		if err := putZoneState(tx, operation, st); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return s.checkCapacity(tx)
}

func (s *Store) InstallZones(operation causal.TransferID, zts []causal.ZoneTransfer) error {
	if len(zts) == 0 {
		return nil
	}
	return s.updateCausal(func(tx *Tx, state CausalState) error {
		return s.installZonesTx(tx, state, operation, zts)
	})
}

func (s *Store) installZonesTx(tx *Tx, state CausalState, operation causal.TransferID, zts []causal.ZoneTransfer) error {
	if t, ok := state.Transfers[operation]; ok && t.Phase == causal.TransferCancelled {
		return nil
	}

	// A delayed per-zone install after the region install must never mount
	// the old bytes again over later acknowledged writes.
	if state.Transfers[operation].Phase == causal.TransferInstalled {
		return nil
	}
	identity := storageID(tx)
	lot := nextZoneLot(tx, operation)
	var row zoneLot
	for _, zt := range zts {
		current, exists := state.Transfers[operation]
		var slot *causal.PermissionTransfer
		if exists {
			slot = &current.PermissionTransfer
		}
		c := current.Cession
		c.Ticket = zt.Ticket
		if !exists {
			if zt.Ticket > 0 && zt.Ticket <= state.Permissions.Sequence {
				continue
			}
			return ErrNotReady
		}
		status, inspectErr := state.Permissions.Inspect(c, slot, causal.Owner{VNode: s.node, Storage: identity})
		if inspectErr != nil {
			return inspectErr
		}
		if status == causal.TicketClosed {
			continue
		}
		if zt.Operation != operation {
			return ErrConflict
		}
		if zt.To.VNode != s.node || zt.To.Storage != identity {
			return ErrNotOwner
		}
		st, _, err := zoneStateOf(tx, state, operation, zt.Zone)
		if err != nil {
			return err
		}
		if st.Installed {
			continue
		}
		t := state.Transfers[operation]
		if t.Phase < causal.TransferReady || t.Cession.Ticket != zt.Ticket || t.Cession.Input.ID != zt.Parent || t.Cession.Source != zt.From || t.Cession.Target != zt.To || !t.Cession.Region.Contains(zt.Zone.ID) {
			return ErrConflict
		}

		_, header, err := stagedZone(tx, operation.String(), zt.Zone)
		if err != nil {
			return err
		}
		if header.Certified == nil {
			return ErrNotReady
		}
		if header.Certified.Zone != zt.Zone || header.Certified.Seq != zt.Seq || causal.Digest(header.Certified.Digest) != zt.DataDigest {
			return ErrSequence
		}
		if err := validZoneTransfer(zt); err != nil {
			return err
		}
		receipt := Receipt{Source: zt.From.VNode, Target: s.node, Seal: *header.Certified}
		if err := s.installStagedZone(tx, operation.String(), receipt); err != nil {
			return err
		}
		row.Zones = append(row.Zones, lotZone{Zone: zt.Zone, Seq: zt.Seq, DataDigest: zt.DataDigest})
		st.Zone = zt.Zone
		st.Sealed = true
		st.Transferred = true
		st.Installed = true
		st.Lot = lot
		if err := putZoneState(tx, operation, st); err != nil {
			return err
		}
	}
	if len(row.Zones) > 0 {
		if err := putZoneLot(tx, operation, lot, row); err != nil {
			return err
		}
	}
	t := state.Transfers[operation]
	var phaseErr error
	t.PermissionTransfer, phaseErr = causal.StartMoving(t.PermissionTransfer)
	if phaseErr != nil {
		return phaseErr
	}
	state.Permissions.EndpointEpoch = max(state.Permissions.EndpointEpoch, t.Cession.Target.Epoch)
	state.Transfers[operation] = t
	if err := writeCausal(tx, state); err != nil {
		return err
	}
	return s.checkCapacity(tx)
}

// FenceZones fences zones and records their cession in ONE durable commit
// (FLIP-ONE-1). A fenced lot was never revocable (cancellation ends at the
// first fence), so nothing is lost by deciding both at once: the no-writer
// window drops a commit and the source keeps the fenced snapshot to resend.
// It returns the seal of each snapshot, computed before the fence, outside
// the writer: the caller sends them and does not hash the lot again while
// the lot has no writer.
func (s *Store) FenceZones(operation causal.TransferID, zones []tree.ZoneKey) ([]Snapshot, []ZoneSeal, []causal.ZoneTransfer, error) {
	return s.fencePrepared(operation, zones, true)
}

// stagingVersion is the canonical header (source, base digest, certification)
// plus mirrored sequence. Staging only advances by contiguous mirror mutations;
// replacing its base changes the header. This stamp is specific to one zone,
// so independent incoming transfers never invalidate each other's preparation.
type stagingVersion struct {
	zone                    tree.ZoneKey
	header, sequence, state []byte
}
type preparedMount struct {
	versions []stagingVersion
	pending  map[string][]byte
	paths    []string
	sealed   []tree.ZoneKey
	// certified is the content of each copy the preparation certified, for
	// the install: valid under the same check as pending (versions).
	certified map[tree.ZoneKey]certifiedCopy
}

var errMountChanged = errors.New("staging changed during mount preparation")

const mountPrepareAttempts = 3

func (s *Store) prepareMount(operation causal.TransferID, source string, delta []ZoneMutations, seals []ZoneSeal, snaps []Snapshot, zts []causal.ZoneTransfer) (preparedMount, error) {
	return read(s, func(tx *Tx) (preparedMount, error) {
		var prepared preparedMount
		state, err := readCausal(tx)
		if err != nil {
			return prepared, err
		}
		zones := make(map[tree.ZoneKey]struct{})
		for _, part := range delta {
			zones[part.Zone] = struct{}{}
		}
		for _, seal := range seals {
			zones[seal.Zone] = struct{}{}
		}
		for _, snap := range snaps {
			zones[snap.Zone] = struct{}{}
		}
		for _, zt := range zts {
			zones[zt.Zone] = struct{}{}
		}
		for zone := range zones {
			table := stagedTable(operation.String(), zone)
			prepared.versions = append(prepared.versions, stagingVersion{zone: zone,
				header:   bytes.Clone(tx.value(table.row(stageHeaderKey))),
				sequence: bytes.Clone(tx.value(table.row(sequenceKey))),
				state:    bytes.Clone(tx.value(zoneStatePath(operation, zone)))})
		}
		// This is a private overlay on a pinned MVCC reader. None of its writes
		// are committed here: it exists only to apply the final mirror, read and
		// hash the resulting copy, and encode its staging changes without writeMu.
		tx.writable = true
		tx.certified = make(map[tree.ZoneKey]certifiedCopy)
		if err := s.prepareMountTx(tx, state, operation, source, delta, seals, snaps); err != nil {
			return prepared, err
		}
		prepared.certified = tx.certified
		prepared.pending = make(map[string][]byte)
		prefix := stagingRows(operation.String())
		for path, raw := range tx.pending {
			if strings.HasPrefix(path, prefix) {
				prepared.pending[path] = raw
			}
		}
		prepared.paths = sortedKeys(prepared.pending)
		for zone := range zones {
			if _, changed := tx.pending[zoneStatePath(operation, zone)]; changed {
				prepared.sealed = append(prepared.sealed, zone)
			}
		}
		return prepared, nil
	})
}

func (s *Store) prepareMountTx(tx *Tx, state CausalState, operation causal.TransferID, source string, delta []ZoneMutations, seals []ZoneSeal, snaps []Snapshot) error {
	// The last delta of the fence rides along: applied to the staged copy
	// exactly as a mirror batch, before the seals are checked. A zone
	// already installed here ignores it, as it ignores its seal.
	if op, ok := state.Transfers[operation]; ok && op.Phase >= causal.TransferReady && op.Phase < causal.TransferInstalled {
		for _, part := range delta {
			st, _, err := zoneStateOf(tx, state, operation, part.Zone)
			if err != nil {
				return err
			}
			if st.Installed {
				continue
			}
			if err := mirrorZone(tx, operation.String(), part.Zone, part.Mutations); err != nil {
				return err
			}
		}
	}
	var err error
	if len(snaps) > 0 {
		err = s.stageZonesTx(tx, state, operation, source, snaps)
	} else if len(seals) > 0 {
		err = s.certifyZonesTx(tx, state, operation, source, seals)
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) mountPreparedTx(tx *Tx, state CausalState, operation causal.TransferID, prepared preparedMount, zts []causal.ZoneTransfer) error {
	if op, ok := state.Transfers[operation]; ok && (op.Phase == causal.TransferCancelled || op.Phase == causal.TransferInstalled) {
		return nil
	}
	for _, version := range prepared.versions {
		table := stagedTable(operation.String(), version.zone)
		if !bytes.Equal(tx.value(table.row(stageHeaderKey)), version.header) ||
			!bytes.Equal(tx.value(table.row(sequenceKey)), version.sequence) ||
			!bytes.Equal(tx.value(zoneStatePath(operation, version.zone)), version.state) {
			return errMountChanged
		}
	}
	if len(prepared.paths) > 0 && receiverClosed(tx, state) {
		return ErrReceiverClosed
	}
	for _, path := range prepared.paths {
		if err := tx.set(path, prepared.pending[path]); err != nil {
			return err
		}
	}
	for _, zone := range prepared.sealed {
		st, _, err := zoneStateOf(tx, state, operation, zone)
		if err != nil {
			return err
		}
		if st.Installed {
			continue
		}
		st.Zone, st.Sealed = zone, true
		if err := putZoneState(tx, operation, st); err != nil {
			return err
		}
	}
	// Staging is what the preparation read, plus the writes just applied,
	// which its certifications saw: the install takes their content as is.
	tx.certified = prepared.certified
	return s.installZonesTx(tx, state, operation, zts)
}

// MountZones certifies the receiver's copy of a fenced lot against the
// source's final seals (or stages the fenced snapshots themselves) and
// installs it, in ONE durable commit (FLIP-ONE-1). Every check of the
// separate steps runs unchanged; a refusal installs nothing.
func (s *Store) MountZones(operation causal.TransferID, source string, delta []ZoneMutations, seals []ZoneSeal, snaps []Snapshot, zts []causal.ZoneTransfer) error {
	if len(zts) == 0 {
		return nil
	}
	for attempt := 0; attempt < mountPrepareAttempts; attempt++ {
		prepared, err := s.prepareMount(operation, source, delta, seals, snaps, zts)
		if err != nil {
			return err
		}
		err = s.updateCausalKind(commitMount, func(tx *Tx, state CausalState) error {
			return s.mountPreparedTx(tx, state, operation, prepared, zts)
		})
		if errors.Is(err, errMountChanged) {
			continue
		}
		return err
	}
	return errors.Join(ErrNotReady, errMountChanged)
}

// FencedLot contains the verified material for replaying one issued lot.
type FencedLot struct {
	Snapshots []Snapshot
	Seals     []ZoneSeal
	Proofs    []causal.ZoneTransfer
}

// ReadFencedLot reads only the proofs of this operation, together with their
// snapshots. A replay sends the exact seals checked against these proofs.
func (s *Store) ReadFencedLot(operation causal.TransferID, zones []tree.ZoneKey) (FencedLot, error) {
	return read(s, func(tx *Tx) (FencedLot, error) {
		state, err := readCausal(tx)
		if err != nil {
			return FencedLot{}, err
		}
		if _, err = s.outgoingTransfer(state, operation); err != nil {
			return FencedLot{}, err
		}
		lot := FencedLot{
			Snapshots: make([]Snapshot, len(zones)),
			Seals:     make([]ZoneSeal, len(zones)),
			Proofs:    make([]causal.ZoneTransfer, len(zones)),
		}
		for i, zone := range zones {
			st, ok, err := zoneStateOf(tx, state, operation, zone)
			if err != nil {
				return FencedLot{}, err
			}
			if !ok || !st.Frozen || !st.Transferred {
				return FencedLot{}, ErrNotReady
			}
			if acked, err := zoneAcked(tx, operation, st); err != nil || acked {
				return FencedLot{}, errors.Join(err, ErrNotReady)
			}
			snap, err := fencedSnapshot(tx, operation, zone)
			if err != nil {
				return FencedLot{}, err
			}
			proof, exists, err := zoneTransferOf(tx, state, operation, zone)
			if err != nil {
				return FencedLot{}, err
			}
			if !exists || proof.Operation != operation || proof.Zone != zone {
				return FencedLot{}, ErrNotReady
			}
			seal, err := snap.Seal()
			if err != nil {
				return FencedLot{}, err
			}
			if seal.Zone != zone || snap.Seq != proof.Seq || causal.Digest(seal.Digest) != proof.DataDigest {
				return FencedLot{}, ErrSequence
			}
			lot.Snapshots[i], lot.Seals[i], lot.Proofs[i] = snap, seal, proof
		}
		return lot, nil
	})
}

// A saved fenced snapshot is mandatory until the installation receipt. Never
// reconstruct it from live rows: they may already have been collected or
// acquired again. A corrupt row or an I/O failure must remain an error.
func fencedSnapshot(tx *Tx, operation causal.TransferID, zone tree.ZoneKey) (Snapshot, error) {
	raw := tx.value(transferRows(operation) + element(zoneBytes(zone)))
	if raw == nil {
		return Snapshot{}, ErrNotReady
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, err
	}
	recordDecodes.Add(int64(len(snap.Records)))
	if snap.Zone != zone {
		return Snapshot{}, ErrConflict
	}
	return snap, nil
}

func (s *Store) noteMigrationZone(tx *Tx, z tree.ZoneKey) error {
	c, e := s.causalSnapshot(tx)
	if e != nil {
		return e
	}
	for id, t := range c.state.Transfers {
		if t.Cession.Source.Storage != c.storageID || t.Phase >= causal.TransferSealed || !t.Cession.Region.Contains(z.ID) {
			continue
		}
		if _, ok, e := zoneStateOf(tx, c.state, id, z); e != nil {
			return e
		} else if !ok {
			if e = putZoneState(tx, id, ZoneState{Zone: z}); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Store) FreezeRegion(id causal.TransferID) (CausalTransfer, error) {
	return s.fenceGranularRegion(id, false)
}
