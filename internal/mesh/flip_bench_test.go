package mesh

import (
	"context"
	"sync"
	"testing"
	"time"
)

// BenchmarkJoinFinalPhase joins a vnode that takes 4 096 zones from one donor
// (NoSync, in-process transport) and reports:
//   - first_install_ms: delay from Join start until the first lot is writable
//   - final_ms: span from the first install-zone request to its last ACK
//   - units: number of installed lots (hold_max is logged in zone transfer progress)
//
// Run with INDEXUS_BENCH_NO_USAGE_SCAN=1 to disable the test-only full usage scan.
func BenchmarkJoinFinalPhase(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		net, _, target := streamFixture(b, 4096)
		var mu sync.Mutex
		var firstRequest, firstInstall, lastInstall time.Time
		installs := 0
		net.setBefore(func(request Request) error {
			if request.Kind == "install-zone" {
				mu.Lock()
				if firstRequest.IsZero() {
					firstRequest = time.Now()
				}
				mu.Unlock()
			}
			return nil
		})
		net.setAfter(func(request Request) error {
			if request.Kind == "install-zone" {
				mu.Lock()
				now := time.Now()
				if firstInstall.IsZero() {
					firstInstall = now
				}
				lastInstall = now
				installs++
				mu.Unlock()
			}
			return nil
		})
		b.StartTimer()
		started := time.Now()
		if err := target.Join(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(firstInstall.Sub(started).Milliseconds()), "first_install_ms")
		b.ReportMetric(float64(lastInstall.Sub(firstRequest).Milliseconds()), "final_ms")
		b.ReportMetric(float64(installs), "units")
	}
}
