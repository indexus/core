package storage

import (
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/tree"
)

var pendingSpawnsKey = []byte("pending-spawns")

// A reservation is advisory capacity at a point, not an exact future identity.
// A fresh nearby vnode satisfies it when placement at that point changes.
// This never grants or revokes a write right. Development disks use this new
// metadata shape directly; there is no migration path for the previous shape.
type pendingSpawn struct {
	Target tree.VNodeDescriptor
	Before tree.ID
}

func (p pendingSpawn) waiting(view *tree.View) bool {
	owner := view.Owner(p.Target.ID)
	return owner.IsZero() || owner == p.Before
}

func readPendingSpawns(tx *Tx) ([]pendingSpawn, error) {
	var pending []pendingSpawn
	if raw := tx.value(meta(pendingSpawnsKey)); raw != nil {
		if err := json.Unmarshal(raw, &pending); err != nil {
			return nil, err
		}
	}
	for _, p := range pending {
		if p.Target.Name == "" || p.Target.ID.IsZero() || p.Before.IsZero() {
			return nil, ErrUnsupportedFormat
		}
	}
	return pending, nil
}

func (s *Store) ReserveSpawns(vnodes []tree.VNodeDescriptor) error {
	if len(vnodes) == 0 {
		return nil
	}
	return s.update(func(tx *Tx) error {
		pending, err := readPendingSpawns(tx)
		if err != nil {
			return err
		}
		view, err := dataView(tx)
		if err != nil {
			return err
		}
		for _, v := range vnodes {
			if v.Name == "" || v.ID.IsZero() {
				return ErrConflict
			}
			found := false
			for _, old := range pending {
				if old.Target.Name == v.Name {
					if old.Target != v {
						return ErrConflict
					}
					found = true
				}
			}
			if !found && !view.HasName(v.Name) {
				pending = append(pending, pendingSpawn{Target: v, Before: view.Owner(v.ID)})
			}
		}
		return tx.putJSON(meta(pendingSpawnsKey), pending)
	})
}

func (s *Store) PendingSpawnNames() ([]string, error) {
	return read(s, func(tx *Tx) ([]string, error) {
		pending, err := readPendingSpawns(tx)
		if err != nil || len(pending) == 0 {
			return nil, err
		}
		view, err := dataView(tx)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, p := range pending {
			if p.waiting(view) {
				names = append(names, p.Target.Name)
			}
		}
		return names, nil
	})
}

func prunePendingSpawns(tx *Tx, view *tree.View) error {
	pending, err := readPendingSpawns(tx)
	if err != nil {
		return err
	}
	next := pending[:0]
	for _, p := range pending {
		if p.waiting(view) {
			next = append(next, p)
		}
	}
	if len(next) == len(pending) {
		return nil
	}
	if len(next) == 0 {
		return tx.del(meta(pendingSpawnsKey))
	}
	return tx.putJSON(meta(pendingSpawnsKey), next)
}
