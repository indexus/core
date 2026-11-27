package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditWaitsForOpenTransfersWithoutNeedingDuplicateZones(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		open := 0
		if calls.Add(1) == 1 {
			open = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"handoffs": map[string]any{"open": map[string]int{"count": open}}, "joining": false})
	}))
	defer server.Close()
	moving, polls, err := waitForAuditQuiet(context.Background(), []string{server.URL}, 2*time.Second)
	if err != nil || moving != 0 || polls != 2 {
		t.Fatalf("moving=%d polls=%d err=%v", moving, polls, err)
	}
}

func TestAuditCannotCertifyQuietFromBusyOrUnknownTelemetry(t *testing.T) {
	for _, body := range []string{
		`{"handoffs":{"open":{"count":1}},"joining":false}`,
		`{"handoffs":{"open":{"count":0}},"joining":false,"frozen_transfers":["op"]}`,
		`{"handoffs":{"open":{"count":0}},"joining":true}`,
		`{}`, `{"handoffs":{"open":{"count":-1}},"joining":false}`, `not JSON`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, _, err := waitForAuditQuiet(context.Background(), []string{server.URL}, 0)
			if err == nil {
				t.Fatal("busy or unknown monitor was accepted as quiet")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", 503) }))
	defer server.Close()
	if _, _, err := waitForAuditQuiet(context.Background(), []string{server.URL}, 0); err == nil {
		t.Fatal("failed monitor certified quiet")
	}
	if _, _, err := waitForAuditQuiet(context.Background(), nil, 0); err == nil {
		t.Fatal("no monitors certified quiet")
	}
}

func TestAuditSettlementDeadlineDoesNotAuthorizeAnOpenTransfer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"handoffs":{"open":{"count":1}},"joining":false}`))
	}))
	defer server.Close()
	_, _, err := waitForAuditQuiet(context.Background(), []string{server.URL}, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline did not fail the audit: %v", err)
	}
}

func TestAuditDistinguishesUnavailableTotalFromNumericZero(t *testing.T) {
	var report auditReport
	report.recordTotal("unknown", 7, 0, errors.New("status 503: frozen"))
	report.recordTotal("empty", 7, 0, nil)
	if len(report.TotalErrors) != 1 || report.TotalErrors["unknown"] == "" {
		t.Fatal("unavailable total lost its error")
	}
	if _, exists := report.CountMismatches["unknown"]; exists {
		t.Fatal("unavailable total was reported as numeric zero")
	}
	if got := report.CountMismatches["empty"]; got != [2]int64{7, 0} {
		t.Fatalf("real numeric mismatch lost: %v", got)
	}
}

func TestFailedAuditSettlementStillPublishesFailure(t *testing.T) {
	out := t.TempDir()
	err := writeAuditReport(out, auditReport{SettlementError: "open transfers"})
	if err == nil {
		t.Fatal("unsettled audit passed")
	}
	raw, e := os.ReadFile(filepath.Join(out, "audit.json"))
	if e != nil {
		t.Fatal(e)
	}
	var result auditReport
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e)
	}
	if result.Pass || result.SettlementError == "" {
		t.Fatal("published report hides unsettled state")
	}
}

func TestCollectionTotalPreservesFailureReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"inventory changed during paginated read"}`))
	}))
	defer server.Close()
	_, err := collectionTotal(server.URL, "test", "Demo", true)
	if err == nil || !strings.Contains(err.Error(), "inventory changed during paginated read") {
		t.Fatalf("read failure lost server reason: %v", err)
	}
}
