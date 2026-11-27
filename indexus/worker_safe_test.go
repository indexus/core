package indexus

import (
	"sync/atomic"
	"testing"
	"time"
)

// countingJobs counts the runs of each job and records the hold.
type countingJobs struct {
	observe, update, refresh, autoscale atomic.Int64
	held                                atomic.Bool
}

func (*countingJobs) Delay() time.Duration     { return time.Millisecond }
func (c *countingJobs) Observe() error         { c.observe.Add(1); return nil }
func (c *countingJobs) Update() error          { c.update.Add(1); return nil }
func (c *countingJobs) Refresh() error         { c.refresh.Add(1); return nil }
func (c *countingJobs) AutoscaleTick()         { c.autoscale.Add(1) }
func (c *countingJobs) HoldMaintenance(h bool) { c.held.Store(h) }

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
	}
}

// StartSafe runs observe, update and the autoscale tick, holds what changes
// rights and does not rebalance. Start adds rebalance and lifts the hold.
// Stop returns once no safe job runs any more.
func TestWorkerSafeJobsRunBeforeRebalance(t *testing.T) {
	jobs := &countingJobs{}
	w := NewWorker(jobs)
	w.StartSafe()
	w.StartSafe() // once
	eventually(t, "safe jobs running", func() bool {
		return jobs.observe.Load() > 2 && jobs.update.Load() > 2 && jobs.autoscale.Load() > 2
	})
	if jobs.refresh.Load() != 0 || !jobs.held.Load() {
		t.Fatalf("before Start: %d rebalances, held=%v", jobs.refresh.Load(), jobs.held.Load())
	}
	done := make(chan struct{})
	go func() { _ = w.Start(); close(done) }()
	eventually(t, "rebalance after Start", func() bool { return jobs.refresh.Load() > 0 && !jobs.held.Load() })

	w.Stop()
	<-done
	observed, updated, ticked := jobs.observe.Load(), jobs.update.Load(), jobs.autoscale.Load()
	time.Sleep(20 * time.Millisecond)
	if jobs.observe.Load() != observed || jobs.update.Load() != updated || jobs.autoscale.Load() != ticked {
		t.Fatal("a safe job ran after Stop")
	}
}

// Start alone still runs every job and never holds maintenance.
func TestWorkerStartAloneRunsEveryJob(t *testing.T) {
	jobs := &countingJobs{}
	w := NewWorker(jobs)
	go func() { _ = w.Start() }()
	defer w.Stop()
	eventually(t, "every job running", func() bool {
		return jobs.observe.Load() > 0 && jobs.update.Load() > 0 && jobs.autoscale.Load() > 0 && jobs.refresh.Load() > 0
	})
	if jobs.held.Load() {
		t.Fatal("maintenance held without StartSafe")
	}
}
