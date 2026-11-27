package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// Deletion and its exact usage accounting need keys and lengths, not old values.
type keysOnlyReader struct {
	zonelog.Reader
	prefix string
	values map[string]bool
}

func (r keysOnlyReader) Get(c, z, id string) (zonelog.Record, bool, error) {
	if c == kvCollection && r.values[id] {
		return zonelog.Record{}, false, errors.New("cleanup loaded a discarded value")
	}
	return r.Reader.Get(c, z, id)
}

func (r keysOnlyReader) ScanWith(c, z string, after *zonelog.Record, ids *zonelog.IDRange, load func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	var failure error
	err := r.Reader.ScanWith(c, z, after, ids, func(v zonelog.Record) bool {
		if c == kvCollection && strings.HasPrefix(v.ID, r.prefix) && load(v) {
			failure = errors.New("cleanup decoded a discarded value")
			return false
		}
		return load(v)
	}, visit)
	return errors.Join(err, failure)
}

func TestDropUsesOnlyKeysAndSizesWithPendingWrites(t *testing.T) {
	s, _ := routingStore(t)
	prefix := key(historyBucket, []byte("discarded"))
	untouched := key(historyBucket, []byte("keep"), []byte("row"))
	first, second := prefix+element([]byte("first")), prefix+element([]byte("second"))
	if err := s.update(func(tx *Tx) error {
		for _, p := range []string{first, second, untouched} {
			if e := tx.put(p, []byte(strings.Repeat("payload", 8192))); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(tx *Tx) error {
		tx.reader.Reader = keysOnlyReader{tx.reader.Reader, prefix, map[string]bool{first: true, second: true}}
		// Own replacements and new rows must be deleted too, without confusing
		// their sizes with the committed sizes used by capacity accounting.
		if e := tx.put(first, []byte("replacement")); e != nil {
			return e
		}
		if e := tx.put(prefix+element([]byte("new")), []byte("new")); e != nil {
			return e
		}
		return tx.drop(prefix)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.view(func(tx *Tx) error {
		if tx.any(prefix) {
			t.Fatal("discarded rows remain")
		}
		if tx.value(untouched) == nil {
			t.Fatal("unrelated row deleted")
		}
		used, _, e := s.usageAfter(tx)
		if e != nil {
			return e
		}
		scanned, e := s.dataUsage(tx)
		if e == nil && used != scanned {
			t.Fatalf("usage %+v, scan %+v", used, scanned)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

type noRecordPayloadReader struct{ zonelog.Reader }

func (r noRecordPayloadReader) ScanWith(c, z string, after *zonelog.Record, ids *zonelog.IDRange, load func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	var failure error
	err := r.Reader.ScanWith(c, z, after, ids, func(v zonelog.Record) bool {
		if c != kvCollection && load(v) {
			failure = errors.New("zone cleanup decoded a discarded opaque row")
			return false
		}
		return load(v)
	}, visit)
	return errors.Join(err, failure)
}

func TestClearZoneDoesNotDecodeOpaquePayload(t *testing.T) {
	s, _ := routingStore(t)
	record := Record{Collection: "Demo000000000000", Location: "A", ID: "deleted", Deleted: true, Payload: bigPayload(64 << 10)}
	if _, err := s.Write("tombstone", record); err != nil {
		t.Fatal(err)
	}
	z, err := recordZone(record)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok, err := s.db.Get(record.Collection, record.Location, record.ID)
	if err != nil || !ok || !raw.Opaque {
		t.Fatalf("fixture did not create an opaque row: %+v %v", raw, err)
	}
	if err := s.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(tx *Tx) error {
		tx.reader.Reader = noRecordPayloadReader{tx.reader.Reader}
		return tx.clearZone(z)
	}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := s.db.Get(record.Collection, record.Location, record.ID); err != nil || exists {
		t.Fatalf("row remained after collection: %t %v", exists, err)
	}
}
