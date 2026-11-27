package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The records golden file is shared with sdk (src/api/testdata/records/):
// basic.bin is EncodeRecords of goldenRecords, basic.json the same items as
// /records answers them in JSON, plus what an IXRB decoder must read back.
// INDEXUS_WRITE_GOLDEN=1 rewrites both files; otherwise they must match.

func goldenRecords() []Item {
	raw := make([]byte, 300)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	b64, _ := json.Marshal(base64.StdEncoding.EncodeToString(raw))
	return []Item{
		{Collection: "golden", Location: "7Bxy", Id: "tree-1", Metrics: []float64{43.5, -1.25, 12}, Payload: b64, Binary: true, Weight: 3},
		{Collection: "golden", Location: "7Bxz", Id: "bâtiment-é", Metrics: []float64{1}, Payload: json.RawMessage(`{"format":"indexus-building-v3","levels":[1,2],"name":"tour"}`)},
		{Collection: "golden", Location: "7C", Id: "empty", Metrics: nil},
		{Collection: "golden", Location: "_-", Id: "bytes-0", Payload: json.RawMessage(`""`), Binary: true},
		{Collection: "golden", Location: "a", Id: "text", Metrics: []float64{0.1, 1e300}, Payload: json.RawMessage(`"plain text"`)},
	}
}

type goldenRecord struct {
	Location   string    `json:"location"`
	Id         string    `json:"id"`
	Binary     bool      `json:"binary"`
	Weight     float64   `json:"weight"`
	Metrics    []float64 `json:"metrics"`
	PayloadHex string    `json:"payloadHex"`
}

// goldenWideRecords: a record wider than 255 metrics (an embedding) between
// two narrow ones, which IXRB writes as version 3.
func goldenWideRecords() []Item {
	wide := make([]float64, 1536)
	for i := range wide {
		wide[i] = float64(i%17) - 8.5
	}
	return []Item{
		{Collection: "golden", Location: "7Bxy", Id: "narrow", Metrics: []float64{1, 2}},
		{Collection: "golden", Location: "7Bxz", Id: "embedding", Metrics: wide, Payload: json.RawMessage(`{"text":"a passage"}`)},
		{Collection: "golden", Location: "7C", Id: "empty"},
	}
}

func TestRecordsGoldenFile(t *testing.T) {
	decoded, body := checkRecordsGolden(t, "basic", goldenRecords())
	items := goldenRecords()
	// A binary record comes back as its bytes, a JSON one as its text.
	if want, _ := base64.StdEncoding.DecodeString("AAcOFRwjKjE4P0ZNVFtiaW"); !bytes.HasPrefix(decoded[0].Payload, want[:12]) {
		t.Fatalf("binary payload: %x", decoded[0].Payload[:12])
	}
	if string(decoded[1].Payload) != string(items[1].Payload) || decoded[2].Payload != nil {
		t.Fatalf("payloads: %q %q", decoded[1].Payload, decoded[2].Payload)
	}
	if body[4] != recordsVersion {
		t.Fatalf("narrow records are not version %d", recordsVersion)
	}
}

func TestRecordsGoldenFileWide(t *testing.T) {
	decoded, body := checkRecordsGolden(t, "wide", goldenWideRecords())
	if body[4] != recordsWide || len(decoded[1].Metrics) != 1536 || decoded[1].Metrics[17] != -8.5 {
		t.Fatalf("wide records: version %d, %d metrics", body[4], len(decoded[1].Metrics))
	}
}

func checkRecordsGolden(t *testing.T, name string, items []Item) ([]Item, []byte) {
	t.Helper()
	var body bytes.Buffer
	if err := EncodeRecords(&body, items); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecords("golden", body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	expect := make([]goldenRecord, len(decoded))
	for i, it := range decoded {
		metrics := it.Metrics
		if metrics == nil {
			metrics = []float64{}
		}
		expect[i] = goldenRecord{it.Location, it.Id, it.Binary, it.Weight, metrics, hex.EncodeToString(it.Payload)}
	}
	answer, err := json.Marshal(map[string]any{"records": items})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := json.MarshalIndent(map[string]any{
		"collection": "golden",
		"json":       json.RawMessage(answer),
		"decoded":    expect,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	doc = append(doc, '\n')
	dir := filepath.Join("testdata", "records")
	if os.Getenv("INDEXUS_WRITE_GOLDEN") == "1" {
		if err := os.WriteFile(filepath.Join(dir, name+".bin"), body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), doc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for file, want := range map[string][]byte{name + ".bin": body.Bytes(), name + ".json": doc} {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("testdata/records/%s is not what the encoder writes; rerun with INDEXUS_WRITE_GOLDEN=1", file)
		}
	}
	return decoded, body.Bytes()
}
