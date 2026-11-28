package mesh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
)

// Only unfenced source proposals queue. The host sequencer is locked just
// while checking the oldest live ticket and taking a lane. Its file is eight
// bytes. A contender's flock makes its ticket disappear logically on exit.
type copyQueue struct {
	dir string
	// A bounded admission may time out before a long holder releases. Carry
	// one unserved turn to this vnode's next admission. A vnode between
	// attempts has no live ticket and hence cannot hold another vnode back.
	retryRank atomic.Uint64
}

type copyTicket struct {
	file, before *os.File
	rank         uint64
}

func (t *copyTicket) remove() {
	if t.before != nil {
		_ = t.before.Close()
		t.before = nil
	}
	if t.file != nil {
		_ = os.Remove(t.file.Name())
		_ = t.file.Close()
		t.file = nil
	}
}

func (q *copyQueue) timedOut(t *copyTicket) {
	if t.file == nil {
		return
	}
	t.remove()
	for old := q.retryRank.Load(); old == 0 || t.rank < old; old = q.retryRank.Load() {
		if q.retryRank.CompareAndSwap(old, t.rank) {
			return
		}
	}
}

// A waiter watches only its immediate live predecessor's flock. A process
// exit wakes its successor even if the dead ticket was never unlinked.
func (t *copyTicket) blocked() (bool, error) {
	if t.before == nil {
		return false, nil
	}
	err := syscall.Flock(int(t.before.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	t.before.Close()
	t.before = nil
	return false, err
}

// Run under the sequencer on registration, predecessor loss, or grant.
// os.ReadDir orders the fixed-width ranks. Release removes the name before
// unlocking its file. An extant but unlocked ticket belongs to a dead caller.
func (q *copyQueue) predecessor(rank uint64) (*os.File, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		candidate, err := strconv.ParseUint(entries[i].Name(), 10, 64)
		if err != nil || candidate >= rank {
			continue
		}
		f, err := os.OpenFile(filepath.Join(q.dir, entries[i].Name()), os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return f, nil
		}
		if err == nil {
			err = os.Remove(f.Name())
		}
		f.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, nil
}

func nextCopyRank(sequencer *os.File) (uint64, error) {
	var data [8]byte
	n, err := sequencer.ReadAt(data[:], 0)
	if err != nil && !(err == io.EOF && n == 0) {
		return 0, err
	}
	rank := binary.BigEndian.Uint64(data[:]) + 1
	if rank == 0 {
		return 0, errors.New("host copy queue exhausted its ticket numbers")
	}
	binary.BigEndian.PutUint64(data[:], rank)
	_, err = sequencer.WriteAt(data[:], 0)
	return rank, err
}

// Admit the oldest *live* requester. Later sources cannot race its poll,
// even if the previous holder releases and immediately asks for more work.
func (b *CopyBudget) takeQueued(transfer string, t *copyTicket) (*os.File, bool, error) {
	q := b.queue
	observedPredecessor := t.before != nil
	if blocked, err := t.blocked(); blocked || err != nil {
		return nil, false, err
	}
	gate, err := b.tryHostLock("source-sequence")
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer gate.Close()

	// A free lane can be granted immediately if no older live rank exists.
	// A holder of a saved rank may have rejoined since our last observation,
	// so check the queue at the grant itself, while still under the sequencer.
	rank := t.rank
	if t.file == nil {
		rank = q.retryRank.Swap(0)
		if rank == 0 {
			rank = ^uint64(0)
		}
	}
	held, shared := b.take(transfer)
	grant := held != nil || shared
	if grant || t.file == nil || observedPredecessor {
		t.before, err = q.predecessor(rank)
		if err != nil {
			releaseCopyFile(held)
			return nil, false, err
		}
		if grant && t.before == nil {
			t.remove()
			q.retryRank.Store(0)
			return held, shared, nil
		}
		releaseCopyFile(held)
	}
	if t.file == nil {
		if rank == ^uint64(0) {
			rank, err = nextCopyRank(gate)
			if err != nil {
				return nil, false, err
			}
		}
		f, err := os.OpenFile(filepath.Join(q.dir, fmt.Sprintf("%020d", rank)), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, false, err
		}
		t.file, t.rank = f, rank
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.remove()
			return nil, false, err
		}
	}
	return nil, false, nil
}
