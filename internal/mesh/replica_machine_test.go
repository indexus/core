package mesh

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestReplicaMachineNames(t *testing.T) {
	for input, want := range map[string]string{
		"https://10.77.0.1:19100":          "10.77.0.1",
		"http://10.77.0.1:19101":           "10.77.0.1",
		"10.77.0.1:19100":                  "10.77.0.1",
		"https://[2001:db8::1]:19100":      "2001:db8::1",
		"https://[2001:db8:0::1]:19101":    "2001:db8::1",
		"[2001:db8::1]:19100":              "2001:db8::1",
		"2001:db8::1":                      "2001:db8::1",
		"https://[::ffff:10.77.0.1]:19102": "10.77.0.1",
		"https://HOST.example.:19103":      "host.example",
		"node-3":                           "node-3",
		"unix:///private/socket":           "",
		"https://[broken":                  "",
		"":                                 "",
	} {
		if got := machineOf(causal.Endpoint{Host: input}); got != want {
			t.Errorf("machine of %q = %q, want %q", input, got, want)
		}
	}
}

func TestReplicatedACKRequiresAnotherMachine(t *testing.T) {
	const sibling = "https://10.77.0.1:19101"
	for _, tc := range []struct {
		name, second string
		endpoint     bool
		want         string
	}{
		{"colocated HTTPS ports", sibling, false, DurabilityLocal},
		{"different IP", "https://10.77.0.2:19100", false, DurabilityReplicated},
		{"explicit endpoint domains", sibling, true, DurabilityReplicated},
		{"unknown URI", "unix:///data/peer2", false, DurabilityLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts := []string{"https://10.77.0.1:19100", tc.second}
			network, nodes := meshFixture(t, len(hosts), hosts...)
			ctx := t.Context()
			if err := nodes[1].Join(ctx); err != nil {
				t.Fatal(err)
			}
			for _, n := range nodes {
				if err := n.Sync(ctx, hosts); err != nil {
					t.Fatal(err)
				}
				if tc.endpoint {
					n.ReplicaMachine = func(e causal.Endpoint) string { return e.Host }
				}
				n.ReplicateWrites = true
			}
			// Exercise both actual owners, including a holder whose own domain is unknown.
			routing, err := nodes[0].Store.CausalRouting()
			if err != nil {
				t.Fatal(err)
			}
			owners := map[string]bool{}
			const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
			for i := range letters {
				r := storage.Record{Collection: "Collection000000", Location: letters[i:i+1] + "a", ID: "row", Metrics: []float64{1}}
				z, err := tree.NewZoneKey(r.Collection, r.Location)
				if err != nil {
					t.Fatal(err)
				}
				e, err := routing.Route(z.ID)
				if err != nil {
					t.Fatal(err)
				}
				if owners[e.Host] {
					continue
				}
				id := fmt.Sprint("w", i)
				out, err := network.nodes[e.Host].Write(ctx, id, r)
				if err != nil || out.Durability != tc.want {
					t.Fatalf("holder %q write got %q %v; want %q", e.Host, out.Durability, err, tc.want)
				}
				owners[e.Host] = true
			}
			if len(owners) != 2 {
				t.Fatalf("exercised owners %v", owners)
			}
		})
	}
}

// After a delegation the vnode closest by XOR can still be the writer's
// sibling. Its shadow must not upgrade the actual writer's durable ACK.
func TestOffPlacementWriterDoesNotCountSiblingAsReplica(t *testing.T) {
	hosts := []string{"https://10.77.0.1:19100", "https://10.77.0.1:19101"}
	nw, nodes := meshFixture(t, len(hosts), hosts...)
	ctx := t.Context()
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range nodes {
		if err := s.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
	r := storage.Record{Collection: "Collection000000", Location: "0a", ID: "delegated", Metrics: []float64{1}}
	z, err := tree.NewZoneKey(r.Collection, r.Location)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := nodes[0].Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	home, err := routing.Route(z.ID)
	if err != nil {
		t.Fatal(err)
	}
	donor := nw.nodes[home.Host]
	receiver := nodes[0]
	if donor == receiver {
		receiver = nodes[1]
	}
	rights, err := donor.Store.Rights(tree.Region{})
	if err != nil {
		t.Fatal(err)
	}
	for _, right := range rights {
		if right.Region.Contains(z.ID) {
			if err := donor.handoff(ctx, Request{Kind: "handoff", Input: right.ID, Region: tree.Region{Mask: tree.ID{255}, Value: tree.ID{z.ID[0]}}, Target: receiver.Self, TargetVNode: receiver.VNode}); err != nil {
				t.Fatal(err)
			}
			view, err := receiver.Store.CausalRouting()
			if err != nil {
				t.Fatal(err)
			}
			if peers := receiver.replicaPeers(view, z); len(peers) != 0 {
				t.Fatalf("colocated targets for installed writer: %v", peers)
			}
			receiver.ReplicateWrites = true
			out, err := receiver.Write(ctx, r.ID, r)
			if err != nil || !out.AppliedLocally || out.Durability != DurabilityLocal {
				t.Fatalf("delegated write on former owner's machine: applied locally %t, durability %q, err %v", out.AppliedLocally, out.Durability, err)
			}
			return
		}
	}
	t.Fatal("nominal owner lacks test zone before delegation")
}
