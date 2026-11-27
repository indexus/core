package zonelog

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// crashBatch is batch i of the crash workload: three records that replace
// and delete older ones, and a progress record naming i. One atomic frame.
func crashBatch(i int) []Record {
	rng := rand.New(rand.NewSource(int64(i)))
	batch := []Record{{Collection: testCollection, Location: "@", ID: "progress", Metrics: []float64{float64(i)}}}
	for j := range 3 {
		r := Record{Collection: testCollection, Location: randomLocation(rng), ID: fmt.Sprint(rng.Intn(300))}
		if rng.Intn(5) == 0 {
			r.Deleted = true
		} else {
			r.Metrics = []float64{float64(i), float64(j)}
			r.Payload = []byte(strings.Repeat("p", rng.Intn(2000)))
		}
		batch = append(batch, r)
	}
	return batch
}

// reopenAndCheck opens dir and requires its content to be exactly batches
// 0..p for some p >= acked: nothing acknowledged is lost, nothing torn or
// partial is visible. It returns p.
func reopenAndCheck(t *testing.T, dir string, opt Options, acked int) int {
	t.Helper()
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	p := -1
	if r, ok, _ := s.Get(testCollection, "@", "progress"); ok {
		p = int(r.Metrics[0])
	}
	if p < acked {
		t.Fatalf("acknowledged batch %d is lost: store is at %d", acked, p)
	}
	o := oracle{}
	for i := 0; i <= p; i++ {
		for _, m := range crashBatch(i) {
			o.apply(m)
		}
	}
	checkStore(t, s, o, []string{"@", "0", "L", "0L", "LgL"})
	return p
}

var crashOptions = Options{MemoryBytes: 512 << 10, LogRetain: 256 << 10, BlobBytes: 1000}

// The log cut at any byte, with or without garbage after the cut, reopens to
// a whole number of batches that includes every batch acknowledged before
// the cut.
func TestTornLogKeepsAcknowledgedBatches(t *testing.T) {
	dir := t.TempDir()
	opt := crashOptions
	opt.NoSync = true
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	type ack struct {
		file string
		size int64
	}
	var acks []ack
	for i := range 1200 {
		if err := s.Apply(crashBatch(i)); err != nil {
			t.Fatal(err)
		}
		last := s.logs[len(s.logs)-1]
		acks = append(acks, ack{filepath.Join(dir, last.name), last.size})
	}
	s.work.Wait()
	tail := acks[len(acks)-1].file
	whole, _ := os.ReadFile(tail)
	rng := rand.New(rand.NewSource(7))
	for round := range 40 {
		cut := rng.Int63n(int64(len(whole)) + 1)
		copyDir := t.TempDir()
		names, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, name := range names {
			raw, _ := os.ReadFile(name)
			if name == tail {
				raw = raw[:cut]
				if round%2 == 1 {
					raw = append(raw, whole[cut:min(int64(len(whole)), cut+int64(rng.Intn(64)))]...)
					raw = append(raw, 0xff, 0x00, 0x13, 0x37)
				}
			}
			os.WriteFile(filepath.Join(copyDir, filepath.Base(name)), raw, 0o600)
		}
		acked := -1
		for i, a := range acks {
			if a.file != tail || a.size <= cut {
				acked = i
			}
		}
		// A segment may already hold batches past the cut: more than acked is fine.
		reopenAndCheck(t, copyDir, opt, acked)
	}
}

// A writer process is killed at random moments, during appends, flushes and
// merges, with real fsync. After each kill the store reopens to a whole
// number of batches that includes every acknowledged one.
func TestKillDuringWritesKeepsAcknowledgedBatches(t *testing.T) {
	if dir := os.Getenv("ZONELOG_CRASH_DIR"); dir != "" {
		crashWriter(dir)
		return
	}
	dir := t.TempDir()
	rounds := 12
	if testing.Short() {
		rounds = 3
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	next := 0
	for round := range rounds {
		cmd := exec.Command(os.Args[0], "-test.run", "TestKillDuringWritesKeepsAcknowledgedBatches")
		cmd.Env = append(os.Environ(), "ZONELOG_CRASH_DIR="+dir, "ZONELOG_CRASH_FROM="+strconv.Itoa(next))
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		acked := next - 1
		killAt := time.Now().Add(time.Duration(100+rng.Intn(700)) * time.Millisecond)
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			if n, err := strconv.Atoi(lines.Text()); err == nil {
				acked = n
			}
			if time.Now().After(killAt) {
				cmd.Process.Signal(syscall.SIGKILL)
				break
			}
		}
		// Acks printed before the kill but not yet read still count.
		for lines.Scan() {
			if n, err := strconv.Atoi(lines.Text()); err == nil {
				acked = n
			}
		}
		cmd.Wait()
		p := reopenAndCheck(t, dir, crashOptions, acked)
		t.Logf("round %d: killed after ack %d, store at batch %d", round, acked, p)
		next = p + 1
	}
}

func crashWriter(dir string) {
	s, err := Open(dir, crashOptions)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	from, _ := strconv.Atoi(os.Getenv("ZONELOG_CRASH_FROM"))
	for i := from; ; i++ {
		if err := s.Apply(crashBatch(i)); err != nil {
			fmt.Println("apply:", err)
			os.Exit(1)
		}
		fmt.Println(i)
	}
}
