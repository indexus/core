package storage

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/prep"
)

// TestPayloadDiskAndWire measures, for each NDJSON file of PAYLOADBENCH (a
// directory; payloads as base64 strings of binary payloads), what a binary
// batch takes on disk in zonelog against its payload bytes, and what its
// records take on the wire: JSON (base64) and IXRB, plain and gzipped (the
// fastest level, as /records sends them). One JSON line per file.
func TestPayloadDiskAndWire(t *testing.T) {
	dir := os.Getenv("PAYLOADBENCH")
	if dir == "" {
		t.Skip("PAYLOADBENCH unset")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.bin.ndjson"))
	for _, file := range files {
		type rec struct {
			Location, ID string
			Metrics      []float64
			Payload      []byte // base64 in the file
			head         prep.Head
		}
		var rows []rec
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var r rec
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if r.head, err = prep.MakeHead(r.Location); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, r)
		}
		f.Close()
		sort.Slice(rows, func(i, j int) bool {
			if c := bytes.Compare(rows[i].head[:], rows[j].head[:]); c != 0 {
				return c < 0
			}
			return rows[i].ID < rows[j].ID
		})
		metrics := 0
		for _, r := range rows {
			metrics = max(metrics, len(r.Metrics))
		}
		batch := t.TempDir()
		w, err := prep.NewBatch(batch, prep.Options{Collection: bulkCollection, Metrics: metrics, Payloads: true, PayloadEncoding: "binary", SplitDepth: 1, ZoneDepths: [2]int{0, -1}, Inputs: prep.Inputs{Tool: "payloadbench"}})
		if err != nil {
			t.Fatal(err)
		}
		raw := 0
		var last prep.Head
		var lastID string
		for i, r := range rows {
			if i > 0 && last == r.head && lastID == r.ID {
				continue
			}
			last, lastID = r.head, r.ID
			m := make([]float64, metrics)
			copy(m, r.Metrics)
			if err := w.Add(prep.Row{Head: r.head, ID: []byte(r.ID), Weight: 1, Metrics: m, Payload: r.Payload}); err != nil {
				t.Fatal(err)
			}
			raw += len(r.Payload)
		}
		if _, err := w.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "node.db")
		s, _ := bulkStore(t, path)
		if _, err := ingestDir(s, batch, false); err != nil {
			t.Fatal(err)
		}
		snaps, err := s.Inventory(false)
		if err != nil {
			t.Fatal(err)
		}
		var records []Record
		for _, snap := range snaps {
			records = append(records, snap.Records...)
		}
		s.Close()
		var val, all int64
		filepath.Walk(RecordsDir(path), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				all += info.Size()
				if strings.HasSuffix(p, ".val") {
					val += info.Size()
				}
			}
			return nil
		})
		// The wire: JSON as /records answers it, IXRB as EncodeRecords writes it.
		type item struct {
			Collection string          `json:"collection"`
			Location   string          `json:"location"`
			Id         string          `json:"id"`
			Metrics    []float64       `json:"metrics"`
			Payload    json.RawMessage `json:"payload,omitempty"`
			Binary     bool            `json:"binary,omitempty"`
		}
		items := make([]item, len(records))
		ixrb := append([]byte("IXRB"), 2, 0, 0, 0)
		ixrb = binary.LittleEndian.AppendUint32(ixrb, uint32(len(records)))
		for i, r := range records {
			items[i] = item{r.Collection, r.Location, r.ID, r.Metrics, r.Payload, r.Binary}
			var bytes []byte
			json.Unmarshal(r.Payload, &bytes)
			ixrb = binary.LittleEndian.AppendUint16(ixrb, uint16(len(r.Location)))
			ixrb = append(ixrb, r.Location...)
			ixrb = binary.LittleEndian.AppendUint16(ixrb, uint16(len(r.ID)))
			ixrb = append(append(ixrb, r.ID...), 1)
			ixrb = append(ixrb, make([]byte, 8)...)
			ixrb = append(ixrb, byte(len(r.Metrics)))
			ixrb = append(ixrb, make([]byte, 8*len(r.Metrics))...)
			ixrb = binary.LittleEndian.AppendUint32(ixrb, uint32(len(bytes)))
			for len(ixrb)%4 != 0 {
				ixrb = append(ixrb, 0)
			}
			ixrb = append(ixrb, bytes...)
		}
		js, _ := json.Marshal(map[string]any{"records": items})
		gz := func(b []byte) int {
			var out bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed)
			zw.Write(b)
			zw.Close()
			return out.Len()
		}
		fmt.Printf("{\"file\":%q,\"records\":%d,\"payload_bytes\":%d,\"val_bytes\":%d,\"records_dir\":%d,\"val_over_payload\":%.3f,\"dir_over_payload\":%.3f,\"json\":%d,\"json_gzip\":%d,\"ixrb\":%d,\"ixrb_gzip\":%d}\n",
			filepath.Base(file), len(records), raw, val, all, float64(val)/float64(raw), float64(all)/float64(raw), len(js), gz(js), len(ixrb), gz(ixrb))
	}
}
