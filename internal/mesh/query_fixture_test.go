package mesh

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Query fixtures retain the targeted-holder entry point while production
// discovers holders through region coverage.
func (s *Service) queryHolders(ctx context.Context, holders []causal.Endpoint, r Request) ([]storage.Snapshot, error) {
	return s.queryHoldersBudget(ctx, holders, r, nil)
}
func (s *Service) queryHoldersBudget(ctx context.Context, holders []causal.Endpoint, r Request, b *queryBudget) ([]storage.Snapshot, error) {
	region := tree.Region{}
	if r.Kind == KindInventoryPrefix {
		region = tree.PrefixRegion(r.Zone)
	}
	return s.queryCoverage(ctx, holders, nil, region, r, b)
}
