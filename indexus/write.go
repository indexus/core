package indexus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

var ErrNodeFull = errors.New("node out of memory headroom")

// ErrInvalidKey marks a write the network can never accept as sent: a client
// error, not a retryable refusal.
var ErrInvalidKey = errors.New("invalid key")

func (n *Node) New(item *Item, root string, via Visited) error {
	_, err := n.NewRouted(item, root, via)
	return err
}

// WriteRoute tells a client where a write it sent elsewhere was applied:
// the owner's address and its cell, a region of the key space (EncodeCell).
// A client that writes the next keys of that cell to the owner skips the
// relay (route=direct); one that ignores it keeps writing through any node
// (route=relay). Empty when the write was applied where it was sent.
type WriteRoute struct {
	Owner string
	Cell  string
}

// NewRouted is New, with the route of a relayed write.
func (n *Node) NewRouted(item *Item, root string, via Visited) (WriteRoute, error) {
	if n.mesh == nil {
		return WriteRoute{}, ErrNotReady
	}
	return n.meshWrite(item, root, false)
}

func (n *Node) Delete(item *Item, root string, via Visited) error {
	if n.mesh == nil {
		return ErrNotReady
	}
	_, err := n.meshWrite(item, root, true)
	return err
}

func checkKey(item *Item, root string) error {
	if err := validKey(item, root); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	return nil
}

func validKey(item *Item, root string) error {
	switch {
	case item == nil:
		return errors.New("nil item")
	case item.Location == "":
		return errors.New("item without location")
	case root == "":
		return errors.New("empty root")
	case root != BASE64.Root() && !strings.HasPrefix(item.Location, root):
		return fmt.Errorf("root %q is not an ancestor of %q", root, item.Location)
	case len(item.Collection) > BASE64.IDLength(),
		len(item.Location) > BASE64.IDLength():
		return fmt.Errorf("key %s/%s is wider than an identifier", item.Collection, item.Location)
	}
	_, err := zoneKeyID(item.Collection, item.Location)
	return err
}

func (n *Node) GuardMemory(ctx context.Context) {
	limit := n.settings.memRefusePct
	if limit <= 0 {
		return
	}

	const every = time.Second

	for {
		used := autoscale.MemoryUsedPct()
		if used > 0 {

			if n.full.Load() {
				n.full.Store(used > limit-5)
			} else if used >= limit {
				slog.Warn("refusing writes, out of memory headroom",
					"mem_pct", used, "limit_pct", limit)
				n.full.Store(true)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
