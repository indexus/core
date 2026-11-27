package node

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

type clusterBoot struct {
	config     Config
	mesh       *mesh.Service
	endpoint   string
	bootstraps []string
	store      *storage.Store
	client     *meshhttp.Client
	ready      atomic.Bool
}

func (b *clusterBoot) Start(ctx context.Context, node *indexus.Node) error {
	if b == nil {
		return nil
	}
	return b.startMesh(ctx, node)
}

// Open enables the vnode without joining (see openMesh).
func (b *clusterBoot) Open(ctx context.Context, node *indexus.Node) error {
	if b == nil {
		return nil
	}
	return b.openMesh(ctx, node)
}

// Join acquires the vnode's cell after Open. The node reports Joining until
// it returns nil; a join that ran out of time is finished by keepJoining.
func (b *clusterBoot) Join(ctx context.Context, node *indexus.Node) error {
	if b == nil {
		return nil
	}
	node.SetJoining(true)
	err := b.joinMesh(ctx, node)
	if err == nil {
		node.SetJoining(false)
	}
	return err
}

// hasRight reports that the vnode holds an installed right it could itself
// delegate: the first grain of its join, or what it held before a restart.
func (b *clusterBoot) hasRight() bool {
	if b == nil || b.store == nil {
		return false
	}
	held, err := b.store.HasDelegatableRight()
	return err == nil && held
}
func (b *clusterBoot) Close() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.mesh != nil {
		b.mesh.Close()
	}
	return b.store.Close()
}
func splitValues(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}
