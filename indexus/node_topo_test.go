package indexus

import (
	"context"
	"testing"
)

func TestNODETOPO1_RegistryDoesNotChangeOwner(t *testing.T) {
	t.Parallel()
	aName, bName := mustName(t), mustName(t)
	aID, _ := BASE64.Decode(aName)
	bID, _ := BASE64.Decode(bName)
	f := newReadPhaseFixture(t, []VNodeDescriptor{{Name: aName, ID: FromSlice(aID)}, {Name: bName, ID: FromSlice(bID)}}, false, false)
	if err := f.nodes[1].JoinCausalCluster(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes[0].Refresh(); err != nil {
		t.Fatal(err)
	}
	a := f.nodes[0]
	active := a.topologyView()

	var noise []Contact
	for _, ip := range []string{"10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"} {
		noise = append(noise, newDialPeer(mustName(t), ip, 22003))
	}
	a.register(noise)
	a.subscribe(noise)
	a.acknowledge(noise)

	locs := []string{BASE64.Root(), "a", "ab", "abc", "z", "0"}
	before := map[string]ID{}
	for _, loc := range locs {
		id, err := ZoneID("Demo000000000000", loc)
		if err != nil {
			t.Fatal(err)
		}
		got := a.topologyView().Owner(id)
		want := active.Owner(id)
		if got != want {
			t.Fatalf("loc %s owner %s want %s", loc, got, want)
		}
		before[loc] = got
	}

	a.register([]Contact{newDialPeer(mustName(t), "10.1.0.1", 9)})
	a.reject(noise)
	a.subscribe([]Contact{newDialPeer(mustName(t), "10.1.0.2", 9)})
	for _, loc := range locs {
		id, err := ZoneID("Demo000000000000", loc)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.topologyView().Owner(id); got != before[loc] {
			t.Fatalf("loc %s owner changed after registry edits", loc)
		}
	}
	if a.topologyView().Len() != 2 {
		t.Fatalf("V_active len=%d", a.topologyView().Len())
	}
}

func mustName(t *testing.T) string {
	t.Helper()
	name, err := BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	return name
}
