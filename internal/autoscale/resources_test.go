package autoscale

import (
	"slices"
	"strings"
	"testing"
)

func TestMemoryPressureDoesNotTreatMappedCacheAsPrivateMemory(t *testing.T) {
	const gib = uint64(1 << 30)
	process := map[string]uint64{"VmRSS": 6 * gib, "RssAnon": gib / 2, "RssFile": 11 * gib / 2}
	host := map[string]uint64{"MemTotal": 8 * gib, "MemAvailable": 6 * gib}
	s := memoryPressure(process, host, 8*gib)
	if s.RSS != 6*gib || s.NonFileRSS != gib/2 || s.FileRSS != 11*gib/2 || s.MemPct != 25 {
		t.Fatalf("mapped cache caused false pressure: %+v", s)
	}
}

func TestMemoryPressureRetainsAnonymousSharedAndHostGuards(t *testing.T) {
	const gib = uint64(1 << 30)
	for _, tc := range []struct {
		name          string
		process, host map[string]uint64
		limit         uint64
		want          float64
	}{
		{"private", map[string]uint64{"RssAnon": 6 * gib}, map[string]uint64{"MemTotal": 8 * gib, "MemAvailable": 6 * gib}, 8 * gib, 75},
		{"host", map[string]uint64{"RssAnon": gib / 2}, map[string]uint64{"MemTotal": 8 * gib, "MemAvailable": gib / 2}, 8 * gib, 93.75},
		{"shared/cgroup", map[string]uint64{"RssAnon": gib / 4, "RssShmem": gib / 8}, map[string]uint64{"MemTotal": 8 * gib, "MemAvailable": 8 * gib}, 2 * gib, 18.75},
		{"unknown breakdown", map[string]uint64{"VmRSS": 6 * gib}, map[string]uint64{"MemTotal": 8 * gib}, 8 * gib, 75},
		{"unavailable", nil, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if s := memoryPressure(tc.process, tc.host, tc.limit); s.MemPct != tc.want {
				t.Fatalf("pressure = %g, want %g", s.MemPct, tc.want)
			}
		})
	}
}

func TestProcMemoryBytes(t *testing.T) {
	values := parseProcBytes(strings.NewReader("Name:\tnode\nVmRSS: 128 kB\nRssAnon: 32 kB\nRssFile: 96 kB\nThreads: 8\nBroken: nope kB\n"))
	if len(values) != 3 || values["VmRSS"] != 128*1024 || values["RssAnon"] != 32*1024 {
		t.Fatalf("invalid proc memory parse: %v", values)
	}
}

func TestDefaultHeapLimitReservesProcessHeadroom(t *testing.T) {
	for _, tc := range []struct {
		host uint64
		want int64
	}{
		{0, 1536 << 20},
		{1 << 30, 768 << 20},
		{2 << 30, 1536 << 20},
		{32 << 30, 16 << 30},
		{64 << 30, 32 << 30},
	} {
		if got := defaultHeapLimit(tc.host); got != tc.want {
			t.Fatalf("host %d: got %d, want %d", tc.host, got, tc.want)
		}
	}
}

func TestMemoryBudgetFindsTheServiceCgroup(t *testing.T) {
	for _, tc := range []struct{ data, path string }{
		{"0::/system.slice/indexus-node.service", "/sys/fs/cgroup/system.slice/indexus-node.service/memory.max"},
		{"4:cpu,memory:/worker", "/sys/fs/cgroup/memory/worker/memory.limit_in_bytes"},
		{"0::/../../outside", "/sys/fs/cgroup/outside/memory.max"},
	} {
		if !slices.Contains(cgroupMemoryPaths(tc.data), tc.path) {
			t.Fatalf("cgroup %q: missing %s", tc.data, tc.path)
		}
	}
}

// The node processes of one machine split the heap one alone would take:
// each capped itself at half the host, whatever their number.
func TestHeapShareSplitsTheHostBetweenItsProcesses(t *testing.T) {
	const gib = uint64(1 << 30)
	for _, tc := range []struct {
		host      uint64
		processes int
		want      int64
	}{
		{32 * gib, 1, int64(16 * gib)},
		{32 * gib, 5, int64(16 * gib / 5)},
		{32 * gib, 0, int64(16 * gib)},
		{2 * gib, 8, 256 << 20}, // never below a heap that can run
	} {
		if got := heapShare(tc.host, tc.processes); got != tc.want {
			t.Errorf("host %d GiB, %d processes: %d, want %d", tc.host/gib, tc.processes, got, tc.want)
		}
	}
	if total := 5 * heapShare(32*gib, 5); total > int64(16*gib) {
		t.Fatalf("five processes aim at %d bytes of a 32 GiB host", total)
	}
}
