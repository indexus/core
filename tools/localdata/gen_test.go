package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The records of scripts/research/bulkbench.sh, the shape of the zonelog benchmark
// (internal/zonelog/compare): vegetation is one record per depth-5 zone, 13
// metrics and 200 bytes of payload; large has 20 to 60 KB of payload.
const benchAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

func benchMix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ x>>30) * 0xBF58476D1CE4E5B9
	x = (x ^ x>>27) * 0x94D049BB133111EB
	return x ^ x>>31
}

func benchChars(seed uint64, n int) string {
	b := make([]byte, n)
	for i := range b {
		if i%10 == 0 {
			seed = benchMix(seed)
		}
		b[i] = benchAlphabet[seed>>(6*(i%10))&63]
	}
	return string(b)
}

func benchRecord(collection, prefix string, i, payload int) (map[string]any, int) {
	s := benchMix(uint64(i) + uint64(len(collection))<<40)
	metrics := make([]float64, 13)
	for j := range metrics {
		s = benchMix(s)
		metrics[j] = float64(s%100000) / 100
	}
	p := append(make([]byte, 0, payload+64), '"')
	for chunk := benchChars(s, 64); len(p) < payload-1; {
		p = append(p, chunk...)
	}
	p = append(p[:payload-1], '"')
	id := prefix + strconv.Itoa(i)
	// IXR1 value plus flat key, as zonelog.md counts a raw record.
	raw := 13 + len(id) + 16 + 8*len(metrics) + len(p)
	return map[string]any{"Collection": collection, "Location": benchChars(s, 5), "ID": id, "Metrics": metrics, "Payload": json.RawMessage(p)}, raw
}

// TestGenerateBenchRecords writes vegetation.ndjson and large.ndjson into
// $BULKGEN and prints their raw bytes. It does nothing without BULKGEN.
func TestGenerateBenchRecords(t *testing.T) {
	dir := os.Getenv("BULKGEN")
	if dir == "" {
		t.Skip("BULKGEN names the directory of the generated records")
	}
	scale := 1.0
	if v, err := strconv.ParseFloat(os.Getenv("BULKGEN_SCALE"), 64); err == nil {
		scale = v
	}
	for _, set := range []struct {
		name, collection, prefix string
		count                    int
		payload                  func(i int) int
	}{
		{"vegetation", "AngVegetation001", "v", int(1_120_000 * scale), func(int) int { return 200 }},
		{"large", "AngLargePayload1", "L", int(20_000 * scale), func(i int) int { return 20_000 + int(benchMix(uint64(i))%40_000) }},
	} {
		f, err := os.Create(filepath.Join(dir, set.name+".ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriterSize(f, 1<<20)
		enc := json.NewEncoder(w)
		total := 0
		for i := range set.count {
			r, raw := benchRecord(set.collection, set.prefix, i, set.payload(i))
			if err := enc.Encode(r); err != nil {
				t.Fatal(err)
			}
			total += raw
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		fmt.Printf("{\"set\":%q,\"records\":%d,\"raw_bytes\":%d}\n", set.name, set.count, total)
	}
}
