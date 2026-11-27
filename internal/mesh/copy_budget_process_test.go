package mesh

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Unlike the handle-level tests, these holders have separate process tables.
func TestCopyBudgetProcessHelper(t *testing.T) {
	dir := os.Getenv("INDEXUS_COPY_PROCESS_TEST")
	if dir == "" {
		return
	}
	slots, err := strconv.Atoi(os.Getenv("INDEXUS_COPY_PROCESS_SLOTS"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCopyBudget(dir, slots)
	if err != nil {
		t.Fatal(err)
	}
	held, shared := b.take(os.Getenv("INDEXUS_COPY_PROCESS_OP"))
	if held == nil || shared {
		t.Fatalf("child did not acquire: shared=%v", shared)
	}
	defer held.Close()
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestCopyBudgetSeparateProcessesAndCrash(t *testing.T) {
	for _, slots := range []int{1, 2} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			dir := t.TempDir()
			b, err := NewCopyBudget(dir, slots)
			if err != nil {
				t.Fatal(err)
			}
			var children []*exec.Cmd
			t.Cleanup(func() {
				for _, c := range children {
					_ = c.Process.Kill()
					_ = c.Wait()
				}
			})
			for i := range slots {
				c := exec.Command(os.Args[0], "-test.run=^TestCopyBudgetProcessHelper$", "-test.timeout=15s")
				c.Env = append(os.Environ(), "INDEXUS_COPY_PROCESS_TEST="+dir, "INDEXUS_COPY_PROCESS_SLOTS="+strconv.Itoa(slots), "INDEXUS_COPY_PROCESS_OP=op"+strconv.Itoa(i))
				c.Stderr = os.Stderr
				pipe, err := c.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				input, err := c.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer input.Close()
				if err = c.Start(); err != nil {
					t.Fatal(err)
				}
				children = append(children, c)
				line, err := bufio.NewReader(pipe).ReadString('\n')
				if err != nil || line != "locked\n" {
					t.Fatalf("child handshake %q: %v", line, err)
				}
			}
			if f, shared := b.take("other"); f != nil || shared {
				if f != nil {
					f.Close()
				}
				t.Fatal("extra operation entered a full host budget")
			}
			if f, shared := b.take("op0"); f != nil || !shared {
				if f != nil {
					f.Close()
				}
				t.Fatal("co-resident receiver did not share the live source")
			}
			dead := children[0]
			if err := dead.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = dead.Wait()
			children = children[1:]
			f, shared := b.take("replacement")
			if f == nil || shared {
				t.Fatal("crashed process retained its host slot")
			}
			f.Close()
		})
	}
}
