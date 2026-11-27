package storage

import (
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
	"strings"
)

var zoneTransfersBucket = []byte("zone-transfers")

func zoneTransfersRows(z tree.ZoneKey) string {
	return key(metaBucket, zoneTransfersBucket, zoneBytes(z))
}
func zoneTransferPath(z tree.ZoneKey, id causal.TransferID) string {
	return zoneTransfersRows(z) + element([]byte(id.String()))
}

// zoneTransferOf is the proof of z's transfer in operation id: from the lot
// row its state names, or from its own row on a store fenced before lot rows.
func zoneTransferOf(tx *Tx, state CausalState, id causal.TransferID, z tree.ZoneKey) (causal.ZoneTransfer, bool, error) {
	var p causal.ZoneTransfer
	st, _, e := zoneStateOf(tx, state, id, z)
	if e != nil {
		return p, false, e
	}
	if st.Lot != 0 {
		return lotTransfer(tx, state, id, st.Lot, z)
	}
	raw := tx.value(zoneTransferPath(z, id))
	if raw == nil {
		return p, false, nil
	}
	e = json.Unmarshal(raw, &p)
	return p, e == nil, e
}

// zoneTransfersOf are the proofs of z's transfers: the rows of its own, then
// those its lots hold, in operation order.
func (s *Store) zoneTransfersOf(tx *Tx, z tree.ZoneKey) ([]causal.ZoneTransfer, error) {
	var out []causal.ZoneTransfer
	var failed error
	e := tx.each(zoneTransfersRows(z), "", func(_ string, raw []byte) bool {
		var p causal.ZoneTransfer
		failed = json.Unmarshal(raw, &p)
		out = append(out, p)
		return failed == nil
	})
	if e = errors.Join(e, failed); e != nil {
		return nil, e
	}
	c, e := s.causalSnapshot(tx)
	if e != nil {
		return nil, e
	}
	var ids []causal.TransferID
	for id, t := range c.state.Transfers {
		if t.Cession.Region.Contains(z.ID) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b causal.TransferID) int { return strings.Compare(a.String(), b.String()) })
	for _, id := range ids {
		st, _, e := zoneStateOf(tx, c.state, id, z)
		if e != nil {
			return nil, e
		}
		if st.Lot == 0 {
			continue
		}
		p, ok, e := lotTransfer(tx, c.state, id, st.Lot, z)
		if e != nil {
			return nil, e
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}
func (s *Store) ZoneTransfers(z tree.ZoneKey) ([]causal.ZoneTransfer, error) {
	return read(s, func(tx *Tx) ([]causal.ZoneTransfer, error) { return s.zoneTransfersOf(tx, z) })
}
func (s *Store) ZoneHolder(z tree.ZoneKey) (causal.Endpoint, error) {
	return read(s, func(tx *Tx) (causal.Endpoint, error) {
		c, e := s.causalSnapshot(tx)
		if e != nil {
			return causal.Endpoint{}, e
		}
		permission, installed, e := s.installedPermission(tx, c, z)
		if e != nil {
			return causal.Endpoint{}, e
		}
		if e = s.writableIn(tx, c, z); e == nil {
			host := localHost(c)
			if contact := s.contactHost.Load(); contact != nil {
				host = *contact
			}
			return causal.Endpoint{Owner: causal.Owner{VNode: s.node, Storage: c.storageID}, Host: host, Epoch: c.state.Permissions.EndpointEpoch}, nil
		}
		// A still installed parent may be handing this point out. Match its exact
		// identity; an older issued operation cannot redirect a newly acquired right.
		if installed {
			for id, t := range c.state.Transfers {
				if t.Cession.Source.Storage != c.storageID || t.Phase >= causal.TransferInstalled || t.Cession.Input.ID != permission || !t.Cession.Region.Contains(z.ID) {
					continue
				}
				st, _, e := zoneStateOf(tx, c.state, id, z)
				if e != nil {
					return causal.Endpoint{}, e
				}
				if st.Transferred || t.Phase >= causal.TransferIssued {
					return c.membership.Resolve(t.Cession.Target)
				}
			}
		}
		delegations, e := readDelegations(tx)
		if e != nil {
			return causal.Endpoint{}, e
		}
		for _, d := range delegations {
			if d.Region.Contains(z.ID) {
				return c.membership.Resolve(d.Holder)
			}
		}
		// Until the native right is installed, empty addresses still belong to
		// the donor. Point installations were handled above; membership alone
		// must not turn the incoming receiver into a routing dead end.
		for _, t := range c.state.Transfers {
			if t.Cession.Target.Storage == c.storageID && t.Phase < causal.TransferInstalled && t.Cession.Region.Contains(z.ID) {
				return c.membership.Resolve(t.Cession.Source)
			}
		}
		return c.membership.Route(z.ID)
	})
}
func localHost(c *causalSnapshot) string {
	for _, t := range c.state.Transfers {
		if t.Cession.Target.Storage == c.storageID {
			return t.Cession.Target.Host
		}
		if t.Cession.Source.Storage == c.storageID {
			return t.Cession.Source.Host
		}
	}
	if c.state.Genesis.Holder.Storage == c.storageID {
		return c.state.Genesis.Holder.Host
	}
	for _, r := range c.state.Membership.Members {
		if r.Endpoint.Storage == c.storageID {
			return r.Endpoint.Host
		}
	}
	return ""
}
