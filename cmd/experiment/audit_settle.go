package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// settlement prints one census for the pre-audit observation window. The
// audit itself repeats the census around its ownership and total checks.
func settlement(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: experiment settlement MONITORS_FILE")
	}
	pool := auditMonitorPool{file: args[0]}
	_, moving, err := pool.census(ctx)
	if err != nil {
		return err
	}
	fmt.Println(moving)
	return nil
}

// Unknown telemetry cannot certify quiet. Counts include handoffs, frozen
// transfers and joining processes; overlapping indicators need only be zero.
func surveyAuditSlots(ctx context.Context, mons []string) (map[string]string, int, error) {
	type observation struct {
		alive  bool
		id     string
		moving int
	}
	counts := make([]observation, len(mons))
	failures := make([]error, len(mons))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, mon := range mons {
		wg.Add(1)
		go func(i int, mon string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				failures[i] = ctx.Err()
				return
			}
			call, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(call, http.MethodGet, mon+"/metrics?view=settlement", nil)
			if err != nil {
				failures[i] = err
				return
			}
			res, err := client.Do(req)
			if err != nil {
				if errors.Is(err, syscall.ECONNREFUSED) {
					return
				}
				failures[i] = err
				return
			}
			defer res.Body.Close()
			var m struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Handoffs struct {
					Open struct{ Count *int } `json:"open"`
				} `json:"handoffs"`
				Frozen  []string `json:"frozen_transfers"`
				Joining *bool    `json:"joining"`
			}
			err = json.NewDecoder(res.Body).Decode(&m)
			if res.StatusCode != 200 || err != nil || m.Handoffs.Open.Count == nil || *m.Handoffs.Open.Count < 0 || m.Joining == nil {
				failures[i] = fmt.Errorf("audit cannot observe transfers at %s: status=%d decode=%v", mon, res.StatusCode, err)
				return
			}
			counts[i] = observation{alive: true, id: m.Name + "/" + m.ID, moving: *m.Handoffs.Open.Count + len(m.Frozen)}
			if *m.Joining {
				counts[i].moving++
			}
		}(i, mon)
	}
	wg.Wait()
	total := 0
	alive := map[string]string{}
	for i, n := range counts {
		if n.alive {
			alive[mons[i]] = n.id
			total += n.moving
		}
	}
	return alive, total, errors.Join(failures...)
}

// An unavailable total has no numeric value. Preserve the failure without
// representing it as an empty collection or changing the audit's pass criteria.
func (r *auditReport) recordTotal(collection string, want, got int64, err error) {
	if err != nil {
		if r.TotalErrors == nil {
			r.TotalErrors = map[string]string{}
		}
		r.TotalErrors[collection] = err.Error()
		return
	}
	if got != want {
		if r.CountMismatches == nil {
			r.CountMismatches = map[string][2]int64{}
		}
		r.CountMismatches[collection] = [2]int64{want, got}
	}
}

func writeAuditReport(out string, report auditReport) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(out, "audit.json"), raw, 0644); err != nil {
		return err
	}
	fmt.Println(string(raw))
	if !report.Pass {
		return fmt.Errorf("audit failed")
	}
	return nil
}

// All audit stages inherit the command's cancellation, including backoff.
func getContext(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}
func auditBackoff(ctx context.Context) error {
	return waitAuditInterval(ctx, 200*time.Millisecond)
}

func waitAuditInterval(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
