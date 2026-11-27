package summary

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type cancelPollTransport struct {
	cancel    context.CancelFunc
	deadline  time.Time
	calls     int
	inherited bool
	cancelled bool
	seen      map[string]int
}

func (p *cancelPollTransport) Exchange(ctx context.Context, host string, _ mesh.Request) (mesh.Response, error) {
	p.calls++
	if p.seen != nil {
		p.seen[host]++
	}
	deadline, ok := ctx.Deadline()
	p.inherited = ok && deadline.Equal(p.deadline)
	if p.cancel != nil {
		p.cancel()
		p.cancelled = errors.Is(ctx.Err(), context.Canceled)
	}
	// Even a transport returning a successful late response cannot turn the
	// caller's cancelled reconciliation into success.
	return mesh.Response{}, nil
}

func TestPollHostInheritsCallerBudgetAndRejectsLateSuccess(t *testing.T) {
	// The caller's deadline is earlier than the RPC's own ten-second cap.
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	deadline, _ := ctx.Deadline()
	peers := &cancelPollTransport{cancel: stop, deadline: deadline}
	mark := New(&mesh.Service{Peers: peers}, 1, Hooks{})
	if err := mark.PollHost(ctx, "unavailable"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled poll returned %v", err)
	}
	if peers.calls != 1 || !peers.inherited || !peers.cancelled {
		t.Fatalf("poll detached its caller: %+v", peers)
	}
}

func TestPollRemoteHostsStopsAfterCallerCancellation(t *testing.T) {
	parent := describedService(t, "poll-parent", nil)
	initCluster(t, parent)
	var members []causal.Member
	for i, name := range []string{"poll-child-a", "poll-child-b"} {
		child := describedService(t, name, nil)
		child.VNode.ID = tree.ID{byte(i + 2)}
		members = append(members, causal.Member{VNode: child.VNode, Endpoint: child.Self})
	}
	if err := parent.Store.MergeMembership(causal.MembershipDelta{Members: members}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	peers := &cancelPollTransport{cancel: cancel, deadline: deadline}
	parent.Peers = peers
	mark := New(parent, 1, Hooks{})
	if err := mark.PollRemoteHosts(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reconciliation returned %v", err)
	}
	if peers.calls != 1 {
		t.Fatalf("continued polling after cancellation: %d RPCs", peers.calls)
	}
}

// With 63 hosts and a six-host steady window, advancing the entire planned
// window while only its first RPC completes visits just 21 hosts forever.
func TestCancelledPollRoundsDoNotStarveTheirUnattemptedHosts(t *testing.T) {
	parent := describedService(t, "rotation-parent", nil)
	initCluster(t, parent)
	const remotes = 63
	members := make([]causal.Member, 0, remotes)
	for i := range remotes {
		name := fmt.Sprintf("rotation-child-%02d", i)
		members = append(members, causal.Member{
			VNode:    tree.VNodeDescriptor{Name: name, ID: tree.ID{128, byte(i + 1)}},
			Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name + "-disk"}, Host: "http://" + name},
		})
	}
	if err := parent.Store.MergeMembership(causal.MembershipDelta{Members: members}); err != nil {
		t.Fatal(err)
	}
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint("all=", all), func(t *testing.T) {
			peers := &cancelPollTransport{seen: map[string]int{}}
			parent.Peers = peers
			mark := New(parent, 1, Hooks{})
			for range remotes {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				peers.cancel = cancel
				peers.deadline, _ = ctx.Deadline()
				err := mark.PollRemoteHosts(ctx, all)
				cancel()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled round returned %v", err)
				}
			}
			if len(peers.seen) != remotes || peers.calls != remotes {
				t.Fatalf("partial rounds reached %d/%d hosts in %d RPCs", len(peers.seen), remotes, peers.calls)
			}
			peers.cancel = nil
			peers.calls = 0
			peers.seen = map[string]int{}
			if err := mark.PollRemoteHosts(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(peers.seen) != remotes || peers.calls != remotes {
				t.Fatalf("healthy full round reached %d/%d hosts in %d RPCs", len(peers.seen), remotes, peers.calls)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			before := mark.pollRound.Load()
			if err := mark.PollRemoteHosts(ctx, all); !errors.Is(err, context.Canceled) {
				t.Fatalf("already cancelled round returned %v", err)
			}
			if mark.pollRound.Load() != before || peers.calls != remotes {
				t.Fatal("an already cancelled round advanced or sent a request")
			}
		})
	}
}
