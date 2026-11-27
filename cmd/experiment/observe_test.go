package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestObserveCapturesOldTransferEvenWhenMetricsRespondsQuickly(t *testing.T) {
	var captures atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/debug/stacks" {
			captures.Add(1)
			w.Write([]byte("goroutine fixture"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"handoffs":{"open":{"oldest_seconds":60}}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := observe(context.Background(), []string{"-monitors", server.URL, "-out", dir, "-every", "5ms", "-duration", "30ms"}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "stacks", "*.txt"))
	if err != nil || len(files) != 1 || captures.Load() != 1 {
		t.Fatalf("captures=%d files=%v err=%v", captures.Load(), files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || string(raw) != "goroutine fixture" {
		t.Fatalf("missing stacks: %s %v", raw, err)
	}
}
