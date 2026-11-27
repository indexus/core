package mesh

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// inventoryCommitContext advances the transfer after the inventory's MVCC
// snapshot has been pinned and before rows are scanned. The handler/page/control
// checks provide the deterministic cut; control and referrals still read that
// older transaction even though completion commits on the live store.
type inventoryCommitContext struct {
	context.Context
	checks int
	commit func() error
	err    error
}

func (c *inventoryCommitContext) Err() error {
	c.checks++
	if c.checks == 3 {
		c.err = c.commit()
	}
	if c.err != nil {
		return c.err
	}
	return c.Context.Err()
}

type inventoryCommitTransport struct {
	Transport
	source *Service
	commit func() error
	cut    atomic.Bool
}

func (p *inventoryCommitTransport) Exchange(ctx context.Context, host string, request Request) (Response, error) {
	if host == p.source.Self.Host && request.Kind == KindInventoryPrefix && !p.cut.Load() {
		// Membership is a separate control RPC; the data snapshot needs no
		// global metadata preparation before this deterministic commit cut.
		if !p.cut.CompareAndSwap(false, true) {
			return p.Transport.Exchange(ctx, host, request)
		}
		cut := &inventoryCommitContext{Context: ctx, commit: p.commit}
		out, err := p.source.Handle(cut, request)
		if err == nil {
			revision, e := p.source.Store.Revision()
			if e != nil {
				return Response{}, e
			}
			if cut.checks < 3 || out.Page == nil || out.Page.Watermark >= revision {
				return Response{}, fmt.Errorf("transfer did not complete inside the older inventory snapshot")
			}
		}
		return out, err
	}
	return p.Transport.Exchange(ctx, host, request)
}

func TestQueryKeepsEarlierACKWhenTransferCompletesDuringSourceInventory(t *testing.T) {
	t.Parallel()
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("receiver-unavailable=%t", unavailable), func(t *testing.T) {
			net, nodes := meshFixture(t, 3)
			source, target, reader := nodes[0], nodes[1], nodes[2]
			ctx := context.Background()
			const collection = "Demo000000000000"
			zone, _ := tree.NewZoneKey(collection, "aa")
			target.VNode.ID = zone.ID
			seed := storage.Record{Collection: collection, Location: "aa", ID: "seed", Metrics: []float64{1}}
			if _, _, _, err := source.Store.ApplyRecord("seed", seed); err != nil {
				t.Fatal(err)
			}
			var wrote, queried bool
			net.setAfter(func(request Request) error {
				if request.Kind == "install-zone" && !wrote {
					wrote = true
					row := seed
					row.ID = "acked-on-receiver"
					_, err := target.Handle(ctx, Request{Kind: "write", RequestID: row.ID, Record: &row})
					return err
				}
				return nil
			})
			net.setBefore(func(request Request) error {
				if request.Kind != "install" || queried {
					return nil
				}
				queried = true
				if !wrote {
					return fmt.Errorf("receiver never acknowledged a granular write")
				}
				cession := *request.Cession
				transport := &inventoryCommitTransport{Transport: net, source: source}
				transport.commit = func() error {
					if err := target.Store.InstallCausal(cession); err != nil {
						return err
					}
					if err := source.Store.CompleteCausal(cession.ID); err != nil {
						return err
					}
					if unavailable {
						net.setBlocked(target.Self.Host, true)
					}
					return nil
				}
				reader.Peers, target.Peers = transport, transport
				rows, err := reader.QueryPrefixMetrics(ctx, collection, "@")
				net.setBlocked(target.Self.Host, false)
				if !transport.cut.Load() {
					return fmt.Errorf("source inventory cut was not exercised")
				}
				if unavailable {
					if err == nil || err.Error() != "partition" || rows != nil {
						return fmt.Errorf("unavailable receiver returned partial success or wrong failure: rows=%d err=%v", len(rows), err)
					}
					return nil
				}
				if err != nil {
					return err
				}
				count := 0
				for _, snap := range rows {
					count += len(snap.Records)
				}
				if count != 2 {
					return fmt.Errorf("successful read lost receiver ACK: got %d records, want 2", count)
				}
				return nil
			})
			state, err := source.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			if err := source.handoff(ctx, Request{Input: state.Permissions.Native[0].ID, Target: target.Self, TargetVNode: target.VNode, Region: tree.Region{}}); err != nil {
				t.Fatal(err)
			}
			if !queried {
				t.Fatal("regional installation was not exercised")
			}
		})
	}
}
