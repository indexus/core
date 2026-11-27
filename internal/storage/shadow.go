package storage

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/tree"
	"os"
	"strings"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

const shadowPrefix = "shadow:"

// ZoneSeq pairs a zone key with its durable sequence number.
type ZoneSeq struct {
	Zone tree.ZoneKey
	Seq  uint64
}

// ShadowDir is the dedicated zonelog directory for shadow copies on this disk.
func ShadowDir(path string) string { return path + ".shadow" }

// shadowSession names the staging session holding shadow copies from owner's
// storage ID (convergent-replication-wiring.md §2).
func shadowSession(owner string) string { return shadowPrefix + owner }

func isShadow(session string) bool { return strings.HasPrefix(session, shadowPrefix) }

func (s *Store) openShadowLocked(create bool) (*zonelog.Store, error) {
	if s.shadowDB != nil {
		return s.shadowDB, nil
	}
	dir := ShadowDir(s.path)
	if !create {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}
	mem := s.memoryBytes / 2
	db, err := zonelog.Open(dir, zonelog.Options{NoSync: s.noSync, MemoryBytes: mem})
	if err != nil {
		return nil, err
	}
	s.shadowDB = db
	return db, nil
}

func (s *Store) shadowView(fn func(*Tx) error) error {
	s.shadowMu.RLock()
	if s.shadowDB != nil {
		err := newTx(s.shadowDB, false).run(fn)
		s.shadowMu.RUnlock()
		return err
	}
	s.shadowMu.RUnlock()

	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	db, err := s.openShadowLocked(false)
	if err != nil || db == nil {
		return err
	}
	return newTx(db, false).run(fn)
}

func (s *Store) shadowUpdate(fn func(*Tx) error) error {
	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	db, err := s.openShadowLocked(true)
	if err != nil {
		return err
	}
	tx := newTx(db, true)
	return tx.commit(fn(tx))
}

// ShadowBytes returns the on-disk byte size of the isolated shadow store.
func (s *Store) ShadowBytes() int64 {
	entries, err := os.ReadDir(ShadowDir(s.path))
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !info.IsDir() {
			total += info.Size()
		}
	}
	return total
}

func (s *Store) noteOwnedSeq(z tree.ZoneKey, seq uint64) {
	s.ownedSeqsMu.Lock()
	if s.ownedSeqs != nil && seq > s.ownedSeqs[z] {
		s.ownedSeqs[z] = seq
	}
	s.ownedSeqsMu.Unlock()
}

// OwnedSeqs returns the durable sequence number of every non-empty zone this
// disk currently owns. Results are cached per causal view digest and updated
// incrementally on applyWrite so periodic replication does not scan all zones
// under rowsMu.
func (s *Store) OwnedSeqs() ([]ZoneSeq, error) {
	routing, err := s.CausalRouting()
	if err != nil {
		return nil, err
	}
	ver := routing.Digest()
	if snapshot := s.committedCausal(); snapshot != nil {
		ver = causal.Hash(snapshot.version)
	}
	s.ownedSeqsMu.Lock()
	if s.ownedSeqs != nil && s.ownedSeqsVer == ver {
		out := make([]ZoneSeq, 0, len(s.ownedSeqs))
		for z, seq := range s.ownedSeqs {
			out = append(out, ZoneSeq{Zone: z, Seq: seq})
		}
		s.ownedSeqsMu.Unlock()
		return out, nil
	}
	s.ownedSeqsMu.Unlock()

	scanned := map[tree.ZoneKey]uint64{}
	err = s.view(func(tx *Tx) error {
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		cached, err := s.causalSnapshot(tx)
		if err != nil {
			return err
		}
		_ = state
		return s.eachInventoryZone(context.Background(), tx, nil, 0, nil, rowsMeta, func(z tree.ZoneKey, _ []*row) error {
			if err := s.writableIn(tx, cached, z); err != nil {
				return nil
			}
			if seq := liveZone(z).seq(tx); seq > 0 {
				scanned[z] = seq
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	s.ownedSeqsMu.Lock()
	s.ownedSeqs = scanned
	s.ownedSeqsVer = ver
	out := make([]ZoneSeq, 0, len(scanned))
	for z, seq := range scanned {
		out = append(out, ZoneSeq{Zone: z, Seq: seq})
	}
	s.ownedSeqsMu.Unlock()
	return out, nil
}

func (s *Store) validShadowOwner(owner causal.Endpoint) (string, error) {
	if !owner.Valid() || owner.Storage == "" || owner.VNode == s.node {
		return "", ErrConflict
	}
	id, err := s.Identity()
	if err != nil {
		return "", err
	}
	if owner.Storage == id {
		return "", ErrConflict
	}
	return shadowSession(owner.Storage), nil
}

func (s *Store) checkShadowNotOwned(z tree.ZoneKey, routing RoutingSnapshot) error {
	holder, err := routing.Route(z.ID)
	if err != nil {
		return err
	}
	if holder.VNode == s.node {
		return ErrConflict
	}
	return nil
}

// PreloadShadow stages initial zone snapshots from owner into the isolated
// shadow store without touching the live write/rows lock. It refuses zones
// owned by this disk and preserves any existing mirror progress for
// already-staged zones.
func (s *Store) PreloadShadow(owner causal.Endpoint, snapshots []Snapshot) (map[string]uint64, error) {
	txName, err := s.validShadowOwner(owner)
	if err != nil {
		return nil, err
	}
	state, err := s.CausalState()
	if err != nil {
		return nil, err
	}
	if err := s.peek(func(tx *Tx) error {
		if receiverClosed(tx, state) {
			return ErrNotReady
		}
		return nil
	}); err != nil {
		return nil, err
	}
	routing, err := s.CausalRouting()
	if err != nil {
		return nil, err
	}
	for _, snap := range snapshots {
		if err := s.checkShadowNotOwned(snap.Zone, routing); err != nil {
			return nil, err
		}
	}
	progress := make(map[string]uint64, len(snapshots))
	err = s.shadowUpdate(func(tx *Tx) error {
		if err := tx.put(stagingRows(txName), nil); err != nil {
			return err
		}
		for _, snap := range snapshots {
			t, staged, err := stageOnce(tx, txName, owner.Storage, snap)
			switch {
			case err != nil:
				return err
			case staged:
				progress[sealKey(snap.Zone)] = snap.Seq
			default:
				progress[sealKey(snap.Zone)] = t.seq(tx)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return progress, nil
}

// MirrorShadow appends contiguous ordered mutations from owner into its
// isolated shadow session without contending on the live write/rows lock.
func (s *Store) MirrorShadow(owner causal.Endpoint, batch []ZoneMutations) error {
	txName, err := s.validShadowOwner(owner)
	if err != nil {
		return err
	}
	routing, err := s.CausalRouting()
	if err != nil {
		return err
	}
	for _, part := range batch {
		if err := s.checkShadowNotOwned(part.Zone, routing); err != nil {
			return err
		}
	}
	return s.shadowUpdate(func(tx *Tx) error {
		for _, part := range batch {
			if err := mirrorZone(tx, txName, part.Zone, part.Mutations); err != nil {
				return err
			}
		}
		return nil
	})
}

// ShadowSeqs returns the durable staged sequence of each requested zone held
// in owner's shadow session. Zones not staged are omitted.
func (s *Store) ShadowSeqs(owner causal.Endpoint, zones []tree.ZoneKey) (map[string]uint64, error) {
	txName, err := s.validShadowOwner(owner)
	if err != nil {
		return nil, err
	}
	out := make(map[string]uint64, len(zones))
	err = s.shadowView(func(tx *Tx) error {
		for _, z := range zones {
			t, header, err := stagedZone(tx, txName, z)
			if err == ErrNotReady {
				continue
			}
			if err != nil {
				return err
			}
			if header.Source == owner.Storage {
				out[sealKey(z)] = t.seq(tx)
			}
		}
		return nil
	})
	return out, err
}

// ShadowSeq returns the durable staged sequence of one zone in owner's shadow
// session.
func (s *Store) ShadowSeq(owner causal.Endpoint, z tree.ZoneKey) (uint64, bool, error) {
	txName, err := s.validShadowOwner(owner)
	if err != nil {
		return 0, false, err
	}
	var seq uint64
	var ok bool
	err = s.shadowView(func(tx *Tx) error {
		t, header, err := stagedZone(tx, txName, z)
		if err == ErrNotReady {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Source == owner.Storage {
			seq = t.seq(tx)
			ok = true
		}
		return nil
	})
	return seq, ok, err
}
