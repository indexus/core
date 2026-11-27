package storage

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// countingReader serves n rows in order and counts those it hands out.
type countingReader struct {
	zonelog.Reader
	n, served int
}

func (r *countingReader) ScanWith(_, _ string, after *zonelog.Record, _ *zonelog.IDRange, _ func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	for i := range r.n {
		id := fmt.Sprintf("%06d", i)
		if after != nil && id <= after.ID {
			continue
		}
		r.served++
		if !visit(zonelog.Record{Location: "@", ID: id}, 1) {
			return nil
		}
	}
	return nil
}

// A paged scan whose visitor stops at its first row is a seek: it reads a
// first page of scanFirstPage rows, not scanPage. Every transaction that
// checks capacity seeks once per staging session (stagingSessions); with
// 512-row pages a receiver holding many sessions spent its writer there
// (AWS 2026-11-26: 140 to 300 ms per control commit). A full scan still
// visits every row once, in order.
func TestScanPagedStartsWithASmallPage(t *testing.T) {
	seek := &countingReader{n: 5000}
	visited := 0
	if err := scanPaged(seek, "c", "@", nil, nil, nil, func(zonelog.Record, int) bool { visited++; return false }); err != nil {
		t.Fatal(err)
	}
	if visited != 1 || seek.served > scanFirstPage {
		t.Fatalf("a seek visited %d rows and read %d, want 1 and at most %d", visited, seek.served, scanFirstPage)
	}
	full := &countingReader{n: 5000}
	next := 0
	err := scanPaged(full, "c", "@", nil, nil, nil, func(r zonelog.Record, _ int) bool {
		if r.ID != fmt.Sprintf("%06d", next) {
			t.Fatalf("row %q at position %d", r.ID, next)
		}
		next++
		return true
	})
	if err != nil || next != full.n || full.served != full.n {
		t.Fatalf("full scan: err %v, visited %d, read %d, want %d", err, next, full.served, full.n)
	}
}
