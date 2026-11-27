package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// writer posts records through the mesh, as any authenticated client does:
// the receiving node forwards each write to the zone's authority holder, so
// the data lands on the vnodes the XOR geometry names.
type writer struct {
	endpoints []string
	token     string
	client    *http.Client
	retryFor  time.Duration
}

func issueToken(issuer string) (string, error) {
	body := bytes.NewBufferString(`{"client_id":"localdata-write","scopes":["read","write"]}`)
	response, err := http.Post(issuer+"/v1/issue/token", "application/json", body)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var issued struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(response.Body).Decode(&issued); err != nil || response.StatusCode != http.StatusOK || issued.Token == "" {
		return "", fmt.Errorf("issuer %s: no write token (HTTP %d)", issuer, response.StatusCode)
	}
	return issued.Token, nil
}

// requestID names one version of a record: a retry, or a rerun after an
// interruption, replays the same write, and a new version of the record (other
// metrics or payload) is a new write that replaces it.
func requestID(r storage.Record) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%v\x00", r.Collection, r.Location, r.ID, r.Metrics)
	h.Write(r.Payload)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (w *writer) post(endpoint string, r storage.Record) error {
	item := map[string]any{"request_id": requestID(r), "collection": r.Collection, "location": r.Location, "id": r.ID, "metrics": r.Metrics}
	if len(r.Payload) > 0 {
		item["payload"] = r.Payload
	}
	body, err := json.Marshal(map[string]any{"item": item, "root": "@"})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(w.retryFor)
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequest(http.MethodPost, endpoint+"/item", bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+w.token)
		response, err := w.client.Do(request)
		status := 0
		var detail []byte
		if err == nil {
			status = response.StatusCode
			detail, _ = io.ReadAll(io.LimitReader(response.Body, 512))
			response.Body.Close()
			if status == http.StatusCreated {
				return nil
			}
			// Only an unavailable or busy node is worth waiting for.
			if status != http.StatusServiceUnavailable && status != http.StatusTooManyRequests && status < 500 {
				return fmt.Errorf("%s/%s/%s: HTTP %d %s", r.Collection, r.Location, r.ID, status, bytes.TrimSpace(detail))
			}
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("HTTP %d %s", status, bytes.TrimSpace(detail))
			}
			return fmt.Errorf("%s/%s/%s: %w", r.Collection, r.Location, r.ID, err)
		}
		time.Sleep(min(time.Duration(attempt+1)*200*time.Millisecond, 3*time.Second))
	}
}

// writeRecords posts every NDJSON record of input with concurrency workers,
// spread over the endpoints, and stops at the first definitive failure.
func (w *writer) writeRecords(input io.Reader, concurrency int) (int64, error) {
	lines := make(chan []byte, concurrency*2)
	var written atomic.Int64
	var failure error
	var once sync.Once
	stop := make(chan struct{})
	fail := func(err error) {
		once.Do(func() {
			failure = err
			close(stop)
		})
	}
	var workers sync.WaitGroup
	started := time.Now()
	for worker := range concurrency {
		workers.Go(func() {
			endpoint := w.endpoints[worker%len(w.endpoints)]
			for line := range lines {
				select {
				case <-stop:
					continue
				default:
				}
				var r storage.Record
				if err := json.Unmarshal(line, &r); err != nil {
					fail(err)
					continue
				}
				if err := w.post(endpoint, r); err != nil {
					fail(err)
					continue
				}
				if n := written.Add(1); n%10000 == 0 {
					fmt.Fprintf(os.Stderr, "written %d records (%.0f/s)\n", n, float64(n)/time.Since(started).Seconds())
				}
			}
		})
	}
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 4<<20)
read:
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
		select {
		case lines <- line:
		case <-stop:
			break read
		}
	}
	close(lines)
	workers.Wait()
	if failure == nil {
		failure = scan.Err()
	}
	return written.Load(), failure
}
