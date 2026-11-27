package zonelog

import (
	"github.com/indexus/go-indexus-core/internal/testkit"
	"math/rand"
	"testing"
)

// The engine and storage handoff tests share the same power-loss device.
// Adapters only bridge Go's concrete return types to zonelog.File.
type memFS struct {
	*testkit.PowerFS
	release chan struct{}
}

func newMemFS() *memFS                                  { return &memFS{PowerFS: testkit.NewPowerFS()} }
func (f *memFS) Create(name string) (File, error)       { return f.PowerFS.Create(name) }
func (f *memFS) Open(name string) (File, error)         { return f.PowerFS.Open(name) }
func (f *memFS) written(ext string) int64               { return f.Written(ext) }
func (f *memFS) size(ext string) int64                  { return f.Size(ext) }
func (f *memFS) holdSync() chan struct{}                { held := f.HoldSync(); f.release = f.Release; return held }
func (f *memFS) crash(rng *rand.Rand, torn bool) *memFS { return &memFS{PowerFS: f.Crash(rng, torn)} }

// writeUntilPowerOff applies the crash workload from batch from until an
// Apply fails or count batches are in, and returns the last one acknowledged.
func writeUntilPowerOff(fs *memFS, from, count int) (acked int) {
	opt := crashOptions
	opt.FS = fs
	acked = from - 1
	s, err := Open("", opt)
	if err != nil {
		return acked
	}
	for i := from; i < from+count; i++ {
		if err := s.Apply(crashBatch(i)); err != nil {
			break
		}
		acked = i
	}
	s.work.Wait()
	return acked
}

// AUTH-4. The power goes off at a random change to the disk: during an
// append, a flush, a merge, a manifest rename, a log rotation, or the open
// itself. What was not synced is lost or torn, and a rename without a
// directory sync is undone. The store reopens to a whole number of batches
// that includes every acknowledged one; then it takes more writes and loses
// power again.
func TestPowerLossKeepsAcknowledgedBatches(t *testing.T) {
	const batches = 500
	dry := newMemFS()
	if acked := writeUntilPowerOff(dry, 0, batches); acked != batches-1 {
		t.Fatalf("dry run stopped at %d", acked)
	}
	rounds := 60
	if testing.Short() {
		rounds = 10
	}
	rng := rand.New(rand.NewSource(11))
	opt := crashOptions
	for round := range rounds {
		fs := newMemFS()
		fs.FailAt = 1 + rng.Intn(dry.Ops)
		acked := writeUntilPowerOff(fs, 0, batches)
		for again := range 3 {
			opt.FS = fs.crash(rng, (round+again)%2 == 1)
			p := reopenAndCheck(t, "", opt, acked)
			fs = opt.FS.(*memFS)
			fs.FailAt = fs.Ops + 1 + rng.Intn(dry.Ops/4)
			acked = max(p, writeUntilPowerOff(fs, p+1, batches/4))
		}
	}
}
