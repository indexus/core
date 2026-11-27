package p2p

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// setsGzipMinBytes keeps small answers as they are: under about one packet
// gzip saves no round trip and still costs a writer reset and a header.
const setsGzipMinBytes = 1400

// setsGzipMaxPooled bounds what a pooled buffer may keep between requests, so
// one huge read does not pin its memory for the life of the process.
const setsGzipMaxPooled = 4 << 20

// setsGzipLevel 2 is the fastest level of the full matcher. Level 1
// (BestSpeed) uses a cheaper matcher that misses most repeats in the float32
// columns: on 10 000 IXB2 rows it leaves 124 kB where level 2 leaves 85 kB,
// for about 1 ms more per answer; levels 3 and up gain under 0.1 %.
const setsGzipLevel = 2

var setsGzipBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// writeSetsBody sends a /sets body, gzipped when the client accepts it and
// the body is worth it. The body is compressed whole so the answer keeps a
// Content-Length: the SDK reads it as the bytes the link carried.
func writeSetsBody(w http.ResponseWriter, r *http.Request, body []byte) error {
	w.Header().Add("Vary", "Accept-Encoding")
	if len(body) < setsGzipMinBytes || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, err := w.Write(body)
		return err
	}
	var key setsGzipKey
	cacheable := len(body) >= setsGzipCacheMinBytes
	if cacheable {
		key = setsGzipKey{sum: sha256.Sum256(body), size: len(body)}
		if zipped, ok := setsGzipped.get(key); ok {
			return writeSetsGzip(w, zipped)
		}
	}
	buffer := setsGzipBuffers.Get().(*bytes.Buffer)
	buffer.Reset()
	defer func() {
		if buffer.Cap() <= setsGzipMaxPooled {
			setsGzipBuffers.Put(buffer)
		}
	}()
	zw := setsGzip.get(buffer)
	_, err := zw.Write(body)
	if err == nil {
		err = zw.Close()
	}
	setsGzip.put(zw)
	if err != nil {
		return err
	}
	if cacheable {
		setsGzipped.put(key, bytes.Clone(buffer.Bytes()))
	}
	return writeSetsGzip(w, buffer.Bytes())
}

func writeSetsGzip(w http.ResponseWriter, zipped []byte) error {
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(zipped)))
	_, err := w.Write(zipped)
	return err
}

// The gzip of recent /sets bodies, by the SHA-256 of the body: map tiles and
// dashboards read the same zones again and again, and gzip costs as much as
// encoding the body. The key is the body itself, so an entry can never be
// stale; there is no version to trust. Bodies under setsGzipCacheMinBytes
// are cheaper to compress again than to keep.
const (
	setsGzipCacheMinBytes = 16 << 10
	setsGzipCacheBytes    = 16 << 20 // gzipped bytes kept, at most
	setsGzipCacheEntries  = 256
)

type setsGzipKey struct {
	sum  [sha256.Size]byte
	size int
}

var setsGzipped = newSetsGzipCache(setsGzipCacheEntries, setsGzipCacheBytes)

// setsGzipCache is a small LRU: a map and a list, under one mutex.
type setsGzipCache struct {
	mu         sync.Mutex
	entries    map[setsGzipKey]*list.Element
	order      *list.List // front: most recent
	bytes      int
	maxEntries int
	maxBytes   int
}

type setsGzipEntry struct {
	key    setsGzipKey
	zipped []byte
}

func newSetsGzipCache(entries, bytes int) *setsGzipCache {
	return &setsGzipCache{entries: map[setsGzipKey]*list.Element{}, order: list.New(), maxEntries: entries, maxBytes: bytes}
}

func (c *setsGzipCache) get(key setsGzipKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*setsGzipEntry).zipped, true
}

func (c *setsGzipCache) put(key setsGzipKey, zipped []byte) {
	if len(zipped) > c.maxBytes/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		return
	}
	c.entries[key] = c.order.PushFront(&setsGzipEntry{key: key, zipped: zipped})
	c.bytes += len(zipped)
	for len(c.entries) > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.order.Back()
		entry := oldest.Value.(*setsGzipEntry)
		c.order.Remove(oldest)
		delete(c.entries, entry.key)
		c.bytes -= len(entry.zipped)
	}
}

// acceptsGzip reads an Accept-Encoding value (RFC 9110 §12.5.3): gzip, or
// "*", listed without q=0.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "x-gzip" && name != "*" {
			continue
		}
		q := strings.ReplaceAll(strings.ToLower(params), " ", "")
		if q == "q=0" || strings.HasPrefix(q, "q=0.") && strings.Trim(q[4:], "0") == "" {
			continue
		}
		return true
	}
	return false
}
