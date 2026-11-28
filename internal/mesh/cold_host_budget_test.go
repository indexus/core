package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestAutomaticColdHostAdmissionAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	netA, a, dataA := coldPair(t, ctx, 4)
	netB, b, dataB := coldPair(t, ctx, 4)
	// Independent meshes give each source an eligible survivor. The two
	// sources share only their physical host and open its budget separately.
	dir := t.TempDir()
	for _, source := range []*Service{a[1], b[1]} {
		budget, err := NewCopyBudget(dir, 1)
		if err != nil {
			t.Fatal(err)
		}
		source.SetCopyBudget(budget)
	}
	auto := ColdConfig{Automatic: true}
	expectDeferred := func(source, survivor *Service) {
		t.Helper()
		if err := source.Compact(ctx, auto); !errors.Is(err, ErrColdDeferred) {
			t.Fatalf("wanted a deferred new fold, got %v", err)
		}
		if err := source.ResumeCold(ctx); err != nil {
			t.Fatalf("idle recovery attempted to begin work: %v", err)
		}
		status, err := source.Store.ColdStatus()
		if err != nil || status.Drain != nil {
			t.Fatalf("deferred source recorded a drain: %+v %v", status, err)
		}
		status, err = survivor.Store.ColdStatus()
		if err != nil || status.Pin != nil {
			t.Fatalf("deferred fold pinned its survivor: %+v %v", status, err)
		}
	}
	// Existing expansion takes priority before a new drain is made durable.
	lane, _ := b[1].jobs.copies.take("growth")
	if lane == nil {
		t.Fatal("missing growth lane")
	}
	expectDeferred(a[1], a[0])
	lane.Close()
	a[1].ObserveCopyPressure(51, 50, true)
	expectDeferred(a[1], a[0])
	a[1].ObserveCopyPressure(0, 0, false)

	// While A's first request is in flight, B cannot create a new drain.
	netA.setAfter(func(req Request) error {
		if req.Kind == KindColdPin {
			expectDeferred(b[1], b[0])
			return storage.ErrClosed // reply lost after A’s survivor accepted the pin
		}
		return nil
	})
	if err := a[1].Compact(ctx, auto); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("injected loss: %v", err)
	}
	netA.setAfter(nil)

	// B can start once A's attempt exits. A's durable drain can recover
	// while B holds the permit and even when A now sees CPU pressure.
	resumed := false
	netB.setAfter(func(req Request) error {
		if req.Kind == KindColdPin && !resumed {
			resumed = true
			a[1].ObserveCopyPressure(51, 50, true)
			err := a[1].Compact(ctx, auto)
			a[1].ObserveCopyPressure(0, 0, false)
			return err
		}
		return nil
	})
	if err := b[1].Compact(ctx, auto); err != nil || !resumed {
		t.Fatalf("resumed=%v, second auto fold: %v", resumed, err)
	}
	for _, source := range []*Service{a[1], b[1]} {
		status, err := source.Store.ColdStatus()
		if err != nil || status.Drain == nil || !status.Drain.Witnessed {
			t.Fatalf("fold did not finish: %+v %v", status, err)
		}
	}
	assertColdRows(t, ctx, a, dataA, false)
	assertColdRows(t, ctx, b, dataB, false)
}

func TestColdHostAdmissionScope(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config ColdConfig
		shared bool
	}{
		{"manual fold while host permit held", ColdConfig{}, true},
		{"automatic fold without shared machine budget", ColdConfig{Automatic: true}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, nodes, _ := coldPair(t, ctx, 2)
			if tt.shared {
				budget, err := NewCopyBudget(t.TempDir(), 1)
				if err != nil {
					t.Fatal(err)
				}
				nodes[1].SetCopyBudget(budget)
				release, err := budget.admitColdStart()
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if err := nodes[1].Compact(ctx, tt.config); err != nil {
				t.Fatal(err)
			}
			status, err := nodes[1].Store.ColdStatus()
			if err != nil || status.Drain == nil || !status.Drain.Witnessed {
				t.Fatalf("expected witnessed retirement: %+v %v", status, err)
			}
		})
	}
}

// Freeze the scheduling interleaving exactly between a release and the
// waiting donor's next poll. Its process may also die in that interval.
func TestColdStartHonorsLiveWarmTicket(t *testing.T) {
	budget, err := NewCopyBudget(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := budget.take("first")
	if holder == nil {
		t.Fatal("expected first donor to hold the lane")
	}
	child := startCopyChild(t, budget.dir)
	child.act(t, "try", "waiting")
	releaseCopyFile(holder)
	if release, err := budget.admitColdStart(); !errors.Is(err, ErrColdDeferred) {
		if release != nil {
			release()
		}
		t.Fatalf("cold proposal overtook live donor: %v", err)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.cmd.Wait()
	release, err := budget.admitColdStart()
	if err != nil {
		t.Fatalf("dead donor stalled an idle host: %v", err)
	}
	release()
}
