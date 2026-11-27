package wire

import (
	"encoding/json"
	"math"
)

type Abelian struct {
	count   int
	metrics []float64
	// Low-order terms retained across local additions and inverses. The public
	// JSON representation remains count + corrected metrics.
	compensation []float64
}

// Implementing json.Marshaler interface without a nested named-type alloc dance.
func (a *Abelian) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Count   int       `json:"count"`
		Metrics []float64 `json:"metrics"`
	}{
		Count:   a.count,
		Metrics: a.Metrics(),
	})
}

// Implementing json.Unmarshaler interface
func (a *Abelian) UnmarshalJSON(data []byte) error {
	type Obj struct {
		Count   int       `json:"count"`
		Metrics []float64 `json:"metrics"`
	}
	obj := &Obj{}
	if err := json.Unmarshal(data, obj); err != nil {
		return err
	}
	a.count = obj.Count
	a.metrics = obj.Metrics
	a.compensation = nil
	return nil
}

func NewAbelian(count int, metrics []float64) *Abelian {
	return &Abelian{
		count:   count,
		metrics: metrics,
	}
}

func (a *Abelian) IsEmpty() bool {
	if a.count != 0 {
		return false
	}
	for idx := range a.metrics {
		if a.Metric(idx) != 0 {
			return false
		}
	}
	return true
}

func (a *Abelian) Count() int {
	return a.count
}

// Metrics exposes corrected floating sums. Aggregation remains IEEE-754
// arithmetic; the compensation is local state, not part of the wire schema.
func (a *Abelian) Metrics() []float64 {
	if len(a.compensation) == 0 {
		return a.metrics
	}
	out := make([]float64, len(a.metrics))
	for i := range out {
		out[i] = a.Metric(i)
	}
	return out
}

// Metric reads one position, or 0 when the value does not carry it. Nothing
// forces a collection to hold items of one metric width — Sum says so — and an
// aggregate can reach a read with no metrics at all. Indexing Metrics() from a
// request handler therefore panics on data the mesh considers valid.
func (a *Abelian) Metric(idx int) float64 {
	if a == nil || idx < 0 || idx >= len(a.metrics) {
		return 0
	}
	value := a.metrics[idx]
	if idx < len(a.compensation) {
		value += a.compensation[idx]
	}
	return value
}

func (a *Abelian) Clone() *Abelian {
	metrics := make([]float64, len(a.metrics))
	copy(metrics, a.metrics)
	return &Abelian{
		count: a.count, metrics: metrics,
		compensation: append([]float64(nil), a.compensation...),
	}
}

func (a *Abelian) IsEqual(b *Abelian) bool {
	if b == nil {
		return false
	}
	if a.count != b.count {
		return false
	}
	if len(a.metrics) != len(b.metrics) {
		return false
	}
	for idx := range a.metrics {
		if a.Metric(idx) != b.Metric(idx) {
			return false
		}
	}
	return true
}

// Sum folds a delta in, position by position. Deltas come from items and from
// peer aggregates, and nothing forces a collection to hold items of one metric
// width: a narrower delta only touches the positions it carries, and a wider
// one grows the total to fit.
//
// Growing matters because a counter raised by Set.incr starts with no metrics
// at all. Only widening on nil left such a total pinned at length zero, so it
// absorbed the count of every later item and none of their metrics — a cell
// reporting real transactions with a price, a latitude and a longitude of 0.
func (a *Abelian) Sum(delta *Abelian) { a.accumulate(delta, 1) }

// Substract is Sum's inverse, including metric widening and compensation.
func (a *Abelian) Substract(delta *Abelian) { a.accumulate(delta, -1) }

func (a *Abelian) accumulate(delta *Abelian, sign int) {
	if delta == nil {
		return
	}
	a.count += sign * delta.count
	a.widen(len(delta.metrics))
	for idx, value := range delta.metrics {
		correction := 0.0
		if idx < len(delta.compensation) {
			correction = delta.compensation[idx]
		}
		a.addMetric(idx, float64(sign)*value)
		a.addMetric(idx, float64(sign)*correction)
	}
}

// widen grows metrics to at least n positions, preserving what is already
// there. Positions a value never carried read as 0, which is their identity.
func (a *Abelian) widen(n int) {
	if len(a.metrics) < n {
		grown := make([]float64, n)
		copy(grown, a.metrics)
		a.metrics = grown
	}
	if len(a.compensation) < len(a.metrics) {
		grown := make([]float64, len(a.metrics))
		copy(grown, a.compensation)
		a.compensation = grown
	}
}

// Neumaier accumulation preserves small terms even when a later inverse
// cancels a much larger value. Public reads round the corrected sum once.
func (a *Abelian) addMetric(i int, x float64) {
	old := a.metrics[i]
	next := old + x
	if math.IsInf(next, 0) || math.IsNaN(next) {
		a.compensation[i] = 0
	} else if math.Abs(old) >= math.Abs(x) {
		a.compensation[i] += (old - next) + x
	} else {
		a.compensation[i] += (x - next) + old
	}
	a.metrics[i] = next
}
