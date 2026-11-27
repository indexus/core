package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

// Exercise the original quiet-poll assertions against the live slot census.
func waitForAuditQuiet(ctx context.Context, mons []string, settle time.Duration) (int, int, error) {
	if settle > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settle)
		defer cancel()
	}
	pool := auditMonitorPool{candidates: mons}
	_, moving, polls, err := pool.quiet(ctx, settle > 0)
	return moving, polls, err
}

func TestAuditIncludesReceiverPublishedDuringTotal(t *testing.T) {
	for _, test := range []struct {
		name                    string
		newReceiver, staleTotal bool
	}{
		{"late receiver", true, false},
		{"stale pages", false, true},
		{"both", true, true},
	} {
		t.Run(test.name, func(t *testing.T) { auditLateReceiver(t, test.newReceiver, test.staleTotal) })
	}
}

func auditLateReceiver(t *testing.T, newReceiver, stale bool) {
	out := t.TempDir()
	file := filepath.Join(out, "monitors")
	quiet := `{"handoffs":{"open":{"count":0}},"joining":false}`
	var received atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ownership":
			received.Add(1)
			_, _ = w.Write([]byte(`{"C":{"new":{}}}`))
		case "/metrics":
			_, _ = w.Write([]byte(quiet))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer receiver.Close()
	var totals atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/issue/token":
			_, _ = w.Write([]byte(`{"token":"test"}`))
		case "/records":
			_, _ = w.Write([]byte(`{"records":[{"id":"w0","location":"L"}]}`))
		case "/metrics":
			_, _ = w.Write([]byte(quiet))
		case "/ownership":
			_, _ = w.Write([]byte(`{"C":{"old":{}}}`))
		case "/set":
			if totals.Add(1) == 1 {
				// The agent registry changes during the first exact read. Its
				// replacement file deliberately omits the original source.
				if newReceiver {
					if err := os.WriteFile(file, []byte(receiver.URL), 0600); err != nil {
						t.Error(err)
					}
				}
				if stale {
					w.WriteHeader(503)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": indexus.ErrReadChanged.Error()})
					return
				}
			}
			_, _ = w.Write([]byte(`{"set":{"L":{"count":1}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer source.Close()
	if err := os.WriteFile(file, []byte(source.URL), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "acked.jsonl"), []byte("{\"c\":\"C\",\"l\":\"L\",\"i\":\"w0\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := audit(context.Background(), []string{"-targets", source.URL, "-monitors-file", file, "-issuer", source.URL, "-out", out, "-settle", "3s"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "audit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report auditReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	nodes := 1
	if newReceiver {
		nodes++
	}
	if !report.Pass || report.Checked != 1 || report.Collections != 1 || report.Zones != nodes || report.NodesAnswering != nodes || report.FinalRetries != 1 || totals.Load() != 2 || int(received.Load()) != nodes-1 {
		t.Fatalf("totals=%d receiver snapshots=%d report=%+v", totals.Load(), received.Load(), report)
	}
}

func TestFinalAuditBoundsSlowTotalAndRejectsPermanentRefusal(t *testing.T) {
	for _, slow := range []bool{false, true} {
		var totals atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/metrics":
				_, _ = w.Write([]byte(`{"handoffs":{"open":{"count":0}},"joining":false}`))
			case "/set":
				totals.Add(1)
				if slow {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		}))
		var report auditReport
		_, err := auditFinal(context.Background(), &report, auditMonitorPool{candidates: []string{server.URL}}, []string{server.URL}, "token", map[string]int64{"C": 1}, 300*time.Millisecond)
		server.Close()
		if slow && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("slow total escaped deadline: %v", err)
		}
		if totals.Load() != 1 || len(report.TotalErrors) != 1 || report.FinalRetries != 0 {
			t.Fatalf("calls=%d report=%+v", totals.Load(), report)
		}
	}
}
