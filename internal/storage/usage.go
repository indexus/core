package storage

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/indexus/go-indexus-core/internal/protocol"
)

func (s *Store) dataUsage(tx *Tx) (protocol.ResourceCounters, error) {
	usageScans.Add(1)
	var out protocol.ResourceCounters
	out.Owned += tx.rowBytes()
	for _, name := range [][]byte{liveBucket, historyBucket, operationsBucket, stagingBucket, transferBucket} {
		used, err := tx.size(key(name))
		if err != nil {
			return out, err
		}
		if bytes.Equal(name, stagingBucket) || bytes.Equal(name, transferBucket) {
			out.Staging += used
		} else {
			out.Owned += used
		}
	}
	// Activation copies incoming operation receipts into the node-wide replay
	// ledger. Reserve those bytes before readiness, so an acknowledged receiver
	// cannot discover a capacity shortfall after the topology has committed.
	reserved := map[string][]byte{}
	staging := key(stagingBucket)
	var failed error
	err := tx.each(staging, "", func(path string, v []byte) bool {
		elems := split(path[len(staging):]) // session, zone, key
		if len(elems) != 3 || len(elems[2]) == 0 || elems[2][0] != receiptPrefix {
			return true
		}
		k := elems[2][1:]
		prior := tx.value(key(operationsBucket, k))
		if prior == nil {
			prior = reserved[string(k)]
		}
		if prior == nil {
			reserved[string(k)] = v
			out.ReservedIncoming += int64(len(k)) + int64(len(v))
		} else if !bytes.Equal(prior, v) {
			failed = ErrConflict
		}
		return failed == nil
	})
	return out, errors.Join(err, failed)
}

func (s *Store) storedDataUsage(tx *Tx) (protocol.ResourceCounters, error) {
	var used protocol.ResourceCounters
	if raw := tx.value(meta(usageKey)); raw != nil {
		if err := json.Unmarshal(raw, &used); err != nil {
			return used, err
		}
		return used, nil
	}
	return s.dataUsage(tx)
}

func (s *Store) CapacityUsage() (int64, protocol.ResourceCounters, error) {
	used, err := read(s, s.storedDataUsage)
	return s.capacity, used, err
}

// MonitoredUsage is CapacityUsage for monitoring: reads from an MVCC snapshot
// without waiting for any update or fsync in flight.
func (s *Store) MonitoredUsage() (int64, protocol.ResourceCounters) {
	if used, err := read(s, s.storedDataUsage); err == nil {
		s.monitoredUsage.Store(&used)
		return s.capacity, used
	}
	if used := s.monitoredUsage.Load(); used != nil {
		return s.capacity, *used
	}
	return s.capacity, protocol.ResourceCounters{}
}

func (s *Store) persistCapacity(tx *Tx, used protocol.ResourceCounters) error {
	remaining := s.capacity
	for _, part := range []int64{used.Owned, used.Residual, used.Staging, used.ReservedIncoming} {
		if part < 0 || part > remaining {
			return ErrDataCapacity
		}
		remaining -= part
	}
	return tx.putJSON(meta(usageKey), used)
}
