package summary

import (
	"fmt"
	"testing"
)

// SUM-2: the steady-state boundary poll asks O(log N) hosts per tick, and the
// rotation still reaches every host within ⌈N/window⌉ ticks.
func TestSUM2_BoundedPollRotatesOverEveryHost(t *testing.T) {
	for _, n := range []int{1, 3, 4, 15, 63, 1000} {
		hosts := make([]string, n)
		for i := range hosts {
			hosts[i] = fmt.Sprint("h", i)
		}
		window := summaryPollWindow(n)
		if n >= 16 && window*4 > n {
			t.Fatalf("n=%d: window %d is not bounded", n, window)
		}
		if window < min(n, 4) {
			t.Fatalf("n=%d: window %d too small", n, window)
		}
		seen := map[string]int{}
		var offset uint64
		ticks := (n + window - 1) / window
		for tick := 0; tick < ticks; tick++ {
			for _, h := range rotateHosts(hosts, offset, window) {
				seen[h]++
			}
			offset += uint64(window)
		}
		if len(seen) != n {
			t.Fatalf("n=%d: %d of %d hosts polled in %d ticks", n, len(seen), n, ticks)
		}
	}
}
