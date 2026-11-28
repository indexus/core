package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStepsReplayUsesDistinctLogicalWrites(t *testing.T) {
	gen, err := makeScenario("steps", "Ab_2-x", 256, 0, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(23, 7))
	replay := rand.New(rand.NewPCG(23, 7))
	other := rand.New(rand.NewPCG(24, 7))
	locations := make(map[string]bool)
	collections := make(map[string]bool)
	for i := int64(0); i < 4096; i++ {
		k, _ := gen(rng, i, 0)
		again, _ := gen(replay, i, 0)
		if again != k || k.ID != fmt.Sprint("w", i) {
			t.Fatalf("key %d: generated %+v, replayed %+v", i, k, again)
		}
		another, _ := gen(other, i, 0)
		if k.Location == another.Location {
			t.Fatalf("both fixed seeds produced the same location for write %d", i)
		}
		locations[k.Location] = true
		collections[k.Collection] = true
	}
	if len(locations) != 4096 || len(collections) != 256 {
		t.Fatalf("fixed workload yielded %d locations across %d collections", len(locations), len(collections))
	}
}

func TestFreshnessProbeRetriesIdenticalPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var attempts [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/issue/token":
			fmt.Fprint(w, `{"token":"token"}`)
		case "/item":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, raw)
			if len(attempts) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case "/set":
			fmt.Fprint(w, `{"set":{"item":{"count":1}}}`)
			cancel()
		}
	}))
	defer server.Close()
	if err := load(ctx, []string{"-targets", server.URL, "-issuer", server.URL, "-out", t.TempDir(),
		"-rate", "0", "-workers", "1", "-probe-every", "5ms", "-duration", "5s", "-tag", "Test"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) < 2 || !bytes.Equal(attempts[0], attempts[1]) {
		t.Fatalf("expected an identical probe following ambiguous failure, got %q", attempts)
	}
}

func TestLoadReportsTerminalWriteFailuresSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/issue/token":
			fmt.Fprint(w, `{"token":"fixture"}`)
		case "/item":
			http.Error(w, "unsupported", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	err := load(context.Background(), []string{
		"-targets", server.URL, "-issuer", server.URL, "-out", dir,
		"-duration", "1200ms", "-workers", "2", "-rate", "20",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := os.Open(filepath.Join(dir, "load.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var sample second
	if err := json.NewDecoder(stream).Decode(&sample); err != nil {
		t.Fatal(err)
	}
	if sample.Acked != 0 || sample.Exhausted == 0 || len(sample.ExhaustedLatency) == 0 ||
		sample.Failed[http.StatusBadRequest] == 0 {
		t.Fatalf("missing terminal failure telemetry: %+v", sample)
	}
}
