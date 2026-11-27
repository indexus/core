package storage

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestVerifiedCausalTracksOnlyCommittedControl(t *testing.T) {
	store, _ := routingStore(t)
	before, err := store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if store.committedCausal() != before.snapshot {
		t.Fatal("initial committed snapshot unavailable")
	}
	// An unrelated data/control row advances the engine seq, but does not
	// invalidate the authority facts under the unchanged control version.
	if err := store.update(func(tx *Tx) error { return tx.put(meta([]byte("unrelated")), []byte("data")) }); err != nil {
		t.Fatal(err)
	}
	if store.committedCausal() != before.snapshot {
		t.Fatal("unrelated commit lost valid authority facts")
	}
	prepared, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.updateCausal(func(tx *Tx, state CausalState) error {
			state.Permissions.EndpointEpoch++
			if err := writeCausal(tx, state); err != nil {
				return err
			}
			close(prepared)
			<-release
			return nil
		})
	}()
	select {
	case <-prepared:
	case err := <-done:
		t.Fatalf("control update failed before its commit gate: %v", err)
	}
	// The update has encoded and cached its new control but has not committed.
	// Readers must keep the last committed fact without waiting on writeMu.
	readDone := make(chan *causalSnapshot, 1)
	go func() { readDone <- store.committedCausal() }()
	var got *causalSnapshot
	select {
	case got = <-readDone:
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("authority hint waited on the in-flight commit")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got != before.snapshot {
		t.Fatal("uncommitted control replaced last committed authority")
	}
	if current := store.committedCausal(); current != nil && bytes.Equal(current.version, before.snapshot.version) {
		t.Fatal("verified seq fast path served old control after commit")
	}
	after, err := store.CausalRouting()
	if err != nil || after.snapshot == before.snapshot || after.snapshot.state.Permissions.EndpointEpoch != before.snapshot.state.Permissions.EndpointEpoch+1 {
		t.Fatalf("new authority control not observed: %v", err)
	}
	if store.committedCausal() != after.snapshot {
		t.Fatal("new committed control was not verified")
	}
}

func TestVerifiedCausalDoesNotSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified")
	store, err := OpenWithOptions(path, "root", "verified", 1<<24, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.Endpoint("root")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.InitCausal(tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}, causal.Genesis{Cluster: "verified", Holder: endpoint}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	before, err := store.CausalRouting()
	if err != nil || store.committedCausal() != before.snapshot {
		store.Close()
		t.Fatalf("warm cache: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithOptions(path, "root", "verified", 1<<24, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.causalVerified.Load() != nil {
		t.Fatal("process cache restored as durable authority")
	}
	after, err := reopened.CausalRouting()
	if err != nil || !bytes.Equal(after.snapshot.version, before.snapshot.version) || after.snapshot == before.snapshot {
		t.Fatalf("reopened durable control differs from committed state: %v", err)
	}
}
