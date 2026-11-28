package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"math"
	"sync"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// The v1 leaf preimage starts with "indexus/subtree-row/v1" plus NUL, then
// collection, location, document ID, normalized float64 weight, metric count,
// metric float64 values, one binary-encoding byte, and the stored JSON payload
// (a JSON base64 string for binary documents). Strings and payloads have an
// unsigned 64-bit byte length prefix. Integers and IEEE-754 bits are big-endian.
// These are physical local live rows and can include ceded copies.
//
// Sum adds SHA-256 leaves as big-endian integers modulo 2^256. Count is the
// number of visible local documents. Only disjoint contributions can be added.
// Fingerprint binds this sum to its store incarnation, authority and scope.
// Guard also detects ABA across an intervening write. Empty strings mean unknown.
type SubtreeFingerprint struct {
	Fingerprint, Guard string
	Sum                [32]byte
	Count              uint64
}

type fingerprintEntry struct {
	seq    uint64
	result SubtreeFingerprint
}
type fingerprintCache struct {
	mu          sync.Mutex
	incarnation [32]byte
	entries     map[tree.ZoneKey]fingerprintEntry
}

func hashField(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])
	h.Write(value)
}

func hashNumber(h hash.Hash, v uint64) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], v)
	h.Write(raw[:])
}

func (f *SubtreeFingerprint) addRow(row Record) {
	h := sha256.New()
	h.Write([]byte("indexus/subtree-row/v1\x00"))
	for _, v := range []string{row.Collection, row.Location, row.ID} {
		hashField(h, []byte(v))
	}
	hashNumber(h, math.Float64bits(WeightOf(row.Weight)))
	hashNumber(h, uint64(len(row.Metrics)))
	for _, v := range row.Metrics {
		hashNumber(h, math.Float64bits(v))
	}
	if row.Binary {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	hashField(h, row.Payload)
	sum := h.Sum(nil)
	var carry uint16
	for i := len(sum) - 1; i >= 0; i-- {
		carry += uint16(f.Sum[i]) + uint16(sum[i])
		f.Sum[i] = byte(carry)
		carry >>= 8
	}
	f.Count++
}

func (c *fingerprintCache) lookup(zone tree.ZoneKey, seq uint64) (fingerprintEntry, [32]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		rand.Read(c.incarnation[:])
		c.entries = make(map[tree.ZoneKey]fingerprintEntry)
	}
	e, ok := c.entries[zone]
	return e, c.incarnation, ok && e.seq == seq
}

func (c *fingerprintCache) remember(zone tree.ZoneKey, e fingerprintEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.entries[zone]; ok && prev.seq > e.seq {
		return
	}
	if len(c.entries) >= 256 {
		clear(c.entries)
	}
	c.entries[zone] = e
}

// LocalSubtreeFingerprint can decline when the bounded scan is inconclusive
// or ownership is moving. A cached result is usable only at the exact committed
// disk sequence of its original local snapshot. No client state is retained.
func (s *Store) LocalSubtreeFingerprint(ctx context.Context, collection, location string) (SubtreeFingerprint, error) {
	var result SubtreeFingerprint
	if err := ctx.Err(); err != nil {
		return result, err
	}
	zone, err := tree.NewZoneKey(collection, location)
	if err != nil {
		return result, err
	}
	if zone.Location != location || zone.Collection != collection {
		return result, errors.New("noncanonical fingerprint scope")
	}
	entry, nonce, ok := s.fingerprints.lookup(zone, s.db.Seq())
	if ok {
		return entry.result, ctx.Err()
	}
	sn, ok := s.db.ContentSnapshot()
	if !ok {
		return result, ctx.Err()
	}
	defer sn.Close()
	err = s.transaction(sn, false).run(func(tx *Tx) error {
		c, e := s.causalSnapshot(tx)
		if e != nil {
			return e
		}
		if c.state.Moving != nil || pendingCausal(c.state, tx) || coldBusy(tx) {
			return zonelog.ErrContentScanLimit
		}
		e = sn.ScanContent(ctx, collection, location, 32768, 8<<20, func(z zonelog.Record) error {
			if z.Binary && !z.Opaque && len(z.Payload) == 0 {
				z.Payload = []byte{}
			}
			row, e := fromLog(z)
			if e != nil {
				return e
			}
			if !row.Deleted {
				result.addRow(row)
			}
			return nil
		})
		if e != nil {
			return e
		}
		h := sha256.New()
		h.Write([]byte("indexus/subtree-local/v1\x00"))
		h.Write(nonce[:])
		hashField(h, []byte(collection))
		hashField(h, []byte(location))
		hashField(h, c.version)
		h.Write(result.Sum[:])
		hashNumber(h, result.Count)
		result.Fingerprint = hex.EncodeToString(h.Sum(nil))
		h.Reset()
		h.Write(nonce[:])
		hashNumber(h, sn.Seq())
		result.Guard = hex.EncodeToString(h.Sum(nil))
		return ctx.Err()
	})
	if errors.Is(err, zonelog.ErrContentScanLimit) {
		return SubtreeFingerprint{}, nil
	}
	if err != nil {
		return SubtreeFingerprint{}, err
	}
	s.fingerprints.remember(zone, fingerprintEntry{sn.Seq(), result})
	return result, nil
}
