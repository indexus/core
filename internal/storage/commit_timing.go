package storage

import (
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// Commit categories are fixed: recording a transaction never allocates a label
// or retains a request, zone, peer or operation ID.
type commitKind uint8

const (
	commitControl commitKind = iota
	commitWrite
	commitFence
	commitMount
	commitCollect
	commitKindCount
)

var commitKindNames = [...]string{"control", "write", "fence", "mount", "collect"}

type commitTiming struct {
	wait, hold                zonelog.Duration
	attempts, commits, errors atomic.Int64
}

// CommitKindStats measures transactions passing through the store's writer
// admission, including no-ops and failures in Attempts. Commits counts successful
// changes to the zonelog sequence, not physical fsync calls. Bulk ingestion uses
// its own writer admission and is not included.
type CommitKindStats struct {
	Attempts, Commits, Errors int64
	Wait, Hold                zonelog.DurationSnapshot
}

func (c *commitTiming) observe(wait, hold time.Duration, wrote bool, err error) {
	c.wait.Observe(wait)
	c.hold.Observe(hold)
	c.attempts.Add(1)
	if wrote {
		c.commits.Add(1)
	}
	if err != nil {
		c.errors.Add(1)
	}
}

func (s *Store) commitKindStats() map[string]CommitKindStats {
	out := make(map[string]CommitKindStats, commitKindCount)
	for i, name := range commitKindNames {
		c := &s.commitTimings[i]
		out[name] = CommitKindStats{
			Attempts: c.attempts.Load(), Commits: c.commits.Load(), Errors: c.errors.Load(),
			Wait: c.wait.Snapshot(), Hold: c.hold.Snapshot(),
		}
	}
	return out
}
