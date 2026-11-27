package convergent

import (
	"fmt"
	"testing"
)

// lab drives three replicas with instant delivery and one shared clock, and
// can cut one member off. It makes U6 and U7 exact step-by-step stories.
type lab struct {
	now     int64
	reps    map[Member]*Replica
	order   []Member
	blocked map[Member]bool
	acks    []Ack
}

func newLab() *lab {
	l := &lab{reps: map[Member]*Replica{}, order: []Member{"o", "r1", "r2"}, blocked: map[Member]bool{}}
	for _, m := range l.order {
		l.reps[m] = Open(m, Incarnation(m+"#1"), l.order, DefaultConfig(), nil, 0)
	}
	return l
}

// run advances time by d in 10 ms steps, delivering every message at once
// except to or from a blocked member.
func (l *lab) run(d int64, each func()) {
	for end := l.now + d; l.now < end; {
		l.now += 10
		for _, m := range l.order {
			l.reps[m].Tick(l.now)
		}
		for delivered := true; delivered; {
			delivered = false
			for _, m := range l.order {
				msgs, acks := l.reps[m].TakeOutbox()
				l.acks = append(l.acks, acks...)
				for _, msg := range msgs {
					if l.blocked[msg.From] || l.blocked[msg.To] {
						continue
					}
					l.reps[msg.To].Receive(l.now, msg)
					delivered = true
				}
			}
		}
		if each != nil {
			each()
		}
	}
}

// U6: a delete is pruned only once every member holds what the owner
// holds. Here r2, cut off, wrote k before the owner's delete of k; pruning
// the delete while r2 is away would let r2's older write come back.
func u6() error {
	l := newLab()
	l.run(1000, nil)
	l.blocked["r2"] = true
	l.run(4000, nil) // r2 enters fallback
	if !l.reps["r2"].InFallback() {
		return fmt.Errorf("r2 not in fallback")
	}
	if err := l.reps["r2"].Write(l.now, "w", "k", false, 5); err != nil {
		return err
	}
	if err := l.reps["o"].Write(l.now, "d", "k", true, 0); err != nil {
		return err
	}
	l.run(5000, nil)
	l.blocked["r2"] = false
	l.run(5000, nil)
	o := l.reps["o"].State
	var del Version
	for _, a := range l.acks {
		if a.RequestID == "d" {
			del = a.Result
		}
	}
	if !o.Has(del) {
		return fmt.Errorf("the delete lost against an older write: k is %+v", o.Records["k"])
	}
	if _, ok := o.Records["k"]; ok {
		return fmt.Errorf("delete of k never pruned")
	}
	for _, m := range []Member{"r1", "r2"} {
		if !l.reps[m].State.SameData(o) {
			return fmt.Errorf("%s differs from the owner", m)
		}
	}
	return nil
}

func TestU6PruneAfterAll(t *testing.T) {
	if err := u6(); err != nil {
		t.Fatal(err)
	}
}

func TestU6DetectsEarlyPrune(t *testing.T) {
	withFault(t, PruneWithoutAll, func() {
		if u6() == nil {
			t.Fatal("U6 passed with pruning after one replica")
		}
	})
}

// U7: while a replica holds a fallback write the owner lacks, the zone is
// never clean, at every step; once repaired it is clean again.
func u7() error {
	l := newLab()
	l.run(1000, nil)
	if !l.reps["o"].Clean(l.now) {
		return fmt.Errorf("healthy zone not clean")
	}
	l.blocked["r2"] = true
	var bad error
	written := false
	l.run(8000, func() {
		r2 := l.reps["r2"]
		if r2.InFallback() && !written {
			if err := r2.Write(l.now, "f", "k", false, 1); err == nil {
				written = true
			}
		}
		if written && bad == nil && l.reps["o"].Clean(l.now) && !l.reps["o"].State.Vector.Covers(Dot{"r2#1", 1}) {
			bad = fmt.Errorf("t=%d: clean while r2 holds an unmerged write", l.now)
		}
	})
	if !written {
		return fmt.Errorf("r2 never accepted a fallback write")
	}
	if bad != nil {
		return bad
	}
	l.blocked["r2"] = false
	l.run(2000, nil)
	if !l.reps["o"].Clean(l.now) {
		return fmt.Errorf("not clean after repair")
	}
	return nil
}

func TestU7CleanRule(t *testing.T) {
	if err := u7(); err != nil {
		t.Fatal(err)
	}
}

func TestU7DetectsCleanWithoutFresh(t *testing.T) {
	withFault(t, CleanWithoutFresh, func() {
		if u7() == nil {
			t.Fatal("U7 passed without fresh replies")
		}
	})
}
