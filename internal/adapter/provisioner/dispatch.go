package provisioner

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// The callers retain their existing target reservations until their hook
// returns. Only placement decisions are batched. Copies and launches run
// independently, outside the dispatcher lock.
type placementQueue struct {
	mu       sync.Mutex
	running  bool
	requests []*placementCall
}

type placementCall struct {
	ctx               context.Context
	requester, target string
	rate              float64
	recheck           bool
	reply             chan placementAnswer
}

type placementAnswer struct {
	record placementRecord
	env    []string
}

func (s *Server) awaitPlacement(ctx context.Context, dir, requester, target string, rate float64, recheck bool) (placementAnswer, error) {
	q := &s.dispatch
	call := &placementCall{ctx, requester, target, rate, recheck, make(chan placementAnswer, 1)}
	q.mu.Lock()
	q.requests = append(q.requests, call)
	if !q.running {
		q.running = true
		go q.run(s, dir)
	}
	q.mu.Unlock()
	select {
	case a := <-call.reply:
		return a, nil
	case <-ctx.Done():
		return placementAnswer{}, ctx.Err()
	}
}

func (q *placementQueue) run(s *Server, dir string) {
	for {
		machines := s.machineFacts(dir)
		q.mu.Lock()
		calls := q.requests
		q.requests = nil
		q.mu.Unlock()
		waiting := s.placeCalls(machines, calls)
		q.mu.Lock()
		q.requests = append(waiting, q.requests...)
		q.running = len(q.requests) > 0
		active := q.running
		q.mu.Unlock()
		if !active {
			return
		}
		if len(waiting) > 0 {
			time.Sleep(time.Second)
		}
	}
}

// All contenders see the same survey. decidePlacement reserves each concrete
// receiver before the next decision. A donor without an eligible destination
// cannot prevent a different donor from using its own eligible machine.
func (s *Server) placeCalls(machines []autoscale.MachineFacts, calls []*placementCall) (waiting []*placementCall) {
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].rate > calls[j].rate })
	offers := 0
	for _, m := range machines {
		if ready, _ := autoscale.ReceiverOffer(m); ready {
			offers++
		}
	}
	for _, c := range calls {
		if c.ctx.Err() != nil {
			continue
		}
		if c.recheck && offers <= 0 {
			waiting = append(waiting, c)
			continue
		}
		record, env := s.planPlacement(machines, c.requester, c.target, c.recheck)
		record.WriteRate, record.Recheck = c.rate, c.recheck
		if c.recheck && (record.Machine == "" || record.Kind == autoscale.PlaceSlots) {
			waiting = append(waiting, c)
			continue
		}
		if record.Machine != "" || record.capMachine != "" {
			offers--
		}
		c.reply <- placementAnswer{record, env}
	}
	return waiting
}
