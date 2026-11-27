package modelcheck

import (
	"fmt"
	"testing"
)

type zonePhase uint8

const (
	zIdle zonePhase = iota
	zFrozen
	zTransferred
	zInstalled
)

type treeWorld struct {
	Left, Right, Parent zonePhase
	DonorAck            [3]bool
	RecvAck             [3]bool
	Cert, Collected     bool
	Crashed             bool
}

func (w treeWorld) allTransferred() bool {
	return w.Left >= zTransferred && w.Right >= zTransferred && w.Parent >= zTransferred
}

func (w treeWorld) invariants() error {
	phases := [...]zonePhase{w.Left, w.Right, w.Parent}
	for i, p := range phases {
		if w.DonorAck[i] && w.RecvAck[i] {
			return fmt.Errorf("ZONE-1: zone %d has two ACK authorities", i)
		}
		if p >= zTransferred && w.DonorAck[i] {
			return fmt.Errorf("ZONE-1: donor ACK after TRANSFERRED zone %d", i)
		}
		if p >= zInstalled && !w.RecvAck[i] && !w.Crashed {
			// receiver may ACK once installed; absence is allowed (no-writer window)
		}
	}
	if w.Parent >= zFrozen && (w.Left < zTransferred || w.Right < zTransferred) {
		return fmt.Errorf("ZONE-2: parent froze before children TRANSFERRED")
	}
	if w.Cert && !w.allTransferred() {
		return fmt.Errorf("MIG-1: region certificate before MigrationSet TRANSFERRED")
	}
	if w.Collected && !w.Cert {
		return fmt.Errorf("MIG-1: residual GC before region certificate")
	}
	return nil
}

func (w treeWorld) step(which int, next zonePhase) (treeWorld, bool) {
	if w.Crashed {
		return w, false
	}
	cur := [...]zonePhase{w.Left, w.Right, w.Parent}[which]
	if next != cur+1 {
		return w, false
	}
	if which == 2 && next == zFrozen && (w.Left < zTransferred || w.Right < zTransferred) {
		return w, false
	}
	switch which {
	case 0:
		w.Left = next
	case 1:
		w.Right = next
	case 2:
		w.Parent = next
	}
	if next == zTransferred {
		w.DonorAck[which] = false
	}
	if next == zInstalled {
		w.RecvAck[which] = true
	}
	return w, true
}

func TestModel_ZoneTreeHandoff(t *testing.T) {
	type key struct {
		Left, Right, Parent zonePhase
		Cert, Collected     bool
		Crashed             bool
	}
	start := treeWorld{DonorAck: [3]bool{true, true, true}}
	queue := []treeWorld{start}
	seen := map[key]bool{}
	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]
		k := key{w.Left, w.Right, w.Parent, w.Cert, w.Collected, w.Crashed}
		if seen[k] {
			continue
		}
		seen[k] = true
		if err := w.invariants(); err != nil {
			t.Fatal(err)
		}
		if w.Crashed {
			n := w
			n.Crashed = false
			queue = append(queue, n)
			continue
		}
		n := w
		n.Crashed = true
		queue = append(queue, n)
		for which := 0; which < 3; which++ {
			for _, next := range []zonePhase{zFrozen, zTransferred, zInstalled} {
				if stepped, ok := w.step(which, next); ok {
					queue = append(queue, stepped)
				}
			}
		}
		if w.allTransferred() && !w.Cert {
			n := w
			n.Cert = true
			queue = append(queue, n)
		}
		if w.Cert && !w.Collected {
			n := w
			n.Collected = true
			queue = append(queue, n)
		}
	}
	if len(seen) < 20 {
		t.Fatalf("explored too little: %d", len(seen))
	}
}
