package storage

import (
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func (s *Store) causalWritable(tx *Tx, z tree.ZoneKey) error {
	c, e := s.causalSnapshot(tx)
	if e != nil {
		return e
	}
	return s.writableIn(tx, c, z)
}

// installedPermission identifies the CURRENT local permission, including an
// installed point before its native region is consolidated. Historical source
// fences can constrain only the exact input identity they consumed.
func (s *Store) installedPermission(tx *Tx, c *causalSnapshot, z tree.ZoneKey) (causal.Digest, bool, error) {
	self := causal.Owner{VNode: s.node, Storage: c.storageID}
	if r, ok := c.state.Permissions.At(z.ID); ok && r.Owner == self {
		return r.ID, true, nil
	}
	for id, t := range c.state.Transfers {
		if t.Cession.Target.Owner != self || t.Phase < causal.TransferMoving || t.Phase >= causal.TransferInstalled || !t.Cession.Region.Contains(z.ID) {
			continue
		}
		st, _, e := zoneStateOf(tx, c.state, id, z)
		if e != nil {
			return causal.Digest{}, false, e
		}
		if st.Installed {
			return t.Cession.Right().ID, true, nil
		}
	}
	return causal.Digest{}, false, nil
}
func (s *Store) writableIn(tx *Tx, c *causalSnapshot, z tree.ZoneKey) error {
	permission, installed, e := s.installedPermission(tx, c, z)
	if e != nil {
		return e
	}
	if frozen, e := outgoingFrozen(tx, c.state, z, c.storageID, permission); e != nil || frozen {
		return errors.Join(e, ErrFrozen)
	}
	if installed {
		return nil
	}
	return ErrNotOwner
}
func (s *Store) readableIn(tx *Tx, c *causalSnapshot, z tree.ZoneKey) error {
	e := s.writableIn(tx, c, z)
	if errors.Is(e, ErrFrozen) {
		return nil
	}
	for _, t := range c.state.Transfers {
		if t.Cession.Source.Storage == c.storageID && t.Phase < causal.TransferInstalled && t.Cession.Region.Contains(z.ID) {
			return nil
		}
	}
	return e
}
