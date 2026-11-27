package indexus

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// ErrQueryBudget reports a record read larger than its record or byte budget,
// so adapters can answer 413 without importing the mesh.
var ErrQueryBudget = mesh.ErrQueryBudget

// GetRecordsContext reads canonical user records, including optional opaque
// content. Aggregate /set reads deliberately continue to expose metrics only.
func (n *Node) GetRecordsContext(ctx context.Context, collection, prefix string, limit int) ([]Item, error) {
	if n.mesh == nil {
		return nil, ErrNotReady
	}
	if limit < 1 || limit > wire.MaxContentRecords {
		return nil, fmt.Errorf("invalid record budget")
	}
	if err := checkKey(&Item{Collection: collection, Location: prefix}, BASE64.Root()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case n.mesh.readSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-n.mesh.readSlots }()
	inventory, err := n.mesh.service.QueryPrefixLimited(ctx, collection, prefix, limit, wire.MaxContentBytes)
	if err != nil {
		return nil, err
	}
	type recordKey struct{ location, id string }
	rows := make(map[recordKey]Item)
	for _, snap := range inventory {
		for _, r := range snap.Records {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if r.Deleted || r.Collection != collection || (prefix != "@" && !strings.HasPrefix(r.Location, prefix)) {
				continue
			}
			rows[recordKey{location: r.Location, id: r.ID}] = Item{Collection: r.Collection, Location: r.Location, Id: r.ID, Metrics: r.Metrics, Payload: r.Payload, Weight: r.Weight, Binary: r.Binary}
			if len(rows) > limit {
				return nil, fmt.Errorf("record budget exceeded")
			}
		}
	}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Location != out[j].Location {
			return out[i].Location < out[j].Location
		}
		return out[i].Id < out[j].Id
	})
	return out, nil
}
