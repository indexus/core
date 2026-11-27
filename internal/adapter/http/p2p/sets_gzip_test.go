package p2p

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// setsGzipBody is one IXB2 or v1 answer for a zone of `rows` children with
// DVF-like metrics.
func setsGzipBody(tb testing.TB, rows int) []byte {
	tb.Helper()
	set := wire.NewSet()
	for i := 0; i < rows; i++ {
		price := float64(150_000 + i*37%900_000)
		surface := float64(60 + i%140)
		set.Put(fmt.Sprintf("%06x", i), wire.NewAbelian(1+i%5, []float64{price, surface, price / surface, 138 + float64(i%997)/1e4, 182 + float64(i%991)/1e4}))
	}
	sets := map[string]*indexus.Set{"@": set}
	body, err := wire.EncodeSetsBlock(sets, []string{"@"}, wire.SetsPrecision, nil, wire.SetsBlockF32)
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

func TestSetsBodyIsGzippedWhenAcceptedAndLarge(t *testing.T) {
	large := setsGzipBody(t, 2000)
	small := setsGzipBody(t, 2)
	cases := []struct {
		name    string
		accept  string
		body    []byte
		gzipped bool
	}{
		{"large gzip", "gzip, deflate, br", large, true},
		{"large star", "*", large, true},
		{"large refused", "gzip;q=0, br", large, false},
		{"large none", "", large, false},
		{"small gzip", "gzip", small, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/sets", nil)
			if c.accept != "" {
				request.Header.Set("Accept-Encoding", c.accept)
			}
			if err := writeSetsBody(recorder, request, c.body); err != nil {
				t.Fatal(err)
			}
			header := recorder.Header()
			if header.Get("Vary") != "Accept-Encoding" {
				t.Fatalf("Vary = %q", header.Get("Vary"))
			}
			wire := recorder.Body.Bytes()
			if header.Get("Content-Length") != strconv.Itoa(len(wire)) {
				t.Fatalf("Content-Length %s for %d bytes", header.Get("Content-Length"), len(wire))
			}
			got := wire
			if c.gzipped {
				if header.Get("Content-Encoding") != "gzip" || len(wire) >= len(c.body) {
					t.Fatalf("encoding %q, %d bytes for %d", header.Get("Content-Encoding"), len(wire), len(c.body))
				}
				reader, err := gzip.NewReader(bytes.NewReader(wire))
				if err != nil {
					t.Fatal(err)
				}
				if got, err = io.ReadAll(reader); err != nil {
					t.Fatal(err)
				}
			} else if header.Get("Content-Encoding") != "" {
				t.Fatalf("unexpected encoding %q", header.Get("Content-Encoding"))
			}
			if !bytes.Equal(got, c.body) {
				t.Fatal("body changed on the way")
			}
		})
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                  false,
		"identity":          false,
		"gzip":              true,
		"GZIP":              true,
		"x-gzip":            true,
		"br, gzip;q=0.8":    true,
		"gzip;q=0":          false,
		"gzip; q=0.000":     false,
		"gzip;q=0.001":      true,
		"*;q=0, identity":   false,
		"deflate, br, zstd": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v", header, got)
		}
	}
}

// BenchmarkWriteSetsBody prices gzip per /sets answer of 10 000 rows: the
// difference between the identity and gzip-cold lines is the CPU a node pays
// per new body, gzip the same body read again (cache hit), and "wire" is
// what the link carries.
func BenchmarkWriteSetsBody(b *testing.B) {
	for _, format := range []string{"block"} {
		body := setsGzipBody(b, 10_000)
		for _, accept := range []string{"identity", "gzip", "gzip-cold"} {
			b.Run(format+"/"+accept, func(b *testing.B) {
				cold, header := accept == "gzip-cold", accept
				if cold {
					header = "gzip"
				}
				request := httptest.NewRequest(http.MethodGet, "/sets", nil)
				request.Header.Set("Accept-Encoding", header)
				var wire int
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if cold {
						setsGzipped = newSetsGzipCache(setsGzipCacheEntries, setsGzipCacheBytes)
					}
					recorder := httptest.NewRecorder()
					if err := writeSetsBody(recorder, request, body); err != nil {
						b.Fatal(err)
					}
					wire = recorder.Body.Len()
				}
				b.ReportMetric(float64(len(body)), "body")
				b.ReportMetric(float64(wire), "wire")
			})
		}
	}
}

// A repeated body is served from the cache with the same bytes; a changed
// body is compressed anew; the cache stays within its bounds.
func TestSetsGzipCache(t *testing.T) {
	saved := setsGzipped
	t.Cleanup(func() { setsGzipped = saved })
	setsGzipped = newSetsGzipCache(3, 1<<20)
	request := httptest.NewRequest(http.MethodGet, "/sets", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	send := func(body []byte) []byte {
		w := httptest.NewRecorder()
		if err := writeSetsBody(w, request, body); err != nil {
			t.Fatal(err)
		}
		if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
			t.Fatalf("headers %v", w.Header())
		}
		z, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(z)
		if err != nil || !bytes.Equal(plain, body) {
			t.Fatalf("gunzip: %v, equal %v", err, bytes.Equal(plain, body))
		}
		return w.Body.Bytes()
	}
	body := setsGzipBody(t, 2000)
	first := send(body)
	if len(setsGzipped.entries) != 1 {
		t.Fatalf("%d entries after one body", len(setsGzipped.entries))
	}
	if again := send(body); !bytes.Equal(first, again) {
		t.Fatal("a cached answer differs from the first")
	}
	changed := bytes.Clone(body)
	changed[len(changed)-1] ^= 1
	send(changed)
	if len(setsGzipped.entries) != 2 {
		t.Fatalf("%d entries after a changed body", len(setsGzipped.entries))
	}
	for i := 0; i < 5; i++ {
		other := bytes.Clone(body)
		other[100] = byte(i)
		send(other)
	}
	if len(setsGzipped.entries) != 3 || setsGzipped.order.Len() != 3 {
		t.Fatalf("%d entries, bound 3", len(setsGzipped.entries))
	}
	// Small bodies are not kept.
	send(setsGzipBody(t, 100))
	if len(setsGzipped.entries) != 3 {
		t.Fatal("a small body was cached")
	}
	// The byte bound evicts too.
	tiny := newSetsGzipCache(100, 400)
	for i := 0; i < 10; i++ {
		tiny.put(setsGzipKey{size: i}, make([]byte, 90))
	}
	if tiny.bytes > 400 || len(tiny.entries) != 4 {
		t.Fatalf("byte bound: %d bytes, %d entries", tiny.bytes, len(tiny.entries))
	}
	if _, ok := tiny.get(setsGzipKey{size: 9}); !ok {
		t.Fatal("the newest entry was evicted")
	}
}
