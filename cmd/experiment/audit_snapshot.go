package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

func auditOwnership(ctx context.Context, mons []string, report *auditReport) {
	type nodeOwn struct {
		m     string
		own   map[string]map[string]any
		items int64
		ok    bool
	}
	results := make([]nodeOwn, len(mons))
	sem := make(chan struct{}, 8)
	var owg sync.WaitGroup
	for i, m := range mons {
		owg.Add(1)
		go func(idx int, mon string) {
			defer owg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var own map[string]map[string]any
			asked := time.Now()
			res, err := getContext(ctx, mon+"/ownership")
			fmt.Fprintf(os.Stderr, "audit ownership %s: %s %v\n", mon, time.Since(asked).Round(time.Millisecond), err)
			if err != nil {
				return
			}
			err = json.NewDecoder(res.Body).Decode(&own)
			res.Body.Close()
			if err != nil || res.StatusCode != 200 || own == nil {
				return
			}
			var status struct{ Items int64 }
			if res, err := getContext(ctx, mon+"/status"); err == nil {
				_ = json.NewDecoder(res.Body).Decode(&status)
				res.Body.Close()
			}
			results[idx] = nodeOwn{m: mon, own: own, items: status.Items, ok: true}
		}(i, m)
	}
	owg.Wait()
	owners := map[string][]string{}
	report.NodesAnswering, report.NodesGone, report.ItemsHeld = 0, 0, 0
	for _, r := range results {
		if !r.ok {
			continue
		}
		report.NodesAnswering++
		for collection, locations := range r.own {
			for location := range locations {
				z := collection + "/" + location
				owners[z] = append(owners[z], r.m)
			}
		}
		report.ItemsHeld += r.items
		report.PerNodeItems[r.m] = int(r.items)
	}
	var zones []string
	for z, o := range owners {
		if len(o) > 1 {
			zones = append(zones, z)
		}
	}
	report.Zones = len(owners)
	report.DoubleOwned = len(zones)
	sort.Strings(zones)
	report.DoubleSample = zones[:min(len(zones), 20)]
}

func auditTotals(ctx context.Context, nodes []string, tok string, ackedBy map[string]int64, report *auditReport) bool {
	type totalJob struct {
		idx        int
		collection string
		want       int64
	}
	totalJobs := make(chan totalJob, len(ackedBy))
	totalIdx := 0
	for collection, want := range ackedBy {
		totalJobs <- totalJob{idx: totalIdx, collection: collection, want: want}
		totalIdx++
	}
	close(totalJobs)
	totalsStarted := time.Now()
	var totalWG sync.WaitGroup
	var mu sync.Mutex
	stale := 0
	for w := 0; w < 8; w++ {
		totalWG.Add(1)
		go func() {
			defer totalWG.Done()
			for job := range totalJobs {
				got, err := collectionTotalContext(ctx, nodes[job.idx%len(nodes)], tok, job.collection, true)
				mu.Lock()
				report.Collections++
				if errors.Is(err, indexus.ErrReadChanged) {
					stale++
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "audit total %s: %v\n", job.collection, err)
				}
				report.recordTotal(job.collection, job.want, got, err)
				mu.Unlock()
			}
		}()
	}
	totalWG.Wait()
	fmt.Fprintf(os.Stderr, "audit totals done: %d collections in %s, mismatches %d, unavailable %d\n", report.Collections, time.Since(totalsStarted).Round(time.Millisecond), len(report.CountMismatches), len(report.TotalErrors))

	return stale > 0 && stale == len(report.TotalErrors)
}

type auditMonitorPool struct {
	candidates []string
	file       string
	seen       []string
}

func (p *auditMonitorPool) refresh() error {
	if p.file != "" {
		raw, err := os.ReadFile(p.file)
		if err != nil {
			return err
		}
		p.candidates = append(p.candidates, split(strings.ReplaceAll(string(raw), "\n", ","))...)
	}
	sort.Strings(p.candidates)
	p.candidates = slices.Compact(p.candidates)
	return nil
}

func (p *auditMonitorPool) census(ctx context.Context) (map[string]string, int, error) {
	if err := p.refresh(); err != nil {
		return nil, 0, err
	}
	alive, moving, err := surveyAuditSlots(ctx, p.candidates)
	if err != nil {
		return nil, 0, err
	}
	if len(alive) == 0 {
		return nil, 0, errors.New("no monitor can certify audit settlement")
	}
	for mon := range alive {
		p.seen = append(p.seen, mon)
	}
	sort.Strings(p.seen)
	p.seen = slices.Compact(p.seen)
	return alive, moving, nil
}

func (p *auditMonitorPool) quiet(ctx context.Context, wait bool) (map[string]string, int, int, error) {
	for polls := 1; ; polls++ {
		fleet, moving, err := p.census(ctx)
		if err != nil || moving == 0 {
			return fleet, moving, polls, err
		}
		if !wait {
			return nil, moving, polls, fmt.Errorf("audit still observes %d transfer/join indicators", moving)
		}
		if err := auditBackoff(ctx); err != nil {
			return nil, moving, polls, fmt.Errorf("audit transfers did not settle: %w", err)
		}
	}
}

// The one settlement budget covers slot discovery, telemetry, every ownership
// and deep read, all retries and the final census. A numeric mismatch is final.
func auditFinal(ctx context.Context, report *auditReport, pool auditMonitorPool, nodes []string, tok string, ackedBy map[string]int64, settle time.Duration) (int, error) {
	if settle < 0 {
		return 0, fmt.Errorf("negative audit settlement duration")
	}
	if settle > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settle)
		defer cancel()
	}
	started := time.Now()
	defer func() { report.SettlementWaitMS = time.Since(started).Milliseconds() }()
	initial := *report
	retries, polls, ownershipRetries := 0, 0, 0
	var reason string
	for {
		before, moving, n, err := pool.quiet(ctx, settle > 0)
		polls += n
		report.SettlementPolls, report.MovingAtOwnership = polls, moving
		if err != nil {
			return 0, err
		}

		*report = initial
		report.FinalRetries, report.LastFinalRetry, report.SettlementPolls, report.OwnershipRetries = retries, reason, polls, ownershipRetries
		report.PerNodeItems = map[string]int{}
		mons := slices.Sorted(maps.Keys(before))
		auditOwnership(ctx, mons, report)
		stale := auditTotals(ctx, nodes, tok, ackedBy, report)
		after, active, err := pool.census(ctx)
		polls++
		report.SettlementPolls, report.MovingAtOwnership = polls, active
		if err != nil {
			return 0, err
		}
		report.NodesGone = len(pool.seen) - len(before)
		switch {
		case len(report.CountMismatches) > 0 || len(report.TotalErrors) > 0 && !stale:
			return len(pool.seen), nil
		case !maps.Equal(before, after):
			reason = "monitor membership changed during final verification"
		case active > 0:
			reason = fmt.Sprintf("%d transfer/join indicators reopened during final verification", active)
		case stale:
			reason = "inventory changed during paginated read"
		case report.DoubleOwned > 0 && ownershipRetries == 0 && settle > 0:
			reason = "rechecking duplicate ownership in a quiet cluster"
			ownershipRetries++
		default:
			return len(pool.seen), nil
		}
		retries++
		report.LastFinalRetry = reason
		if settle == 0 {
			return 0, errors.New(reason)
		}
		if err := auditBackoff(ctx); err != nil {
			return 0, fmt.Errorf("%s: %w", reason, err)
		}
	}
}
