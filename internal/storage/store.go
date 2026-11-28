package storage

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

var (
	ErrFrozen       = errors.New("durable owner fence is closed")
	ErrNotOwner     = errors.New("local disk does not hold writable authority")
	ErrSequence     = errors.New("noncontiguous or conflicting mutation sequence")
	ErrDataCapacity = errors.New("durable data capacity exceeded")
	// ErrTooManyMetrics: a record wider than MaxMetrics.
	ErrTooManyMetrics = errors.New("too many metrics")
	// ErrClosed: the store is closed.
	ErrClosed      = errors.New("storage: closed")
	ErrMissingData = errors.New("active owner has no durable topology; restore its data before restart")
	// ErrUnsupportedFormat: the disk holds control state in a shape this
	// build no longer reads (retiredShapes). It is refused, never misread.
	ErrUnsupportedFormat = errors.New("storage: control state written by an older build: start from an empty data directory and re-import")
)
var (
	liveBucket       = []byte("live")
	historyBucket    = []byte("history")
	operationsBucket = []byte("operations")
	stagingBucket    = []byte("staging")
	// transferBucket holds the frozen snapshots of each pending transfer, keyed
	// by operation then zone. Embedding them in the causal state blob made every
	// state read or write during a transfer proportional to the moving region.
	transferBucket = []byte("transfer-snapshots")
	// stagingIndexBucket names the sessions that have staged rows
	// (staging_index.go).
	stagingIndexBucket = []byte("staging-sessions")
	metaBucket         = []byte("meta")
	sequenceKey        = []byte("sequence")
	viewKey            = []byte("view")
	// data-usage-2 counts each row's last element and value only; the
	// former key also counted each bucket's name once, which an update could
	// only keep exact with a scan per changed node. A store with the old key
	// scans once and writes the new one.
	usageKey     = []byte("data-usage-2")
	identityKey  = []byte("identity")
	storageIDKey = []byte("storage-id")
)

// Store persists owner mutations and transfer fences in atomic zonelog
// transactions. Open uses fsynced commits; benchmark-only options may relax
// physical durability without changing transaction contents.
type Store struct {
	fingerprints        fingerprintCache
	controlCache        [2]atomic.Pointer[decodedControl]
	membershipCache     [4]atomic.Pointer[decodedMembership]
	membershipSnapshots [4]atomic.Pointer[membershipSnapshot]
	spatial             spatialCatalog
	planningTotalsCache atomic.Pointer[planningTotalsSnapshot]
	causalCache         atomic.Pointer[causalSnapshot]
	causalPrevious      atomic.Pointer[causalSnapshot]
	causalVerified      atomic.Pointer[verifiedCausal] // see committedCausal
	causalCacheMu       sync.Mutex
	contactHost         atomic.Pointer[string]
	path                string
	db                  *zonelog.Store
	// monitoredUsage is the last usage MonitoredUsage read.
	monitoredUsage atomic.Pointer[protocol.ResourceCounters]
	// writeMu serializes write transactions and bulk ingests; readers use
	// lock-free MVCC zonelog.Snapshots and never block writers or fsync.
	writeMu sync.Mutex
	// staged is the staging sessions that have rows at the last commit, in
	// order (staging_index.go). writeMu guards it.
	staged []string
	// copies is, for each request, how many staged copies of it the last
	// commit holds (staging_copies.go). writeMu guards it.
	copies         map[string]int32
	fenceAdmission zoneAdmission
	slowWriteAt    atomic.Int64
	// lastTransferStep is the unix ns of the last updateCausal commit that
	// wrote something (LastTransferStep).
	lastTransferStep      atomic.Int64
	localTransferRevision atomic.Uint64
	// mergeMu serializes membership preparation outside writeMu so
	// concurrent topology merges never spin in O(N^2) retries while live writes
	// keep committing uninterrupted.
	mergeMu                           sync.Mutex
	writeLockTiming, validationTiming zonelog.Duration
	commitTimings                     [commitKindCount]commitTiming
	writes                            *diskBatch
	locks                             lockWatch
	node                              string
	cluster                           string
	capacity                          int64
	// omitHistory keeps the live record and the retry receipt, and does not
	// append the mutation log. A later handoff cannot replay that backlog.
	omitHistory bool
	noSync      bool
	memoryBytes int
	// shadowDB isolates shadow copies from the live write/rows lock (SHADOW-1).
	// Opened lazily on first shadow call so non-replicated nodes pay zero cost.
	shadowMu sync.RWMutex
	shadowDB *zonelog.Store
	// ownedSeqs caches per-zone durable sequences owned by this disk so
	// OwnedSeqs avoids scanning all zones on every monitoring tick.
	ownedSeqsMu  sync.Mutex
	ownedSeqsVer causal.Digest
	ownedSeqs    map[tree.ZoneKey]uint64
	// collections caches the catalog (collections.go): name → *Collection,
	// nil for a collection read as legacy.
	collections sync.Map
	// collecting maps each active TransferID to its shared collection result.
	collecting sync.Map
}

// Options changes physical commit scheduling, never logical transaction
// contents. NoSync is only suitable for benchmarks where power-loss durability
// is deliberately excluded. BatchWrites keeps ACK-after-commit while allowing
// concurrent applies to share one zonelog transaction and one fsync.
type Options struct {
	// Entropy is an identity source, not protocol randomness. Nil uses
	// crypto/rand.Reader; deterministic fault tests supply a seeded reader.
	Entropy io.Reader
	// FS optionally supplies the zonelog device (fault/resource simulation).
	// Nil uses the ordinary filesystem; durability still follows NoSync.
	FS          zonelog.FS
	NoSync      bool
	BatchWrites bool
	BatchSize   int
	BatchDelay  time.Duration
	// Admission bounds queued plus in-flight writes independently of BatchSize.
	// Zero selects bounded defaults; bytes measures retained request weight,
	// not total process RSS or physical disk consumption.
	MaxPendingWrites int
	MaxPendingBytes  int64
	// OmitHistory skips the per-zone mutation log. Live values and retry
	// receipts stay. Catch-up of a peer that missed those writes cannot replay
	// them and must copy the current records instead.
	OmitHistory bool
	// MemoryBytes is the memory budget of the record store
	// (zonelog.Options.MemoryBytes); 0 keeps its default, 256 MB.
	MemoryBytes int
}

// RecordsDir is the directory of a store's rows.
func RecordsDir(path string) string { return path + ".records" }

func Open(path, node, cluster string, capacity int64) (*Store, error) {
	return OpenWithOptions(path, node, cluster, capacity, Options{
		BatchWrites: true,
		BatchSize:   256,
		BatchDelay:  2 * time.Millisecond,
	})
}

func OpenWithOptions(path, node, cluster string, capacity int64, options Options) (*Store, error) {
	if path == "" || node == "" || cluster == "" || capacity <= 0 || options.MaxPendingWrites < 0 || options.MaxPendingBytes < 0 {
		return nil, fmt.Errorf("durable data configuration incomplete")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// A store is its records directory alone. A file at path itself is a
	// store of an earlier engine (bbolt), which no release reads.
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s: not a zonelog store (a store is %s alone): start from an empty data directory", path, RecordsDir(path))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	db, err := zonelog.Open(RecordsDir(path), zonelog.Options{FS: options.FS, Entropy: options.Entropy, NoSync: options.NoSync, MemoryBytes: options.MemoryBytes})
	if err != nil {
		return nil, err
	}
	s := &Store{
		db:          db,
		path:        path,
		node:        node,
		cluster:     cluster,
		capacity:    capacity,
		omitHistory: options.OmitHistory,
		noSync:      options.NoSync,
		memoryBytes: options.MemoryBytes,
	}
	// Even ungrouped mode uses the same bounded admission path. A group of
	// one with no delay preserves its one-request-per-transaction semantics.
	size, delay := 1, time.Duration(0)
	if options.BatchWrites {
		size, delay = cmp.Or(options.BatchSize, 1000), max(options.BatchDelay, 0)
	}
	s.writes = newDiskBatchWithLimits(func(fn func(*Tx) error) error {
		_, err := s.updateWroteKind(commitWrite, fn)
		return err
	}, size, delay, options.MaxPendingWrites, options.MaxPendingBytes)
	s.startLockWatch()
	if len(s.db.Collections()) == 0 {
		s.spatial.state.Store(&spatialState{})
	}
	err = s.update(func(tx *Tx) error {
		identity := []byte(cluster + "\x00" + node)
		if tx.value(meta(identityKey)) == nil {
			if err := tx.put(meta(storageFormatKey), storageFormat); err != nil {
				return err
			}
		} else if !bytes.Equal(tx.value(meta(storageFormatKey)), storageFormat) {
			return fmt.Errorf("%w: explicit rebuild required", ErrUnsupportedFormat)
		}
		if err := validatePermissionControl(tx, node, cluster); err != nil {
			return err
		}

		if old := tx.value(meta(identityKey)); old != nil && !bytes.Equal(old, identity) {
			return ErrWrongCluster
		}
		if tx.value(meta(storageIDKey)) == nil {
			var id [16]byte
			entropy := options.Entropy
			if entropy == nil {
				entropy = rand.Reader
			}
			if _, err := io.ReadFull(entropy, id[:]); err != nil {
				return err
			}
			if err := tx.put(meta(storageIDKey), []byte(hex.EncodeToString(id[:]))); err != nil {
				return err
			}
		}
		if err := tx.put(meta(identityKey), identity); err != nil {
			return err
		}

		if err := backfillStagingIndex(tx); err != nil {
			return err
		}

		// Usage is committed atomically with every mutation and transfer.
		// A restart must not walk every bucket and fault the whole file into
		// RAM merely to recompute the same durable counters.
		used, err := s.storedDataUsage(tx)
		if err != nil {
			return err
		}
		return s.persistCapacity(tx, used)
	})
	if err == nil {
		err = s.loadStaged()
	}
	if err == nil {
		err = s.loadCopies()
	}
	if err == nil {
		err = s.reconcileBulk()
	}
	if err == nil {
		err = s.loadCollections()
	}
	if err != nil {
		s.db.Close()
		return nil, err
	}
	return s, nil
}

// view runs a read transaction on an isolated MVCC snapshot of one commit:
// concurrent updates and fsyncs never block it, and it never blocks updates.
func (s *Store) view(fn func(*Tx) error) error {
	snap := s.db.Snapshot()
	defer snap.Close()
	return s.transaction(snap, false).run(fn)
}

// read runs fn in a read transaction and returns what it returns.
func read[T any](s *Store, fn func(*Tx) (T, error)) (T, error) {
	var out T
	err := s.view(func(tx *Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

// peek reads without waiting for an update in flight: each read sees the
// last commit, not one snapshot. Only for checks repeated under the lock.
func (s *Store) peek(fn func(*Tx) error) error {
	return s.transaction(s.db, false).run(fn)
}

// update runs a write transaction and commits what it wrote, control rows
// and record rows, as one atomic zonelog batch.
func (s *Store) update(fn func(*Tx) error) error {
	_, err := s.updateWrote(fn)
	return err
}

// updateWrote is update that also reports whether its commit wrote anything:
// zonelog gives no seq to a batch that changes nothing, and writeMu keeps
// every other update out between the two reads of the seq.
func (s *Store) updateWrote(fn func(*Tx) error) (bool, error) {
	return s.updateWroteKind(commitControl, fn)
}

func (s *Store) updateWroteKind(kind commitKind, fn func(*Tx) error) (wrote bool, err error) {
	started := time.Now()
	s.locks.write.wait()
	s.writeMu.Lock()
	s.locks.write.took()
	waited := time.Since(started)
	s.writeLockTiming.Observe(waited)
	acquired := time.Now()
	defer func() {
		held := time.Since(acquired)
		s.locks.write.released()
		s.writeMu.Unlock() // diagnostics must never extend the writer critical section
		s.commitTimings[kind].observe(waited, held, wrote, err)
		if waited < time.Second && held < time.Second {
			return
		}
		now := time.Now().UnixNano()
		previous := s.slowWriteAt.Load()
		if now-previous >= int64(5*time.Second) && s.slowWriteAt.CompareAndSwap(previous, now) {
			slog.Warn("storage writer delayed", "node", s.node, "kind", commitKindNames[kind], "wait_ms", waited.Milliseconds(), "held_ms", held.Milliseconds())
		}
	}()
	before := s.db.Seq()
	err = s.updateLocked(fn)
	return err == nil && s.db.Seq() != before, err
}

// updateCausal is update with the causal state it reads first.
func (s *Store) updateCausal(fn func(*Tx, CausalState) error) error {
	return s.updateCausalKind(commitControl, fn)
}

func (s *Store) updateCausalKind(kind commitKind, fn func(*Tx, CausalState) error) error {
	wrote, err := s.updateWroteKind(kind, func(tx *Tx) error {
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		return fn(tx, state)
	})
	if wrote {
		s.lastTransferStep.Store(time.Now().UnixNano())
		s.localTransferRevision.Add(1)
	}
	return err
}

// LastTransferStep is when this process last committed a step of a transfer
// of this disk (begin, freeze, stage, emit, install, ack, complete, abort,
// collect, a move or a cold drain): an updateCausal commit that wrote
// something. A call that changes nothing, a knowledge merge and a fallback
// write are not steps. Zero until the first step: after a restart, an open
// transfer's quiet time starts again.
// LocalTransferRevision is a process-local repair hint. It advances only after
// this disk commits a transfer step, not when unrelated remote facts merge.
// It never grants permission; a reopened summary starts with a full fold.
func (s *Store) LocalTransferRevision() uint64 { return s.localTransferRevision.Load() }

func (s *Store) LastTransferStep() time.Time {
	if at := s.lastTransferStep.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}

// updateLocked is update for a caller holding writeMu (a bulk ingest).
func (s *Store) updateLocked(fn func(*Tx) error) error {
	tx := s.transaction(s.db, true)
	started := time.Now()
	err := tx.run(fn)
	if err == nil {
		err = s.accountUsage(tx)
	}
	s.validationTiming.Observe(time.Since(started))
	return s.commitSpatial(tx, err)
}

func (s *Store) Identity() (string, error) {
	var id string
	err := s.peek(func(tx *Tx) error {
		id = storageID(tx)
		return nil
	})
	return id, err
}

// storageID is the random identity of this database file. Authority names it,
// so a node restored onto another disk cannot claim the old disk's grants.
func storageID(tx *Tx) string {
	return string(tx.value(meta(storageIDKey)))
}

func (s *Store) Close() error {
	// The collection a completed handoff started still writes: closing under
	// it raced with its last batch (found by -race, 2026-11-21).
	if s.writes != nil {
		s.writes.close()
	}
	s.shadowMu.Lock()
	var shadowErr error
	if s.shadowDB != nil {
		shadowErr = s.shadowDB.Close()
		s.shadowDB = nil
	}
	s.shadowMu.Unlock()
	return errors.Join(s.db.Close(), shadowErr)
}
