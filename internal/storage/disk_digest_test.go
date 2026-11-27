package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The digests below pin the exact bytes written to bbolt (buckets, keys,
// values, in order), so a refactor of the write path that changes the disk
// format fails here. Since the rows moved to zonelog they pin the control
// half: sequences, receipts, history, staging, counters. They changed once
// more when bbolt left: new stores no longer write meta/records-engine; and
// with linear knowledge (E5b): the control document keeps the digest of its
// knowledge, stored under meta/causal-v1-knowledge, and an installed
// transfer drops its per-zone states; and with the control version
// (REM-87): meta/causal-v1-version holds sha256 of the control document;
// and when usage stopped counting bucket names: meta/data-usage-2.
// Local receiver tickets deliberately bump the sole disk format to indexus-flat-4;
// the fixture below otherwise writes the same control/data mutation sequence.
// The target control field is now explicitly MembershipDigest (not Knowledge).
// Scoped inventory cursors add durable per-collection data
// cut sequences to live mutations; staging alone leaves these stamps unchanged.
const (
	diskDigestFull        = "2ae93e3ca70625636bfed06a9d214839a1bd0b4ee36da9b29bc5ea3777f8b491"
	diskDigestOmitHistory = "e4cac71487649b89fcbd617a42362ddfea4365dfde9871ed63f3811905cbf032"
	diskDigestStaged      = "0230a6704ab390ee562368d4d11c4cb0c095b237afcf0eba2f4c86e8d4ddead4"
)

func TestDiskDigestOfFixedMutationSequence(t *testing.T) {
	dir := t.TempDir()
	full := openTestData(t, filepath.Join(dir, "full.db"), "D")
	defer full.Close()
	fixDigestIdentity(t, full)
	initTestCausal(t, full)
	writeDigestSequence(t, full)

	quiet, err := OpenWithOptions(filepath.Join(dir, "quiet.db"), "D", "test", 1<<24, Options{OmitHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	fixDigestIdentity(t, quiet)
	initTestCausal(t, quiet)
	writeDigestSequence(t, quiet)

	staged := openTestData(t, filepath.Join(dir, "staged.db"), "E")
	defer staged.Close()
	fixDigestIdentity(t, staged)
	initTestCausal(t, staged)
	stageDigestSequence(t, full, staged)

	for _, c := range []struct {
		name  string
		store *Store
		want  string
	}{
		{"full", full, diskDigestFull},
		{"omit-history", quiet, diskDigestOmitHistory},
		{"staged", staged, diskDigestStaged},
	} {
		if got := diskDigest(t, c.store); got != c.want {
			t.Errorf("%s disk digest = %s, want %s", c.name, got, c.want)
		}
		sessions := assertStagingIndex(t, c.store)
		if staged := c.store == staged; staged != slices.Equal(sessions, []string{"t1"}) || !staged && len(sessions) != 0 {
			t.Errorf("%s: indexed staging sessions %q", c.name, sessions)
		}
	}
}

// fixDigestIdentity replaces the random storage identity, which also enters the
// causal genesis, so the whole file is reproducible.
func fixDigestIdentity(t *testing.T, s *Store) {
	t.Helper()
	err := s.update(func(tx *Tx) error {
		return tx.put(meta(storageIDKey), []byte("00112233445566778899aabbccddeeff"))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeDigestSequence(t *testing.T, s *Store) {
	t.Helper()
	write := func(request string, r Record) {
		t.Helper()
		if _, _, _, err := s.ApplyRecord(request, r); err != nil {
			t.Fatalf("%s: %v", request, err)
		}
	}
	payload := json.RawMessage(`{"name":"first","tags":["a","b"]}`)
	write("c1", Record{Collection: "Demo000000000000", Location: "aa", ID: "r1", Metrics: []float64{1.5, -2}, Payload: payload})
	write("c2", Record{Collection: "Demo000000000000", Location: "aa", ID: "r2", Metrics: []float64{3}})
	write("c3", Record{Collection: "Demo000000000000", Location: "ab", ID: "r3"})
	write("c4", Record{Collection: "Other00000000001", Location: "aa", ID: "r1", Metrics: []float64{0.25}})
	// Update, then delete, then recreate under the same ID.
	write("u1", Record{Collection: "Demo000000000000", Location: "aa", ID: "r1", Metrics: []float64{4, 5}})
	write("d1", Record{Collection: "Demo000000000000", Location: "aa", ID: "r2", Deleted: true})
	write("u2", Record{Collection: "Demo000000000000", Location: "aa", ID: "r2", Metrics: []float64{6}})
	// An identical retry is replayed without a new mutation.
	m, _, applied, err := s.ApplyRecord("c3", Record{Collection: "Demo000000000000", Location: "ab", ID: "r3"})
	if err != nil || applied || m.Seq != 1 {
		t.Fatalf("replay = %+v applied=%v err=%v", m, applied, err)
	}
	// A conflicting retry is refused and leaves the disk untouched.
	if _, _, _, err = s.ApplyRecord("c3", Record{Collection: "Demo000000000000", Location: "ab", ID: "r3", Metrics: []float64{9}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay err = %v", err)
	}
	// A cold pin makes every applied write also bump the cold activity counter.
	err = s.update(func(tx *Tx) error { return tx.put(meta(coldPinKey), []byte(`{}`)) })
	if err != nil {
		t.Fatal(err)
	}
	write("p1", Record{Collection: "Demo000000000000", Location: "ab", ID: "r4", Metrics: []float64{7}})
	err = s.update(func(tx *Tx) error { return tx.del(meta(coldPinKey)) })
	if err != nil {
		t.Fatal(err)
	}
}

// stageDigestSequence copies one zone of source into target through the
// prepare, mirror and certify path a handoff uses.
func stageDigestSequence(t *testing.T, source, target *Store) {
	t.Helper()
	zone, err := recordZone(Record{Collection: "Demo000000000000", Location: "aa", ID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := source.Snapshot(zone)
	if err != nil {
		t.Fatal(err)
	}
	base := snap
	base.Seq = 2
	base.Records = []Record{{Collection: "Demo000000000000", Location: "aa", ID: "r1", Metrics: []float64{1.5, -2}, Payload: json.RawMessage(`{"name":"first","tags":["a","b"]}`)}, {Collection: "Demo000000000000", Location: "aa", ID: "r2", Metrics: []float64{3}}}
	base.Requests = nil
	for _, r := range snap.Requests {
		if r.Seq <= 2 {
			base.Requests = append(base.Requests, r)
		}
	}
	if _, err = target.Prepare("t1", "D", []Snapshot{base}); err != nil {
		t.Fatal(err)
	}
	backlog, err := source.Mutations(zone, 2, snap.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if err = target.Mirror("t1", zone, backlog); err != nil {
		t.Fatal(err)
	}
	// A retried mirror round is accepted without a second write.
	if err = target.Mirror("t1", zone, backlog); err != nil {
		t.Fatal(err)
	}
	seal, err := snap.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Certify("t1", "D", seal); err != nil {
		t.Fatal(err)
	}
}

// diskDigest hashes the control state as bbolt's buckets, keys and values,
// in order: the buckets are rebuilt from the paths of the control rows.
func diskDigest(t *testing.T, s *Store) string {
	t.Helper()
	root := &digestNode{}
	for _, name := range [][]byte{liveBucket, historyBucket, operationsBucket, stagingBucket, metaBucket, transferBucket} {
		root.child(name).bucket = true
	}
	err := s.view(func(tx *Tx) error {
		return tx.rows("", "", func(path string, payload []byte) bool {
			// The staging index (2026-11-26) is a new bucket beside the ones
			// pinned here, which it leaves byte for byte as they were: the
			// digests keep pinning those, and the test checks the index
			// against the staged rows (assertStagingIndex).
			if strings.HasPrefix(path, key(stagingIndexBucket)) {
				return true
			}
			n, elems := root, split(path)
			for _, e := range elems[:len(elems)-1] {
				n = n.child(e)
				n.bucket = true
			}
			n = n.child(elems[len(elems)-1])
			n.value = payload[1:]
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	root.digest(h, true)
	return hex.EncodeToString(h.Sum(nil))
}

type digestNode struct {
	bucket   bool
	value    []byte
	children map[string]*digestNode
}

func (n *digestNode) child(name []byte) *digestNode {
	if n.children == nil {
		n.children = map[string]*digestNode{}
	}
	c := n.children[string(name)]
	if c == nil {
		c = &digestNode{}
		n.children[string(name)] = c
	}
	return c
}

func (n *digestNode) digest(h hash.Hash, root bool) {
	for _, name := range slices.Sorted(maps.Keys(n.children)) {
		c := n.children[name]
		if c.bucket {
			digestField(h, 'B', []byte(name))
			c.digest(h, false)
		} else {
			digestField(h, 'K', []byte(name))
			digestField(h, 'V', c.value)
		}
	}
	if !root {
		digestField(h, 'E', nil)
	}
}

func digestField(h hash.Hash, kind byte, raw []byte) {
	var size [9]byte
	size[0] = kind
	binary.BigEndian.PutUint64(size[1:], uint64(len(raw)))
	h.Write(size[:])
	h.Write(raw)
}

// Prepare is a fixture for pinning the staging format, not a protocol entry point.
func (s *Store) Prepare(transaction, source string, snapshots []Snapshot) (map[string]uint64, error) {
	progress := map[string]uint64{}
	err := s.updateCausal(func(tx *Tx, state CausalState) error {
		if receiverClosed(tx, state) {
			return ErrReceiverClosed
		}
		if err := s.prepareSnapshots(tx, transaction, source, snapshots, progress); err != nil {
			return err
		}
		return s.checkCapacity(tx) // as PreloadCausal does after its last write
	})
	if err != nil {
		return nil, err
	}
	return progress, nil
}
