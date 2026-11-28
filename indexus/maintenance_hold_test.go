package indexus

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// While maintenance is held, a summary reconcile schedules no cold fold: the
// safe jobs of a joining vnode must not change rights. Lifting the hold
// restores the schedule.
func TestHeldMaintenanceSchedulesNoColdFold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var descriptors []VNodeDescriptor
	for _, first := range []byte{255, 127} {
		id := ID{first, 1}
		descriptors = append(descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.services {
		if err := s.Sync(ctx, []string{f.services[0].Self.Host, f.services[1].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	node := f.nodes[1]
	if err := node.EnableCausalCompaction(CausalCompactionConfig{CoolFor: 10 * time.Millisecond, Interval: 10 * time.Millisecond, Timeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		node.DisableCausalCompaction()
		eventually(t, "cold fold worker stopped", func() bool { return !node.mesh.compactionRunning.Load() })
	}()

	node.HoldMaintenance(true)
	// The cool meter judges whole seconds: after one, the next reconcile
	// would schedule the fold at once if nothing held it.
	time.Sleep(1100 * time.Millisecond)
	for i := 0; i < 30; i++ {
		_ = node.Update()
		time.Sleep(5 * time.Millisecond)
	}
	if node.mesh.lastColdAttempt.Load() != 0 {
		t.Fatal("a cold fold was scheduled while maintenance was held")
	}
	node.HoldMaintenance(false)
	eventually(t, "cold fold scheduled once the hold is lifted", func() bool {
		_ = node.Update()
		return node.mesh.lastColdAttempt.Load() != 0
	})
}

// While maintenance is held, Observe registers a new peer without starting
// the maintenance round that follows it (sync, resume, rebalance).
func TestHeldMaintenanceStartsNoRoundOnANewPeer(t *testing.T) {
	id := ID{255, 1}
	f := newReadPhaseFixture(t, []VNodeDescriptor{{ID: id, Name: BASE64.Encode(id.Bytes())}}, false, false)
	node := f.nodes[0]
	node.HoldMaintenance(true)
	node.acknowledge([]Contact{newDialPeer("PeerBBBBBBBBBBBB", "10.0.0.2", 21001)})
	if err := node.Observe(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the round would run in its own goroutine
	if rounds := node.mesh.gossipRound.Load(); rounds != 0 {
		t.Fatalf("%d maintenance rounds while held", rounds)
	}
	node.HoldMaintenance(false)
	node.acknowledge([]Contact{newDialPeer("PeerCCCCCCCCCCCC", "10.0.0.3", 21001)})
	if err := node.Observe(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a round on a new peer once the hold is lifted", func() bool { return node.mesh.gossipRound.Load() > 0 })
}

// Reproduce a join that has already received a right but is still acquiring
// its cell. Then simulate a crash after persisting a separate cold decision.
func TestJoiningDefersNewColdFoldAndRecoversDurableDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := &coldVolumeFixture{capacity: 1 << 27}
	for _, first := range []byte{255, 127} {
		id := ID{first, 1}
		f.descriptors = append(f.descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
		f.paths = append(f.paths, filepath.Join(t.TempDir(), "data.db"))
	}
	f.open(t)
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	f.sync(t, ctx, 2)
	node := f.nodes[1]
	node.SetJoining(true)
	config := CausalCompactionConfig{CoolFor: time.Minute}
	if err := node.EnableCausalCompaction(config); err != nil {
		t.Fatal(err)
	}
	node.mesh.cool.reset(time.Now().Add(-2*time.Minute), 0)
	node.meshScheduleCompaction()
	if node.mesh.lastColdAttempt.Load() != 0 {
		t.Fatal("joining vnode scheduled a new fold after its first right")
	}
	node.SetJoining(false)
	node.meshScheduleCompaction()
	if node.mesh.lastColdAttempt.Load() != 0 {
		t.Fatal("time spent joining counted as cold")
	}

	// Commit only the donor's intent. Restart all processes and let ordinary
	// maintenance finish it while the recovering donor still reports joining.
	if _, err := f.stores[1].BeginColdDrain("restart-drain", f.services[0].Self, storage.ColdLimits{MaxCombinedFraction: 0.45}); err != nil {
		t.Fatal(err)
	}
	for _, store := range f.stores {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f.open(t)
	node = f.nodes[1]
	node.SetJoining(true)
	if err := node.meshMaintainContext(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := f.stores[1].ColdStatus()
	if err != nil || status.Drain == nil || status.Drain.Operation != "restart-drain" || !status.Drain.Witnessed {
		t.Fatalf("persisted drain after restart: %+v, err %v", status.Drain, err)
	}
}
