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

type refusedTransport struct{ calls atomic.Int32 }

func (t *refusedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("dial tcp: connection refused")
}
func TestReadBackDoesNotWaitOnRetiredEndpoint(t *testing.T) {
	previous := client
	tr := &refusedTransport{}
	client = &http.Client{Transport: tr}
	defer func() { client = previous }()
	_, err := readBack(context.Background(), "http://retired", "test", key{})
	if err == nil || !strings.Contains(err.Error(), "connection refused") || tr.calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, tr.calls.Load())
	}
}
func TestAuditMismatchRemainsFailureWithFullOrSampledReadback(t *testing.T) {
	for _, sample := range []string{"0", "1"} {
		t.Run(sample, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/issue/token":
					_, _ = w.Write([]byte(`{"token":"test"}`))
				case "/records":
					reads.Add(1)
					_, _ = w.Write([]byte(`{"records":[{"id":"w0","location":"L"},{"id":"w1","location":"L"}]}`))
				case "/metrics":
					_, _ = w.Write([]byte(`{"handoffs":{"open":{"count":0}},"joining":false}`))
				case "/ownership":
					_, _ = w.Write([]byte(`{"C":{"L":{}}}`))
				case "/status":
					_, _ = w.Write([]byte(`{"items":3}`))
				case "/set":
					_, _ = w.Write([]byte(`{"set":{"L":{"count":3}}}`))
				default:
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer server.Close()
			out := t.TempDir()
			if err := os.WriteFile(filepath.Join(out, "acked.jsonl"), []byte("{\"c\":\"C\",\"l\":\"L\",\"i\":\"w0\"}\n{\"c\":\"C\",\"l\":\"L\",\"i\":\"w1\"}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := audit(context.Background(), []string{"-targets", server.URL, "-monitors", server.URL, "-issuer", server.URL, "-out", out, "-sample", sample, "-settle", "0"})
			if err == nil {
				t.Fatal("mismatched total passed")
			}
			data, e := os.ReadFile(filepath.Join(out, "audit.json"))
			if e != nil {
				t.Fatal(e)
			}
			var r auditReport
			if e = json.Unmarshal(data, &r); e != nil {
				t.Fatal(e)
			}
			if r.Pass || r.ItemsMatch || r.CountMismatches["C"] != [2]int64{2, 3} || r.Missing != 0 || r.ReadErrors != 0 || r.MismatchLost != 0 {
				t.Fatalf("%+v", r)
			}
			wantReads := int32(2)
			wantMismatch := 0
			if sample == "1" {
				wantReads = 3
				wantMismatch = 2
			}
			if reads.Load() != wantReads || r.MismatchChecked != wantMismatch {
				t.Fatalf("reads=%d mismatch=%d", reads.Load(), r.MismatchChecked)
			}
		})
	}
}

// Cancellation must stop an in-flight HTTP request and leave a failed,
// explicitly partial report rather than claiming every planned key was read.
func TestAuditCancellationStopsReadsAndReportsIncomplete(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/issue/token" {
			_, _ = w.Write([]byte(`{"token":"test"}`))
			return
		}
		if r.URL.Path == "/records" {
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	out := t.TempDir()
	ledger := strings.Repeat("{\"c\":\"C\",\"l\":\"L\",\"i\":\"w0\"}\n", 1000)
	if err := os.WriteFile(filepath.Join(out, "acked.jsonl"), []byte(ledger), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- audit(ctx, []string{"-targets", server.URL, "-monitors", server.URL, "-issuer", server.URL, "-out", out, "-sample", "0", "-settle", "0"})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("audit did not start read")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled audit passed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled audit kept reading")
	}
	raw, err := os.ReadFile(filepath.Join(out, "audit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report auditReport
	if err = json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Pass || !report.Incomplete || report.Canceled == "" || report.Checked >= report.Acked {
		t.Fatalf("cancellation hidden: %+v", report)
	}
}
