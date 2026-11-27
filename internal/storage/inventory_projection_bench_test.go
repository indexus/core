package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"
)

func BenchmarkInventoryPayloadProjection(b *testing.B) {
	s, err := OpenWithOptions(filepath.Join(b.TempDir(), "projection.db"), "D", "test", 1<<28, Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	holder, _ := s.Endpoint("D")
	if err = s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		b.Fatal(err)
	}
	payload := json.RawMessage(`{"text":"` + string(bytes.Repeat([]byte("x"), 8192)) + `"}`)
	for i := 0; i < 64; i++ {
		if _, _, _, err = s.ApplyRecord(fmt.Sprint(i), Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprint(i), Metrics: []float64{1, 2, 3}, Payload: payload}); err != nil {
			b.Fatal(err)
		}
	}
	z, _ := tree.NewZoneKey("Demo000000000000", "a")
	for _, metrics := range []bool{false, true} {
		b.Run(fmt.Sprint("metrics-only=", metrics), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				rows, page, e := s.CausalInventoryPage(context.Background(), &z, InventoryPageRequest{MetricsOnly: metrics})
				if e != nil || !page.Done || len(rows[0].Records) != 64 {
					b.Fatalf("%v %v", e, page)
				}
				if i == 0 {
					raw, _ := json.Marshal(rows)
					b.ReportMetric(float64(len(raw)), "response-bytes")
				}
			}
		})
	}
}
