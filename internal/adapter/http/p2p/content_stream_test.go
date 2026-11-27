package p2p

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type streamContentService struct {
	countingService
	rows []indexus.Item
}

func (s *streamContentService) GetRecordsContext(context.Context, string, string, int) ([]indexus.Item, error) {
	return s.rows, nil
}
func contentFixture(t *testing.T, raw []byte, zipped bool) []indexus.Item {
	t.Helper()
	payload := raw
	encoding := "identity-base64"
	if zipped {
		var b bytes.Buffer
		z := gzip.NewWriter(&b)
		_, _ = z.Write(raw)
		_ = z.Close()
		payload = b.Bytes()
		encoding = "gzip-base64"
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	rows := []indexus.Item{}
	chunk := max(1, (len(payload)+2)/3)
	for at := 0; at < len(payload) || at == 0; at += chunk {
		p := contentPart{Path: "test.bin", Encoding: encoding, SHA256: hash, Bytes: int64(len(raw)), Part: len(rows), Data: base64.StdEncoding.EncodeToString(payload[at:min(at+chunk, len(payload))])}
		body, _ := json.Marshal(p)
		rows = append(rows, indexus.Item{Payload: body})
		if len(payload) == 0 {
			break
		}
	}
	for i := range rows {
		var p contentPart
		_ = json.Unmarshal(rows[i].Payload, &p)
		p.Parts = len(rows)
		rows[i].Payload, _ = json.Marshal(p)
	}
	return rows
}
func TestContentStreamIdentityAndGzip(t *testing.T) {
	raw := bytes.Repeat([]byte("bounded binary scene attributes\x00"), 10000)
	for _, stored := range []bool{false, true} {
		for _, codec := range []string{"identity", "gzip", "auto"} {
			t.Run(fmt.Sprint(stored, "/", codec), func(t *testing.T) {
				h := &Handler{Service: &streamContentService{rows: contentFixture(t, raw, stored)}}
				r := httptest.NewRequest("GET", "/content?collection=Assets&location=0000000000000000&path=test.bin&encoding="+codec, nil)
				w := httptest.NewRecorder()
				h.GetContent(w, r)
				if w.Code != 200 {
					t.Fatalf("%d: %s", w.Code, w.Body)
				}
				var decoded io.Reader = w.Body
				if w.Header().Get("Content-Encoding") == "gzip" {
					z, e := gzip.NewReader(w.Body)
					if e != nil {
						t.Fatal(e)
					}
					defer z.Close()
					decoded = z
				}
				got, e := io.ReadAll(decoded)
				if e != nil || !bytes.Equal(got, raw) {
					t.Fatalf("round trip: %v", e)
				}
				if w.Header().Get("Cache-Control") != "no-store, no-transform" {
					t.Fatal("proxy may recompress identity experiment")
				}
				if codec == "identity" && w.Header().Get("Content-Encoding") != "" {
					t.Fatal("identity was compressed")
				}
			})
		}
	}
}
func TestContentEnvelopeRejectsMixedPartsAndExpansionBudget(t *testing.T) {
	rows := contentFixture(t, []byte("payload"), true)
	var p contentPart
	_ = json.Unmarshal(rows[1].Payload, &p)
	p.Bytes++
	rows[1].Payload, _ = json.Marshal(p)
	if _, _, err := contentEnvelope(rows, "test.bin"); err == nil {
		t.Fatal("mixed asset accepted")
	}
	rows = contentFixture(t, []byte("payload"), false)
	_ = json.Unmarshal(rows[0].Payload, &p)
	p.Bytes = wire.MaxContentBytes + 1
	rows[0].Payload, _ = json.Marshal(p)
	if _, _, err := contentEnvelope(rows, "test.bin"); err == nil {
		t.Fatal("budget accepted")
	}
	if _, _, err := contentEnvelope(contentFixture(t, nil, false), "other"); err == nil {
		t.Fatal("wrong path accepted")
	}
}

type gatedContentWriter struct {
	*httptest.ResponseRecorder
	first   chan int
	gate    chan struct{}
	started bool
}

func (w *gatedContentWriter) Write(p []byte) (int, error) {
	n, e := w.ResponseRecorder.Write(p)
	if !w.started {
		w.started = true
		w.first <- n
		<-w.gate
	}
	return n, e
}
func TestContentStreamsBeforeTheWholeDecodedBodyIsPrepared(t *testing.T) {
	raw := bytes.Repeat([]byte(strings.Repeat("a", 1024)), 1024)
	h := &Handler{Service: &streamContentService{rows: contentFixture(t, raw, true)}}
	w := &gatedContentWriter{ResponseRecorder: httptest.NewRecorder(), first: make(chan int, 1), gate: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		h.GetContent(w, httptest.NewRequest("GET", "/content?collection=Assets&location=0000000000000000&path=test.bin&encoding=identity", nil))
		close(done)
	}()
	n := <-w.first
	if n >= len(raw) {
		t.Fatal("whole asset buffered")
	}
	close(w.gate)
	<-done
	if !bytes.Equal(w.Body.Bytes(), raw) {
		t.Fatal("stream changed bytes")
	}
}

type contentSink struct {
	header http.Header
	bytes  int
}

func (s *contentSink) Header() http.Header         { return s.header }
func (s *contentSink) WriteHeader(int)             {}
func (s *contentSink) Write(p []byte) (int, error) { s.bytes += len(p); return len(p), nil }

// Production handler, resident canonical records, no TCP/disk cost. Use an
// actual render fixture via INDEXUS_BENCH_CONTENT; fixture setup is not timed.
func BenchmarkContentProjection(b *testing.B) {
	path := os.Getenv("INDEXUS_BENCH_CONTENT")
	if path == "" {
		b.Skip("set INDEXUS_BENCH_CONTENT to a render fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	makeRows := func(zipped bool) []indexus.Item {
		payload := raw
		encoding := "identity-base64"
		if zipped {
			var packed bytes.Buffer
			z, _ := gzip.NewWriterLevel(&packed, gzip.BestSpeed)
			_, _ = z.Write(raw)
			_ = z.Close()
			payload = packed.Bytes()
			encoding = "gzip-base64"
		}
		var rows []indexus.Item
		parts := max(1, (len(payload)+(256<<10)-1)/(256<<10))
		for i := 0; i < parts; i++ {
			p := contentPart{Path: "test.bin", Encoding: encoding, SHA256: hash, Bytes: int64(len(raw)), Part: i, Parts: parts, Data: base64.StdEncoding.EncodeToString(payload[min(i*(256<<10), len(payload)):min((i+1)*(256<<10), len(payload))])}
			body, _ := json.Marshal(p)
			rows = append(rows, indexus.Item{Payload: body})
		}
		return rows
	}
	identity, gzipped := makeRows(false), makeRows(true)
	for _, c := range []struct {
		name, codec string
		rows        []indexus.Item
		records     bool
	}{{"identity-storage_identity-wire", "identity", identity, false}, {"gzip-storage_identity-wire", "identity", gzipped, false}, {"gzip-storage_gzip-wire", "gzip", gzipped, false}, {"identity-storage_gzip-wire", "gzip", identity, false}, {"gzip-storage_json-base64", "", gzipped, true}} {
		b.Run(c.name, func(b *testing.B) {
			h := &Handler{Service: &streamContentService{rows: c.rows}}
			url := "/content?collection=Assets&location=0000000000000000&path=test.bin&encoding=" + c.codec
			if c.records {
				url = "/records?collection=Assets&location=0000000000000000&limit=1024"
			}
			request := httptest.NewRequest("GET", url, nil)
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			var size int
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := &contentSink{header: make(http.Header)}
				if c.records {
					h.GetRecords(w, request)
				} else {
					h.GetContent(w, request)
				}
				size = w.bytes
			}
			b.ReportMetric(float64(size), "wire-B")
		})
	}
}

// Real HTTP server/client, without gzip in storage, server or client. The same
// bytes must also survive native HTTP gzip decoding with gzip-backed records.
func TestContentRoundTripOverHTTP(t *testing.T) {
	raw := bytes.Repeat([]byte("scene attributes\x00"), 10000)
	for _, zipped := range []bool{false, true} {
		h := &Handler{Service: &streamContentService{rows: contentFixture(t, raw, zipped)}}
		server := httptest.NewServer(http.HandlerFunc(h.GetContent))
		codec := "identity"
		if zipped {
			codec = "gzip"
		}
		client := server.Client()
		response, err := client.Get(server.URL + "/content?collection=Assets&location=0000000000000000&path=test.bin&encoding=" + codec)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		got, err := io.ReadAll(response.Body)
		response.Body.Close()
		server.Close()
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("HTTP round trip: %v", err)
		}
		if response.Uncompressed != zipped || response.Header.Get("Content-Encoding") != "" {
			t.Fatal("unexpected wire compression")
		}
	}
}
