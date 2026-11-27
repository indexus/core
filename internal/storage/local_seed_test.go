package storage

import (
	"errors"
	"reflect"
	"testing"
)

func TestLocalSeedIsAtomicIdempotentAndSealable(t *testing.T) {
	s, _ := routingStore(t)
	rows := []Record{{Collection: "Demo000000000000", Location: "aa", ID: "one", Metrics: []float64{1, 2}}, {Collection: "Demo000000000000", Location: "ab", ID: "two", Metrics: []float64{3}}}
	if e := s.SeedLocalRecords(rows); e != nil {
		t.Fatal(e)
	}
	before := assertPlanningParity(t, s)
	if e := s.SeedLocalRecords(rows); e != nil {
		t.Fatal(e)
	}
	if after := assertPlanningParity(t, s); !reflect.DeepEqual(before, after) {
		t.Fatal("retry changed seed")
	}
	bad := append([]Record{{Collection: "Demo000000000000", Location: "ac", ID: "new", Metrics: []float64{1}}}, rows[0])
	bad[1].Metrics = []float64{99}
	if e := s.SeedLocalRecords(bad); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if after := assertPlanningParity(t, s); !reflect.DeepEqual(before, after) {
		t.Fatal("failed batch partly committed")
	}
	if e := s.SealLocalSeed(); e != nil {
		t.Fatal(e)
	}
	if e := s.SeedLocalRecords(rows); e == nil {
		t.Fatal("sealed seed accepted records")
	}
	if _, _, _, e := s.ApplyRecord("client-write", Record{Collection: "Demo000000000000", Location: "aa", ID: "one", Metrics: []float64{4}}); e != nil {
		t.Fatal(e)
	}
	assertPlanningParity(t, s)
}
