package storage

import (
	"context"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/prep"
)

// TestBulkIngestCost times IngestBulk alone, with real fsync, on the batch
// directory $BULKBENCH_BATCH (scripts/research/bulkbench.sh stages one), then the
// metrics walk a summary fold makes over it.
func TestBulkIngestCost(t *testing.T) {
	dir := os.Getenv("BULKBENCH_BATCH")
	if dir == "" {
		t.Skip("BULKBENCH_BATCH names a staged batch")
	}
	work := t.TempDir()
	if w := os.Getenv("BULKBENCH_WORK"); w != "" {
		work = w
	}
	s, err := OpenWithOptions(filepath.Join(work, "node.causal.db"), "D", "bench", 1<<36, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, _ := s.Endpoint("D")
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "bench", Holder: h}); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("BULKBENCH_PROFILE"); path != "" {
		f, _ := os.Create(path)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	m, err := prep.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var batchBytes uint64
	for _, p := range m.Parts {
		batchBytes += p.Bytes
	}
	started := time.Now()
	result, err := s.IngestBulk(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	ingest := time.Since(started)
	started = time.Now()
	rows := 0
	err = s.view(func(tx *Tx) error {
		return tx.walk(nil, nil, "", false, func(_ tree.ZoneKey, r *row) (bool, error) {
			if r != nil {
				rows++
			}
			return true, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	walk := time.Since(started)
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Printf("{\"records\":%d,\"zones\":%d,\"batch_bytes\":%d,\"ingest_s\":%.3f,\"records_per_s\":%.0f,\"mb_per_s\":%.1f,\"walk_s\":%.3f,\"walked\":%d,\"heap_peak_sys\":%d,\"rss_peak\":%s}\n",
		result.Records, result.Zones, batchBytes, ingest.Seconds(), float64(result.Records)/ingest.Seconds(), float64(batchBytes)/1e6/ingest.Seconds(), walk.Seconds(), rows, mem.HeapSys, peakRSS())
}

func peakRSS() string {
	raw, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			var kb int64
			fmt.Sscanf(strings.TrimPrefix(line, "VmHWM:"), "%d", &kb)
			return fmt.Sprint(kb * 1024)
		}
	}
	return "0"
}
