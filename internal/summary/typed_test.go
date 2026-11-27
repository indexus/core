package summary

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/algebra"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// peerTransport delivers a request to the service of its host.
type peerTransport map[string]*mesh.Service

func (p peerTransport) Exchange(ctx context.Context, host string, r mesh.Request) (mesh.Response, error) {
	return p[host].Handle(ctx, r)
}

func describedService(t testing.TB, name string, peers mesh.Transport) *mesh.Service {
	t.Helper()
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), name, "test", 1<<24, storage.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	holder, err := store.Endpoint("http://" + name)
	if err != nil {
		t.Fatal(err)
	}
	return &mesh.Service{Store: store, Self: holder, VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{1}}, Peers: peers}
}

// initCluster gives both disks the same genesis, so that they talk.
func initCluster(t testing.TB, services ...*mesh.Service) {
	t.Helper()
	genesis := causal.Genesis{Cluster: "test", Holder: services[0].Self}
	for _, s := range services {
		if err := s.Store.InitCausal(services[0].VNode, genesis); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContributionsAreFoldedOnlyWithTheirDescription(t *testing.T) {
	d := &algebra.Descriptor{BitsPerChar: 6, Width: 16, RecordDepths: [2]int{1, 16}, Schema: algebra.Schema{algebra.Max}}
	b, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := d.Name()
	digest := algebra.Digest(b)

	peers := peerTransport{}
	sender := describedService(t, "sender", peers)
	if _, err := sender.Store.AdmitCollection(name, b); err != nil {
		t.Fatal(err)
	}
	parent := describedService(t, "parent", peers)
	stale := describedService(t, "stale", peers) // missed the admission
	for _, s := range []*mesh.Service{sender, parent, stale} {
		peers[s.Self.Host] = s
	}
	initCluster(t, sender, parent, stale)
	tr := New(parent, 1, Hooks{})
	in := mesh.Contribution{Collection: name, Location: "ab", Holder: sender.Self, Count: 1, Metrics: []float64{1}, Digest: hex.EncodeToString(digest[:])}

	// Unknown here: fetched from the sender, verified, admitted.
	if ok, err := tr.checkDescription(in); err != nil || !ok {
		t.Fatalf("fetch on miss: %v %v", ok, err)
	}
	if c, _ := parent.Store.CollectionOf(name); c == nil || c.Digest != digest {
		t.Fatalf("description not admitted on the parent")
	}
	// Another digest for a held description: dropped, not merged.
	other := in
	other.Digest = hex.EncodeToString(make([]byte, 32))
	if ok, err := tr.checkDescription(other); err != nil || ok || tr.stats.droppedDescription.Load() != 1 {
		t.Fatalf("a different digest was accepted: %v %v", ok, err)
	}
	// No digest for a typed collection: the child folded it as sums (it
	// missed the admission). Dropped, and the child is sent the description.
	legacy := in
	legacy.Digest, legacy.Holder = "", stale.Self
	if ok, err := tr.checkDescription(legacy); err != nil || ok {
		t.Fatalf("a typed collection folded as sums was accepted: %v %v", ok, err)
	}
	if c, _ := stale.Store.CollectionOf(name); c == nil || c.Digest != digest {
		t.Fatalf("the child that missed the admission was not sent the description")
	}
	// No digest for a legacy collection: folded as it is.
	plain := legacy
	plain.Collection = "Demo000000000000"
	if ok, err := tr.checkDescription(plain); err != nil || !ok {
		t.Fatalf("legacy contribution: %v %v", ok, err)
	}
	// What this disk sends carries the digest.
	if got := tr.descriptionDigest(name); got != in.Digest {
		t.Fatalf("outgoing digest %q", got)
	}
	if got := tr.descriptionDigest("Demo000000000000"); got != "" {
		t.Fatalf("a legacy collection has no digest, got %q", got)
	}
}

func TestCoveredByFindsTheRemoteChildOfALocation(t *testing.T) {
	remotes := []string{"a0", "aB", "b"}
	for loc, want := range map[string]bool{"a0": true, "a0x": true, "aB-": true, "a": false, "aA": false, "b": true, "bzz": true, "c": false, "0": false} {
		if got := coveredBy(remotes, loc); got != want {
			t.Fatalf("%s: %v", loc, got)
		}
	}
}
