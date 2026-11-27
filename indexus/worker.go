package indexus

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type Service interface {
	Delay() time.Duration
	Observe() error
	Refresh() error
	Update() error
}

// OptionalAutoscale is implemented by nodes that decide on scaling themselves.
type OptionalAutoscale interface {
	AutoscaleTick()
}

// MaintenanceHolder is implemented by services whose safe jobs can start work
// that changes rights. The worker holds that work from StartSafe to Start.
type MaintenanceHolder interface {
	HoldMaintenance(held bool)
}

// A Worker runs the background jobs of a node in two groups.
//
// The safe jobs only read rights or act through requests that the store
// validates: observe (peer liveness, discovery, item counters), update
// (summary reconcile and flush) and the autoscale tick. A vnode that holds a
// first installed right runs them while the rest of its join is still copying
// (StartSafe).
//
// Rebalance (gossip sync, resume of interrupted transfers, rebalance, and
// replication when enabled) acquires and gives rights: it would race the
// first join attempt, so it starts with Start, once that attempt returned.
type Worker struct {
	Service Service
	ctx     context.Context
	cancel  context.CancelFunc
	safe    sync.Once
	running sync.WaitGroup // the safe jobs' goroutines
}

func NewWorker(service Service) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		Service: service,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// StartSafe starts the safe jobs, once, and returns. Until Start the service
// holds the work of theirs that changes rights (MaintenanceHolder).
func (w *Worker) StartSafe() {
	w.safe.Do(func() {
		w.hold(true)
		w.startSafe()
	})
}

func (w *Worker) hold(held bool) {
	if holder, ok := w.Service.(MaintenanceHolder); ok {
		holder.HoldMaintenance(held)
	}
}

func (w *Worker) startSafe() {
	slog.Info("background jobs started", "interval", w.Service.Delay())

	// Refresh hands zones to peers over the network and can spend minutes on
	// ones that stopped answering. Anything sharing its tick stops for as long
	// as it runs: liveness would go on routing writes to peers it should have
	// dropped, the soft-cache would freeze mid-rebalance, and the node would
	// hold a pressure reading from before the handover while memory keeps
	// climbing — blind exactly when it has to decide whether to ask for help.
	w.spawn("observe", w.Service.Delay, w.Service.Observe)
	if contextual, ok := w.Service.(interface{ UpdateContext(context.Context) error }); ok {
		w.spawn("update", w.Service.Delay, func() error { return contextual.UpdateContext(w.ctx) })
	} else {
		w.spawn("update", w.Service.Delay, w.Service.Update)
	}
	if autoscale, ok := w.Service.(OptionalAutoscale); ok {
		// The write demand signal is a rate: a spike must be seen within a
		// second or two, not one maintenance period later.
		w.spawn("autoscale", func() time.Duration { return min(w.Service.Delay(), autoscaleTick) }, func() error {
			autoscale.AutoscaleTick()
			return nil
		})
	}
}

// spawn runs a safe job in its own goroutine, which Stop waits for.
func (w *Worker) spawn(name string, delay func() time.Duration, job func() error) {
	w.running.Add(1)
	go func() {
		defer w.running.Done()
		w.everyWith(name, delay, job)
	}()
}

// Start runs every job: the safe ones unless StartSafe already started them,
// and rebalance, which it runs itself until the worker is closed.
func (w *Worker) Start() error {
	w.safe.Do(w.startSafe)
	w.hold(false)

	// A job failure is almost always transient — an unreachable peer, a full
	// queue. Returning here would stop rebalancing for good while the node
	// keeps answering requests.
	if contextual, ok := w.Service.(interface{ RefreshContext(context.Context) error }); ok {
		w.every("rebalance", func() error { return contextual.RefreshContext(w.ctx) })
	} else {
		w.every("rebalance", w.Service.Refresh)
	}
	return nil
}

// every runs job on the service cadence until the worker is closed, logging
// failures instead of giving up on the loop.
//
// The first pass runs immediately: joiners must Ping their bootstrap without
// waiting a full jobInterval, or the bootstrap stays at peers=1 until the
// first Observe tick (and /routing stays empty until clean()).
// autoscaleTick bounds the autoscale period (see Start).
const autoscaleTick = time.Second

func (w *Worker) every(name string, job func() error) { w.everyWith(name, w.Service.Delay, job) }

func (w *Worker) everyWith(name string, delay func() time.Duration, job func() error) {
	run := func() {
		if w.ctx.Err() != nil {
			return
		}
		if err := job(); err != nil {
			slog.Warn(name+" failed", "err", err)
		}
	}
	run()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(delay()):
			run()
		}
	}
}

func (w *Worker) Close() {
	w.cancel()
}

// Stop closes the worker and waits for the safe jobs to return: afterwards
// none of them touches the store.
func (w *Worker) Stop() {
	w.cancel()
	w.running.Wait()
}
