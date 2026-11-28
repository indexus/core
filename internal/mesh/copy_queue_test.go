package mesh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/storage"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// The parent explicitly orders polls from distinct OS processes. Cross-process
// fairness must hold even when the previous holder gets to poll first.
func TestCopyQueueProcess(t *testing.T) {
	dir := os.Getenv("INDEXUS_TEST_COPY_QUEUE")
	if dir == "" {
		t.Skip("child of TestCopyQueueDoesNotBarge")
	}
	b, err := NewCopyBudget(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	var ticket copyTicket
	var slot *os.File
	for input := bufio.NewScanner(os.Stdin); input.Scan(); {
		reply := "ok"
		switch input.Text() {
		case "try":
			if slot != nil {
				t.Fatal("already holding a slot")
			}
			slot, _, err = b.takeQueued("", &ticket)
			if err != nil {
				t.Fatal(err)
			}
			reply = "waiting"
			if slot != nil {
				reply = "admitted"
			}
		case "release":
			releaseCopyFile(slot)
			slot = nil
		case "timeout":
			b.queue.timedOut(&ticket)
		default:
			t.Fatal("unknown action")
		}
		fmt.Println(reply)
	}
}

type copyQueueChild struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
}

func startCopyChild(t *testing.T, dir string) *copyQueueChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCopyQueueProcess$")
	cmd.Env = append(os.Environ(), "INDEXUS_TEST_COPY_QUEUE="+dir)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &copyQueueChild{cmd: cmd, in: in, out: bufio.NewScanner(out)}
	t.Cleanup(func() {
		in.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return c
}

func (p *copyQueueChild) act(t *testing.T, command, want string) {
	t.Helper()
	if _, err := fmt.Fprintln(p.in, command); err != nil {
		t.Fatal(err)
	}
	if !p.out.Scan() || p.out.Text() != want {
		t.Fatalf("%s: got %q (%v), want %q", command, p.out.Text(), p.out.Err(), want)
	}
}

func TestCopyQueueDoesNotBarge(t *testing.T) {
	dir := t.TempDir()
	hot := startCopyChild(t, dir)
	old := startCopyChild(t, dir)
	third := startCopyChild(t, dir)
	hot.act(t, "try", "admitted")
	old.act(t, "try", "waiting")
	third.act(t, "try", "waiting")
	hot.act(t, "release", "ok")
	hot.act(t, "try", "waiting") // would acquire with the previous greedy flock
	third.act(t, "try", "waiting")
	old.act(t, "try", "admitted")
	old.act(t, "release", "ok")
	hot.act(t, "try", "waiting")
	third.act(t, "try", "admitted")

	// A bounded wait ends. Hot leaves and retries while third still holds the
	// lane and old has arrived again. Its original turn survives the timeout.
	old.act(t, "try", "waiting")
	hot.act(t, "timeout", "ok")
	hot.act(t, "try", "waiting")
	third.act(t, "release", "ok")
	third.act(t, "try", "waiting")
	old.act(t, "try", "waiting")
	hot.act(t, "try", "admitted")

	// Killing a waiting process releases its ticket flock without cleanup.
	// The next waiter reaps the orphan and progresses on the next release.
	if err := old.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = old.cmd.Wait()
	hot.act(t, "release", "ok")
	hot.act(t, "try", "waiting")
	third.act(t, "try", "admitted")
}

func TestWarmAdmissionRespectsAnOlderSourceWhileUrgentRepairBypasses(t *testing.T) {
	b := hostBudgets(t, 1, 3)
	held, _, err := b[0].takeQueued("", &copyTicket{})
	if err != nil || held == nil {
		t.Fatalf("initial slot %v %v", held, err)
	}
	var old copyTicket
	defer old.remove()
	if f, _, e := b[1].takeQueued("", &old); f != nil || e != nil || old.file == nil {
		t.Fatalf("registrant %v %v", f, e)
	}
	releaseCopyFile(held)
	// No other goroutine polls. Admission cannot claim the older source's
	// free lane even with the entire timeout window available to it.
	ctx := context.Background()
	if f, e := b[2].reserveFile(ctx, "", time.Millisecond, true); !errors.Is(e, storage.ErrNotReady) || f != nil {
		releaseCopyFile(f)
		t.Fatalf("new source overtook old: %v %v", f, e)
	}
	gate, err := b[1].tryHostLock("source-sequence")
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	// A fenced repair neither reads the queue nor waits for its sequencer.
	f, e := b[2].reserveFile(ctx, "urgent", 0, false)
	if e != nil || f == nil {
		t.Fatalf("urgent work blocked by the queue: %v %v", f, e)
	}
	releaseCopyFile(f)
}

// Reproduce six resident vnodes and thirteen consecutive grains. Every
// release is followed by the just-served donor polling first, the adversarial
// order observed on the overloaded host.
func TestCopyQueueRotatesSixBusyDonors(t *testing.T) {
	dir := t.TempDir()
	donors := make([]*copyQueueChild, 6)
	for i := range donors {
		donors[i] = startCopyChild(t, dir)
	}
	donors[0].act(t, "try", "admitted")
	for _, d := range donors[1:] {
		d.act(t, "try", "waiting")
	}
	grants := make([]int, len(donors))
	for i := range 13 {
		current, next := i%len(donors), (i+1)%len(donors)
		donors[current].act(t, "release", "ok")
		donors[current].act(t, "try", "waiting")
		for j, d := range donors {
			if j != current && j != next {
				d.act(t, "try", "waiting")
			}
		}
		donors[next].act(t, "try", "admitted")
		grants[next]++
	}
	t.Logf("admissions on the thirteen releases: %v", grants)
}
