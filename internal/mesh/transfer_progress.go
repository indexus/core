package mesh

import (
	"log/slog"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
)

type transferProgress struct {
	mu        sync.Mutex
	operation string
	started   time.Time
	logged    time.Time
	zones     int
	batches   int
	steps     map[string]time.Duration
	slowest   map[string]time.Duration
	// hold is the longest time one lot had no writer, from FreezeZones to
	// install-zone: what a write to a moving zone waits at worst.
	hold time.Duration
}

func (p *transferProgress) held(d time.Duration) {
	p.mu.Lock()
	p.hold = max(p.hold, d)
	p.mu.Unlock()
}

func newTransferProgress(operation string) *transferProgress {
	now := time.Now()
	return &transferProgress{operation: operation, started: now, logged: now, steps: map[string]time.Duration{}, slowest: map[string]time.Duration{}}
}

// add charges the time since mark to step, counts zones once installed, and
// returns the new mark.
func (p *transferProgress) add(step string, mark time.Time, zones int) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	took := now.Sub(mark)
	p.steps[step] += took
	p.slowest[step] = max(p.slowest[step], took)
	if zones > 0 {
		metrics.HandoffInstalled(p.operation, zones)
		p.zones += zones
		p.batches++
	}
	if now.Sub(p.logged) >= 5*time.Second {
		p.logLocked(nil)
	}
	return now
}

func (p *transferProgress) log(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logLocked(err)
}

func (p *transferProgress) logLocked(err error) {
	p.logged = time.Now()
	args := []any{"operation", p.operation, "zones", p.zones, "units", p.batches, "elapsed", time.Since(p.started).Round(time.Millisecond), "hold_max", p.hold.Round(time.Millisecond)}
	for _, step := range []string{"state", "freeze", "mirror", "stage", "emit", "install"} {
		args = append(args, step, p.steps[step].Round(time.Millisecond), step+"_max", p.slowest[step].Round(time.Millisecond))
	}
	if err != nil {
		args = append(args, "err", err)
	}
	slog.Info("zone transfer progress", args...)
}

// wireTransfer is what a transfer request carries: the certificate (and the
// snapshots the caller sets), not the per-zone states. Receivers read only
// those; the states map, one entry per zone of the operation, was resent with
// every preload and stage-zone call: ≈6 GB per split on AWS (REM-87).
func wireTransfer(t storage.CausalTransfer) storage.CausalTransfer {
	t.Snapshots = nil
	return t
}
