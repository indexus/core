package mesh

import (
	"context"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A machine agent can stop an unneeded join and start another vnode at exactly
// the same address while its donor has a durable offer waiting for a copy slot.
func TestResumeAfterReceiverAddressReuse(t *testing.T) {
	for _, phase := range []string{"before-admission", "after-issue"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			net, nodes := meshFixture(t, 3)
			source, original, replacement := nodes[0], nodes[1], nodes[2]
			ctx := context.Background()
			rights, err := source.Store.Rights(tree.Region{})
			if err != nil || len(rights) != 1 {
				t.Fatalf("genesis rights %v: %v", rights, err)
			}
			right := rights[0]
			var op causal.TransferID
			if phase == "before-admission" {
				offer, err := source.Store.BeginCausal(ctx, right.ID, original.Self, right.Region)
				if err != nil {
					t.Fatal(err)
				}
				op = offer.Cession.ID
			} else {
				offer := confirmedTransfer(t, source, original, right.ID, right.Region)
				op = offer.Cession.ID
				if _, err := source.Store.ForceFreezeRegion(op); err != nil {
					t.Fatal(err)
				}
				sealed, err := source.Store.SealCausalRegion(op)
				if err != nil {
					t.Fatal(err)
				}
				if err := original.Store.StageCausal(sealed); err != nil {
					t.Fatal(err)
				}
				if _, err := source.Store.EmitCausal(op); err != nil {
					t.Fatal(err)
				}
			}
			net.mu.Lock()
			net.nodes[original.Self.Host] = replacement
			net.mu.Unlock()

			resumeErr := source.Resume(ctx)
			state, err := source.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "probe"}
			if _, err := replacement.Store.Write(record.ID, record); err == nil {
				t.Fatal("new occupant acquired authority from the address")
			}
			pending, open := state.Transfers[op]
			if phase == "before-admission" {
				if resumeErr != nil || open {
					t.Fatalf("obsolete offer still open=%t after resume: %v", open, resumeErr)
				}
				if _, err := source.Store.Write(record.ID, record); err != nil {
					t.Fatalf("source lost the right before admission: %v", err)
				}
				return
			}
			if resumeErr == nil || !open || pending.Phase != causal.TransferIssued || pending.Cession.Target.Owner != original.Self.Owner {
				t.Fatalf("issued decision changed: open=%t phase=%v target=%v err=%v", open, pending.Phase, pending.Cession.Target, resumeErr)
			}
			if _, err := source.Store.Write(record.ID, record); err == nil {
				t.Fatal("source reclaimed an issued right")
			}
			net.mu.Lock()
			net.nodes[original.Self.Host] = original
			net.mu.Unlock()
			if err := source.Resume(ctx); err != nil {
				t.Fatalf("resume with original disk: %v", err)
			}
			if _, err := original.Store.Write(record.ID, record); err != nil {
				t.Fatalf("original disk failed to install: %v", err)
			}
		})
	}
}
