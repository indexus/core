package indexus

import (
	"context"
	"time"
)

// GetLocalChildrenContext returns immediate addresses suggested by this peer's
// bounded local storage probes. During handoff, even a reported child can be
// a stale copy. Omitted children must remain eligible for exact traversal.
func (n *Node) GetLocalChildrenContext(ctx context.Context, collection, location string) ([]string, error) {
	if err := checkKey(&Item{Collection: collection, Location: location}, BASE64.Root()); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n.mesh == nil {
		return nil, ErrNotReady
	}
	if len(location) == BASE64.IDLength() {
		return []string{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case n.mesh.readSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-n.mesh.readSlots }()
	seen, _, err := n.mesh.service.Store.ObserveChildPresence(ctx, collection, location)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	children := make([]string, 0)
	if seen != 0 {
		for i, child := range ChildCandidates(BASE64, location) {
			if seen&(uint64(1)<<i) != 0 {
				children = append(children, child)
			}
		}
	}
	return children, nil
}
