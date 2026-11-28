package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// A CopyBudget bounds the transfers that copy at the same time on one host
// (HOST-COPY-1). The vnodes of a host are separate processes, so the budget
// is a directory they share: one file per slot, held with flock. A process
// that dies releases its slots with its file descriptors.
//
// Source-lane rules (the independent warm receiver lane is used in preload):
//
//   - A transfer holds at most one slot, named after it. A source and a
//     receiver of the same transfer on one host share that slot: the receiver
//     finds the name and takes nothing.
//   - Only the source waits, before anything of its grain is fenced, and at
//     most copySlotWait: exhaustion defers the grain; it never bypasses. A
//     resumed grain that already fenced zones, and a receiver, never wait.
//   - Nobody holds a slot while waiting for another one, so two transfers
//     cannot wait for each other.
//   - Writes never look at the budget.
//
// A nil budget is off: every acquire succeeds at once.
type CopyBudget struct {
	dir      string
	slots    int
	pressure atomic.Pointer[copyPressure]
	localIPs map[netip.Addr]bool
	// Independent of the outgoing slots: reciprocal senders can both receive.
	receiver *CopyBudget
}

type copyPressure struct {
	used, target float64
	// Closed when the host sampler replaces this observation. A waiter
	// subscribes to exactly the sample from which it computed its deadline.
	changed chan struct{}
}

// ObserveCopyPressure feeds the already sampled machine CPU into its copy
// scheduler. Unknown measurements do not invent a resource guarantee.
func (s *Service) ObserveCopyPressure(used, target float64, known bool) {
	if b := s.jobs.copies; b != nil {
		var next *copyPressure
		if known && target > 0 {
			next = &copyPressure{used: used, target: target}
			if used > target {
				next.changed = make(chan struct{})
			}
		}
		if old := b.pressure.Swap(next); old != nil && old.changed != nil {
			close(old.changed)
		}
	}
}

// Variables only so tests can shrink them.
var (
	// copySlotWait bounds a source's wait for a slot. It is longer than a
	// grain usually lasts and shorter than handoffWait, so a joiner's request
	// still spans it.
	copySlotWait = 10 * time.Second
	copySlotPoll = 20 * time.Millisecond
)

// Background work gets a quarter of the logical cores, at least one lane.
// This bounds concurrency, not CPU; measured pressure also paces warm grains.
func DefaultCopySlots() int { return max(1, runtime.NumCPU()/4) }

// NewCopyBudget opens the budget kept in dir, shared by every process given
// the same directory. An empty dir is no budget; slots under one is
// DefaultCopySlots.
func NewCopyBudget(dir string, slots int) (*CopyBudget, error) {
	if dir == "" {
		return nil, nil
	}
	if slots < 1 {
		slots = DefaultCopySlots()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b := &CopyBudget{dir: dir, slots: slots, localIPs: localCopyIPs()}
	if slots == 1 {
		incoming := filepath.Join(dir, "incoming")
		if err := os.MkdirAll(incoming, 0o755); err != nil {
			return nil, err
		}
		b.receiver = &CopyBudget{dir: incoming, slots: 1}
	}
	return b, nil
}

// SetCopyBudget gives the service its host's budget, before it serves.
func (s *Service) SetCopyBudget(b *CopyBudget) { s.jobs.copies = b }

// ErrColdDeferred means an optional automatic fold did no work. The caller
// can wait for its next normal maintenance tick without refreshing summaries.
var ErrColdDeferred = errors.New("host deferred a new cold fold")

// Hold a separate lock throughout the first attempt. Probing a grain slot
// gives current copy work priority; release the probe before the fold needs
// the slot itself. A process exit releases both file locks automatically.
func (b *CopyBudget) admitColdStart() (func(), error) {
	if p := b.pressure.Load(); p != nil && p.used >= p.target {
		return nil, ErrColdDeferred
	}
	permit, err := b.tryColdLock("cold-fold")
	if err != nil {
		return nil, err
	}
	for i := 0; i < b.slots; i++ {
		probe, err := b.tryColdLock("slot-" + strconv.Itoa(i))
		if errors.Is(err, ErrColdDeferred) {
			continue
		}
		if err != nil {
			permit.Close()
			return nil, err
		}
		probe.Close()
		return func() { _ = permit.Close() }, nil
	}
	permit.Close()
	return nil, ErrColdDeferred
}

func (b *CopyBudget) tryColdLock(name string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(b.dir, name), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrColdDeferred
		}
		return nil, err
	}
	return f, nil
}

// take tries every slot once. It returns the slot it locked, named after
// transfer, or shared when another holder already copies this transfer. A
// name left on a free slot is that of a holder that died: it is taken.
func (b *CopyBudget) take(transfer string) (held *os.File, shared bool) {
	for i := 0; i < b.slots; i++ {
		f, err := os.OpenFile(filepath.Join(b.dir, "slot-"+strconv.Itoa(i)), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			continue
		}
		locked := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
		switch {
		case !locked && transfer != "" && slotName(f) == transfer:
			f.Close()
			if held != nil {
				held.Close()
			}
			return nil, true
		case locked && held == nil:
			held = f
		default:
			f.Close() // a second free slot is given back
		}
	}
	if held == nil {
		return nil, false
	}
	if held.Truncate(0) != nil {
		held.Close()
		return nil, false
	}
	_, _ = held.WriteAt([]byte(transfer), 0)
	return held, false
}

func slotName(f *os.File) string {
	name, _ := io.ReadAll(io.LimitReader(io.NewSectionReader(f, 0, 256), 256))
	return string(name)
}

// acquire takes a slot for transfer, waiting at most wait, and returns its
// release. Without a slot after wait, or when ctx ends, the caller proceeds
// all the same and the release does nothing.
func (b *CopyBudget) acquire(ctx context.Context, transfer string, wait time.Duration) (release func()) {
	release, _ = b.reserve(ctx, transfer, wait, false)
	return release
}

// reserve admits an unfenced grain strictly, or makes immediate progress on
// already fenced work. Exhausted warm admission never bypasses the lane limit.
func (b *CopyBudget) reserve(ctx context.Context, transfer string, wait time.Duration, warm bool) (func(), error) {
	held, err := b.reserveFile(ctx, transfer, wait, warm)
	return func() { releaseCopyFile(held) }, err
}

func releaseCopyFile(held *os.File) {
	if held != nil {
		_ = held.Truncate(0)
		_ = held.Close()
	}
}

// An unnamed slot admits a proposal before the disk allocates its operation ID.
// Only a named slot may be shared by a co-resident receiver.
func (b *CopyBudget) reserveFile(ctx context.Context, transfer string, wait time.Duration, warm bool) (*os.File, error) {
	if b == nil {
		return nil, ctx.Err()
	}
	deadline := time.Now().Add(wait)
	for waited := false; ; waited = true {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		held, shared := b.take(transfer)
		switch {
		case shared:
			metrics.Handoffs.CopySlotShared.Add(1)
			return nil, nil
		case held != nil:
			return held, nil
		case wait <= 0:
			metrics.Handoffs.CopySlotBusy.Add(1)
			return nil, nil
		case !waited:
			metrics.Handoffs.CopySlotWaits.Add(1)
			metrics.Handoffs.CopySlotWaiting.Add(1)
			started := time.Now()
			defer func() {
				metrics.Handoffs.CopySlotWaiting.Add(-1)
				metrics.Handoffs.CopySlotWaitNanos.Add(int64(time.Since(started)))
			}()
		}
		if !time.Now().Before(deadline) {
			metrics.Handoffs.CopySlotTimeouts.Add(1)
			if warm {
				return nil, fmt.Errorf("%w: host copy budget busy", storage.ErrNotReady)
			}
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(copySlotPoll):
		}
	}
}

// Under pressure, the measured idle share sets the warm-work duty cycle.
// A tenth remains for forward progress: capacity can only improve after some
// authority has moved. No sleep occurs here or inside a frozen lot. The
// caller keeps its host lane while yielding, so another local source cannot
// bypass it by taking that lane. This is a soft policy, not a CPU quota.
func (p *copyPressure) cooldown(work time.Duration) time.Duration {
	if p == nil || p.used <= p.target {
		return 0
	}
	share := max(.1, min(1, (100-p.used)/100))
	return time.Duration(float64(work) * (1 - share) / share)
}

// pace yields between completed lots. A recovery pass with frozen zones, or a
// region already sealed, is urgent and must never prolong a no-writer window.
func (b *CopyBudget) pace(ctx context.Context, operation string, work time.Duration, urgent bool) error {
	if b == nil || urgent {
		return ctx.Err()
	}
	var idle time.Duration
	if replies, ok := ctx.Value(copyRepliesKey{}).(*copyReplies); ok {
		idle = min(work, time.Duration(replies.wait.Load()))
		metrics.Handoffs.CopyPeerWaitNanos.Add(int64(idle))
	}
	// A lot active for A needs A*(1-share)/share idle time in total.
	// The time already spent waiting for a remote reply counts toward it.
	work -= idle
	sample := b.pressure.Load()
	delay := sample.cooldown(work) - idle
	if delay <= 0 {
		return ctx.Err()
	}
	metrics.HandoffPhase(operation, "copy-pacing")
	started := time.Now()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	defer func() {
		metrics.Handoffs.CopyPacedLots.Add(1)
		metrics.Handoffs.CopyPacedNanos.Add(int64(time.Since(started)))
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-sample.changed:
			sample = b.pressure.Load()
			shorter := sample.cooldown(work) - idle
			if shorter <= time.Since(started) {
				return ctx.Err()
			}
			if shorter < delay {
				delay = shorter
				timer.Reset(time.Until(started.Add(delay)))
			}
		}
	}
}

// copySlot takes the host slot of an outgoing grain for as long as it
// copies. It waits only while nothing of the grain is fenced: a wait would
// otherwise lengthen a freeze.
func (s *Service) copySlot(ctx context.Context, offer storage.CausalTransfer) (func(), error) {
	wait := copySlotWait
	if offer.Phase >= causal.TransferMoving {
		wait = 0
	}
	id := offer.Cession.ID.String()
	metrics.HandoffPhase(id, "copy-slot")
	return s.jobs.copies.reserve(ctx, id, wait, wait > 0)
}

// preload queues only explicitly unfenced snapshot pages on single-lane hosts.
// Recovery and admission headers bypass that queue. The outgoing slot probe
// remains nonblocking in both cases.
func (s *Service) preload(ctx context.Context, transfer storage.CausalTransfer, warm bool) (map[string]uint64, error) {
	if budget := s.jobs.copies; warm && len(transfer.Snapshots) > 0 && budget != nil && budget.receiver != nil {
		// Serialize only this local write. The receiver holds no incoming
		// lane while waiting on a remote peer or another transfer.
		release, err := budget.receiver.reserve(ctx, "", copySlotWait, true)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	defer s.jobs.copies.acquire(ctx, transfer.Cession.ID.String(), 0)()
	return s.Store.PreloadCausal(transfer)
}
