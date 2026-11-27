package wire

import (
	"encoding/json"
	"math/big"
	"testing"
)

// A concentrated zone exposed this drift at 72k rows. The reference multiplies
// exact binary inputs at 256-bit precision, independent of production summation.
func TestAggregateConcentratedFloatsMatchHighPrecision(t *testing.T) {
	const n = 72012
	values := []float64{-44.40712639866468, -121.64598485992325}
	total := NewAbelian(0, nil)
	for range n {
		total.Sum(NewAbelian(1, values))
	}
	if total.Count() != n {
		t.Fatal(total.Count())
	}
	for i, x := range values {
		exact := new(big.Float).SetPrec(256).SetFloat64(x)
		exact.Mul(exact, new(big.Float).SetPrec(256).SetInt64(n))
		want, _ := exact.Float64()
		if got := total.Metric(i); got != want {
			t.Fatalf("metric %d drift: got %.17g want %.17g", i, got, want)
		}
	}
}

func TestAggregateCompensationSurvivesCloneMergeAndCancellation(t *testing.T) {
	first := NewAbelian(1, []float64{1e16})
	first.Sum(NewAbelian(1, []float64{1}))
	clone := first.Clone()
	merged := NewAbelian(0, nil)
	merged.Sum(clone)
	merged.Substract(NewAbelian(1, []float64{1e16}))
	if merged.Count() != 1 || merged.Metric(0) != 1 {
		t.Fatalf("lost low-order value: count=%d metric=%g", merged.Count(), merged.Metric(0))
	}
	if first.Metric(0) != clone.Metric(0) {
		t.Fatal("clone changed source")
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	var restored Abelian
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.IsEqual(merged) || restored.Metrics()[0] != 1 {
		t.Fatalf("JSON lost value: %s", raw)
	}
	restored.Substract(NewAbelian(1, []float64{1}))
	if !restored.IsEmpty() {
		t.Fatal("inverse did not cancel")
	}
	// Unmarshal into an already accumulated object must discard old correction.
	if err = json.Unmarshal([]byte(`{"count":1,"metrics":[3]}`), clone); err != nil {
		t.Fatal(err)
	}
	if clone.Metric(0) != 3 {
		t.Fatal("unmarshal retained an obsolete correction")
	}
}
