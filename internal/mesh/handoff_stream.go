package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A handoff streams a region to its receiver (HANDOFF-STREAM):
//
//   - The copy goes in pages of at most preloadPageRecords rows (whole zones),
//     each one bounded request, instead of one request carrying the region.
//   - A later round sends only the zones the receiver does not hold yet; the
//     others advance through the mutation mirror from the receiver's progress.
//   - The donor runs each operation once, in the background, for at most
//     handoffJobTimeout. A request waits handoffWait for it and otherwise
//     answers not-ready; a retry joins the same job instead of starting over.
//
// The receiver needs nothing new: a preload already accepts any subset of
// the region's zones and answers its progress per zone.
const handoffJobTimeout = 30 * time.Minute

// Variables only so tests can shrink them.
var (
	preloadPageRecords = 2000
	handoffWait        = 20 * time.Second
)

// ErrHandoffPending answers a handoff request whose job is still running. It
// is not-ready on the wire, so callers retry.
var ErrHandoffPending = fmt.Errorf("%w: handoff in progress", storage.ErrNotReady)

// ErrNotNeeded answers a new joiner when the donor is no longer under
// pressure: a joiner that waited "busy" through a spike must not join after
// it, move data for nothing and be folded away again. The joiner leaves.
var ErrNotNeeded = errors.New("donor no longer needs a peer")

type handoffJob struct {
	right causal.Digest
	done  chan struct{}
	err   error
}

type handoffJobs struct {
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	mu      sync.Mutex
	jobs    map[string]*handoffJob
	// known is the receiver's progress per operation; only the donor's
	// handoff worker, serialized by its right gate, reads or writes it.
	known map[causal.TransferID]map[string]uint64
	// copies is the host's copy budget, nil when off (HOST-COPY-1).
	copies *CopyBudget
}

// close cancels network/phase waits, then joins workers before their Store is
// closed. Durable fences survive cancellation and resume after reopen.
func (j *handoffJobs) close() {
	j.mu.Lock()
	j.closed = true
	if j.cancel != nil {
		j.cancel()
	}
	j.mu.Unlock()
	j.workers.Wait()
}

func (j *handoffJobs) progress(operation causal.TransferID) map[string]uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.known == nil {
		j.known = map[causal.TransferID]map[string]uint64{}
	}
	if j.known[operation] == nil {
		j.known[operation] = map[string]uint64{}
	}
	return j.known[operation]
}

func (j *handoffJobs) bindRight(key string, input causal.Digest) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job := j.jobs[key]; job != nil {
		job.right = input
	}
}

func (j *handoffJobs) forget(operation causal.TransferID) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.known, operation)
}

// handoffAsync runs request as the single background job of its operation
// and waits for it at most wait (or until ctx ends).
func (s *Service) handoffAsync(ctx context.Context, request Request, wait time.Duration) error {
	key := handoffKey(request)
	s.jobs.mu.Lock()
	if s.jobs.closed {
		s.jobs.mu.Unlock()
		return storage.ErrClosed
	}
	if s.jobs.ctx == nil {
		s.jobs.ctx, s.jobs.cancel = context.WithCancel(context.Background())
	}
	if s.jobs.jobs == nil {
		s.jobs.jobs = map[string]*handoffJob{}
	}
	job := s.jobs.jobs[key]
	if job == nil {
		// A consumed right has one successor. Different installed rights are
		// disjoint and may progress independently, with a small disk budget.
		busy := len(s.jobs.jobs) >= 2
		for other, running := range s.jobs.jobs {
			if running.right == request.Input && other != handoffKey(request) {
				busy = true
			}
		}
		if busy {
			s.jobs.mu.Unlock()
			metrics.Handoffs.Busy.Add(1)
			return ErrHandoffPending
		}
		job = &handoffJob{done: make(chan struct{}), right: request.Input}
		s.jobs.jobs[key] = job
		baseCtx := s.jobs.ctx
		s.jobs.workers.Add(1)
		go func() {
			defer s.jobs.workers.Done()
			jobCtx, cancel := context.WithTimeout(baseCtx, handoffJobTimeout)
			defer cancel()
			job.err = s.handoff(jobCtx, request)
			s.jobs.mu.Lock()
			delete(s.jobs.jobs, key)
			s.jobs.mu.Unlock()
			close(job.done)
		}()
	}
	s.jobs.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-job.done:
		return job.err
	case <-timer.C:
		return ErrHandoffPending
	case <-ctx.Done():
		return ctx.Err()
	}
}

// copyAndMirror brings the receiver up to offer's snapshots. known holds the
// receiver's progress per zone from earlier rounds of the same handoff and is
// updated in place: zones it lacks are preloaded in pages, the others only
// mirror their new mutations.
func (s *Service) copyAndMirror(ctx context.Context, offer storage.CausalTransfer, known map[string]uint64) error {
	target := offer.Cession.Target
	if err := s.shipCatalog(ctx, target); err != nil {
		return err
	}
	var page []storage.Snapshot
	rows := 0
	flushAlways := func() error {
		part := wireTransfer(offer)
		part.Snapshots = page
		response, err := s.endpointCall(ctx, target, Request{Kind: "preload", Transfer: &part})
		if err != nil {
			return err
		}
		for k, v := range response.Progress {
			known[k] = v
		}
		page, rows = nil, 0
		return nil
	}
	flush := func() error {
		if len(page) == 0 {
			return nil
		}
		return flushAlways()
	}
	for _, snap := range offer.Snapshots {
		if _, ok := known[zoneKey(snap)]; ok {
			continue
		}
		if rows > 0 && rows+len(snap.Records) > preloadPageRecords {
			if err := flush(); err != nil {
				return err
			}
		}
		page = append(page, snap)
		rows += len(snap.Records)
	}
	if rows == 0 && len(page) == 0 {
		// Nothing new to copy: still preload once, so the receiver sees this
		// offer's certificate and answers its current progress. An empty
		// region preloads whole; otherwise its smallest zone stands in.
		if len(offer.Snapshots) == 0 {
			page = offer.Snapshots
		} else {
			smallest := offer.Snapshots[0]
			for _, snap := range offer.Snapshots[1:] {
				if len(snap.Records) < len(smallest.Records) {
					smallest = snap
				}
			}
			page = []storage.Snapshot{smallest}
		}
		if err := flushAlways(); err != nil {
			return err
		}
	} else if err := flush(); err != nil {
		return err
	}
	return s.mirrorOffer(ctx, offer, known)
}

// mirrorOffer sends only mutations after an acknowledged durable preload.
func (s *Service) mirrorOffer(ctx context.Context, offer storage.CausalTransfer, known map[string]uint64) error {
	target := offer.Cession.Target
	ranges := make([]storage.MutationRange, 0, len(offer.Snapshots))
	for _, snap := range offer.Snapshots {
		after, ok := known[zoneKey(snap)]
		if !ok || after > snap.Seq {
			return storage.ErrSequence
		}
		if after < snap.Seq {
			ranges = append(ranges, storage.MutationRange{Zone: snap.Zone, After: after, Through: snap.Seq})
		}
	}
	for start := 0; start < len(ranges); start += mirrorBatchZones {
		part := ranges[start:min(start+mirrorBatchZones, len(ranges))]
		batch, err := s.Store.MutationsBatch(part)
		if err != nil {
			return err
		}
		if len(batch) > 0 {
			if _, err = s.endpointCall(ctx, target, Request{Kind: "mirror", Operation: offer.Cession.ID, Batch: batch}); err != nil {
				return err
			}
		}
		for _, r := range part {
			known[zoneKey(storage.Snapshot{Zone: r.Zone})] = r.Through
		}
	}
	return nil
}

func zoneKey(snap storage.Snapshot) string { return zoneKeyOf(snap.Zone) }
func zoneKeyOf(z tree.ZoneKey) string      { return z.Collection + "\x00" + z.Location }

// An emitted zone cannot thaw. If installation or its ACK was lost, replay the
// same proof before proceeding; the receiver must preserve later live writes.
func (s *Service) resumeZoneInstalls(ctx context.Context, transfer storage.CausalTransfer) error {
	states, err := s.Store.TransferZones(transfer.Cession.ID)
	if err != nil {
		return err
	}
	batchSize := s.FlipUnitZones
	if batchSize < 1 {
		batchSize = flipUnitZones
	}
	var proofs []causal.ZoneTransfer
	var keys []tree.ZoneKey
	flush := func() error {
		if len(proofs) == 0 {
			return nil
		}
		done := s.beginCutover(keys)
		defer s.endCutover(keys, done)
		// A fenced zone was ceded in the same commit (FLIP-ONE-1): the receiver
		// may hold neither its last delta nor its certification. Replay the
		// seals with the proofs, then the fenced snapshots themselves.
		snaps, err := s.Store.FencedSnapshots(transfer.Cession.ID, keys)
		if err != nil {
			return err
		}
		seals := make([]storage.ZoneSeal, len(snaps))
		for i, snap := range snaps {
			if seals[i], err = snap.Seal(); err != nil {
				return err
			}
		}
		install := Request{Kind: "install-zone", Operation: transfer.Cession.ID, Owner: s.Self, Seals: seals, ZoneTransfers: proofs}
		_, err = s.retryZoneInstall(ctx, transfer.Cession.Target, install)
		if errors.Is(err, storage.ErrSequence) || errors.Is(err, storage.ErrNotReady) {
			part := wireTransfer(transfer)
			part.Snapshots = snaps
			_, err = s.retryZoneInstall(ctx, transfer.Cession.Target, Request{Kind: "install-zone", Operation: transfer.Cession.ID, Transfer: &part, ZoneTransfers: proofs})
		}
		if err == nil {
			err = s.Store.AckZoneInstalls(transfer.Cession.ID, keys)
		}
		proofs, keys = nil, nil
		return err
	}
	for _, st := range states {
		if !st.Transferred || st.Acked {
			continue
		}
		rows, err := s.Store.ZoneTransfers(st.Zone)
		if err != nil {
			return err
		}
		found := false
		for _, proof := range rows {
			if proof.Operation == transfer.Cession.ID {
				proofs, keys = append(proofs, proof), append(keys, st.Zone)
				found = true
				break
			}
		}
		if !found {
			return storage.ErrNotReady
		}
		if len(proofs) == batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// transferLotBytes targets small grouped lots; a single zone up to
// transferZoneBytes travels alone. Anything larger is refused before fencing.
const (
	transferLotBytes  = 1 << 20
	transferZoneBytes = 8 << 20
)

// copyLot prepares one byte-bounded slice of unit while its donor stays
// writable, then converges recent mutations via ZoneSeqs without re-reading
// full zone payloads on every catch-up attempt.
func (s *Service) copyLot(ctx context.Context, transfer storage.CausalTransfer, unit []tree.ZoneKey, known map[string]uint64) ([]tree.ZoneKey, error) {
	snaps, err := s.Store.Snapshots(ctx, unit, transferLotBytes)
	if err != nil {
		return nil, err
	}
	part := wireTransfer(transfer)
	var keys []tree.ZoneKey
	var bytes int
	allKnown := true
	for _, snap := range snaps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return nil, err
		}
		if len(raw) > transferZoneBytes {
			return nil, storage.ErrInventoryItemTooLarge
		}
		if len(part.Snapshots) > 0 && bytes+len(raw) > transferLotBytes {
			break
		}
		bytes += len(raw)
		part.Snapshots = append(part.Snapshots, snap)
		keys = append(keys, snap.Zone)
		if _, ok := known[zoneKey(snap)]; !ok {
			allKnown = false
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	lastPending := uint64(0)
	for attempt := 0; attempt < 4; attempt++ {
		var err error
		if attempt == 0 && !allKnown {
			err = s.copyAndMirror(ctx, part, known)
		} else {
			err = s.mirrorOffer(ctx, part, known)
		}
		if err != nil {
			return nil, err
		}
		seqs, err := s.Store.ZoneSeqs(keys)
		if err != nil {
			return nil, err
		}
		pending := uint64(0)
		for i, seq := range seqs {
			after, ok := known[zoneKeyOf(keys[i])]
			if !ok || after > seq {
				return nil, storage.ErrSequence
			}
			pending += seq - after
			part.Snapshots[i].Seq = seq
		}
		bounded := attempt > 0 && pending <= lastPending && pending <= uint64(len(keys))/freezeBacklogDivisor
		if pending == 0 || bounded || (attempt > 0 && pending <= freezeBacklogFloor) {
			return keys, nil
		}
		lastPending = pending
	}
	return nil, protocol.ErrRhoDiverged
}

// rightGates serialize the one-successor decision, not unrelated authority.
// References include waiters: deleting a live gate would let a third caller
// bypass its queue. Cancellation releases a reference without taking the gate.
type rightGates struct {
	mu      sync.Mutex
	entries map[causal.Digest]*rightGate
}
type rightGate struct {
	held chan struct{}
	refs int
}

func (g *rightGates) acquire(ctx context.Context, right causal.Digest) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.entries == nil {
		g.entries = map[causal.Digest]*rightGate{}
	}
	entry := g.entries[right]
	if entry == nil {
		entry = &rightGate{held: make(chan struct{}, 1)}
		g.entries[right] = entry
	}
	entry.refs++
	g.mu.Unlock()
	releaseRef := func() {
		g.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(g.entries, right)
		}
		g.mu.Unlock()
	}
	select {
	case entry.held <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-entry.held
			releaseRef()
			return nil, err
		}
		return func() { <-entry.held; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

// retryZoneInstall repairs a failed handoff in its existing worker, without a
// maintenance-period delay or another goroutine. A lost ACK replays the same
// durable proof; timeout never restores the donor's consumed permission.
func (s *Service) retryZoneInstall(ctx context.Context, target causal.Endpoint, request Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	out, err := s.endpointCall(ctx, target, request)
	for delay := time.Duration(0); err != nil; delay = min(max(25*time.Millisecond, delay*2), 200*time.Millisecond) {
		if errors.Is(err, storage.ErrSequence) || errors.Is(err, storage.ErrNotReady) || errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotOwner) || errors.Is(err, storage.ErrClosed) {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
		out, err = s.endpointCall(ctx, target, request)
	}
	return out, err
}

// A short partition must not abandon an otherwise live join. Control requests
// are idempotent (handoff joins the donor's existing job); protocol refusals
// remain visible to the join state machine and durable interruptions escape.
func (s *Service) joinCall(ctx context.Context, to causal.Endpoint, request Request) (Response, error) {
	out, err := s.endpointCall(ctx, to, request)
	for _, delay := range []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond} {
		if err == nil || ctx.Err() != nil || joinRefusal(err) {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
		out, err = s.endpointCall(ctx, to, request)
	}
	return out, err
}

func joinRefusal(err error) bool {
	for _, refusal := range []error{
		context.Canceled, context.DeadlineExceeded, storage.ErrClosed,
		storage.ErrConflict, storage.ErrNotOwner, storage.ErrNotReady,
		storage.ErrFrozen, storage.ErrSequence, storage.ErrDataCapacity,
		storage.ErrWrongCluster, storage.ErrInventoryItemTooLarge, storage.ErrRhoDiverged,
		causal.ErrConflict, causal.ErrDependency, ErrNotNeeded,
	} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}
