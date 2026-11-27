package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestDrainCancelsAnOutstandingHTTPRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		res, _ := server.Client().Do(req)
		if res != nil {
			res.Body.Close()
		}
	}()
	<-started
	if drainWorkers(&wg, cancel, 20*time.Millisecond) {
		t.Fatal("blocked request reported drained")
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("request context was not canceled")
	}
}

func TestCompletedDrainDoesNotCancelRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	if !drainWorkers(&wg, cancel, time.Second) || ctx.Err() != nil {
		t.Fatal("completed drain was cancelled")
	}
}
