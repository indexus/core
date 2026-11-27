package p2p

import (
	"compress/gzip"
	"io"
	"sync"
)

// gzipPool keeps gzip writers of one level between requests: a new writer
// allocates about 1 MB of flate state, a Reset none.
type gzipPool struct {
	level int
	pool  sync.Pool
}

func (p *gzipPool) get(w io.Writer) *gzip.Writer {
	if zw, ok := p.pool.Get().(*gzip.Writer); ok {
		zw.Reset(w)
		return zw
	}
	zw, _ := gzip.NewWriterLevel(w, p.level)
	return zw
}

// put takes back a writer, closed or not; it no longer holds its target.
func (p *gzipPool) put(zw *gzip.Writer) {
	zw.Reset(io.Discard)
	p.pool.Put(zw)
}

var (
	// Records and content: payloads are most of the answer, fastest level.
	fastGzip = &gzipPool{level: gzip.BestSpeed}
	// /sets: see setsGzipLevel.
	setsGzip = &gzipPool{level: setsGzipLevel}
)

var gzipReaders sync.Pool

// getGzipReader is gzip.NewReader from a pooled reader.
func getGzipReader(r io.Reader) (*gzip.Reader, error) {
	if z, ok := gzipReaders.Get().(*gzip.Reader); ok {
		if err := z.Reset(r); err != nil {
			gzipReaders.Put(z)
			return nil, err
		}
		return z, nil
	}
	return gzip.NewReader(r)
}

func putGzipReader(z *gzip.Reader) {
	z.Close()
	gzipReaders.Put(z)
}
