package storage

import (
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
)

type RegionReferral struct {
	Region   tree.Region
	Endpoint causal.Endpoint
}

// A point permission is canonical-zone specific. Geometric IDs may alias
// ancestors, so only an exact maximum-depth query can claim its whole address
// scope without asking the incoming source about other canonical zones.
func installedExactPoint(tx *Tx, c *causalSnapshot, query *tree.ZoneKey) (bool, error) {
	if query == nil || inventoryQueryRegion(query).Mask != tree.FullMask {
		return false, nil
	}
	for id, t := range c.state.Transfers {
		if t.Cession.Target.Storage != c.storageID || t.Phase < causal.TransferMoving || t.Phase >= causal.TransferInstalled || !t.Cession.Region.Contains(query.ID) {
			continue
		}
		st, _, e := zoneStateOf(tx, c.state, id, *query)
		if e != nil {
			return false, e
		}
		if st.Installed {
			return true, nil
		}
	}
	return false, nil
}
func inventoryCoverage(tx *Tx, c *causalSnapshot, region tree.Region, query *tree.ZoneKey) ([]tree.Region, error) {
	var out []tree.Region
	for _, r := range c.state.Permissions.Native {
		if piece, ok := r.Region.Intersect(region); ok {
			out = append(out, piece)
		}
	}
	for _, t := range c.state.Transfers {
		if t.Cession.Source.Storage == c.storageID && t.Phase < causal.TransferInstalled {
			if piece, ok := t.Cession.Region.Intersect(region); ok {
				out = append(out, piece)
			}
		}
	}
	exact, e := installedExactPoint(tx, c, query)
	if e != nil {
		return nil, e
	}
	if exact {
		out = append(out, tree.Point(query.ID))
	}
	return out, nil
}
func subtractScopes(scope tree.Region, covered []tree.Region) []tree.Region {
	parts := []tree.Region{scope}
	for _, cover := range covered {
		var next []tree.Region
		for _, part := range parts {
			next = append(next, part.Subtract(cover)...)
		}
		parts = next
	}
	return parts
}

// inventoryReferrals lists, for the part of region this disk does not hold,
// the disks a reader must also ask: the holders its read directory names,
// and the targets of the zones its unfinished transfers already moved.
func inventoryReferrals(tx *Tx, c *causalSnapshot, region tree.Region, query *tree.ZoneKey) ([]RegionReferral, error) {
	exact, err := installedExactPoint(tx, c, query)
	if err != nil {
		return nil, err
	}
	refs := referrals{c: c, region: region}
	if err = refs.addDelegations(tx, query, exact); err != nil {
		return nil, err
	}
	for id, t := range c.state.Transfers {
		if t.Cession.Source.Storage != c.storageID || t.Phase >= causal.TransferIssued {
			continue
		}
		if err = refs.addMovedZones(tx, id, t, query, exact); err != nil {
			return nil, err
		}
	}
	return refs.out, nil
}

// referrals collects distinct (region, endpoint) pairs inside one region.
type referrals struct {
	c      *causalSnapshot
	region tree.Region
	out    []RegionReferral
}

// add refers the part of scope inside the region to h, once.
func (r *referrals) add(scope tree.Region, h causal.Endpoint) error {
	piece, ok := scope.Intersect(r.region)
	if !ok {
		return nil
	}
	h, err := r.c.membership.Resolve(h)
	if err != nil {
		return err
	}
	ref := RegionReferral{Region: piece, Endpoint: h}
	if !slices.Contains(r.out, ref) {
		r.out = append(r.out, ref)
	}
	return nil
}

// addDelegations refers what the read directory delegates, less what this
// disk holds itself. The current directory is the ONLY regional read
// obligation: first confirmed preload points at its source; first emission
// at its target; native installation clears it. Completed/history operation
// records are not routing state and cannot resurrect an obsolete mandatory
// holder.
func (r *referrals) addDelegations(tx *Tx, query *tree.ZoneKey, exact bool) error {
	var installed []tree.Region
	for _, right := range r.c.state.Permissions.Native {
		installed = append(installed, right.Region)
	}
	if exact {
		installed = append(installed, tree.Point(query.ID))
	}
	delegations, err := readDelegations(tx)
	if err != nil {
		return err
	}
	for _, d := range delegations {
		for _, part := range subtractScopes(d.Region, installed) {
			if err = r.add(part, d.Holder); err != nil {
				return err
			}
		}
	}
	return nil
}

// addMovedZones refers to the target of transfer t each zone of the query
// it already moved there, less what this disk holds by another right.
func (r *referrals) addMovedZones(tx *Tx, id causal.TransferID, t CausalTransfer, query *tree.ZoneKey, exact bool) error {
	var covered []tree.Region
	for _, right := range r.c.state.Permissions.Native {
		if right.ID != t.Cession.Input.ID {
			covered = append(covered, right.Region)
		}
	}
	if exact {
		covered = append(covered, tree.Point(query.ID))
	}
	var failed error
	err := eachZoneState(tx, r.c.state, id, func(st ZoneState) bool {
		if !st.Transferred || !containsInventoryQuery(query, st.Zone) {
			return true
		}
		for _, part := range subtractScopes(tree.Point(st.Zone.ID), covered) {
			if failed = r.add(part, t.Cession.Target); failed != nil {
				return false
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	return failed
}
