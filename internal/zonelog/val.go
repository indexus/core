package zonelog

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"sync"
)

// A payload file NNN.val holds the payloads a segment wrote, as frames of
// up to valFrame bytes, then the frames' index and a footer:
//
//	frames  each the flate (fastest level) of its bytes, or the bytes
//	        themselves when flate does not make them smaller
//	index   per frame: u64 offset in the file | u32 stored bytes | u32 bytes
//	footer  u64 index offset | u32 frames | u32 CRC-32C of the index | "ZLV1"
//
// A row's voff and vlen count payload bytes as written, before compression:
// a read decompresses the frames its bytes are in, and keeps them in the
// page cache.
const valFrame = 64 << 10

const valFooterLen = 20

type valRef struct {
	off          uint64
	stored, size uint32
}

// valWriter compresses frames off the writer's goroutine, at most
// valWorkers at a time and valAhead in flight, and writes them in order.
type valWriter struct {
	bw      *bufio.Writer
	buf     []byte
	off     uint64
	frames  []valRef
	pending []chan valOut
	slots   chan struct{}
	skip    int // frames to store as they are: recent ones did not shrink
}

type valOut struct{ stored, raw []byte }

const (
	valWorkers = 2
	valAhead   = 4
	// valSkip frames are stored without trying flate after one that
	// did not shrink by 1/16: incompressible payloads cost a copy.
	valSkip = 16
)

var flaters = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(io.Discard, flate.BestSpeed)
	return w
}}

func newValWriter(f File) *valWriter {
	return &valWriter{bw: bufio.NewWriterSize(f, 1<<20), slots: make(chan struct{}, valWorkers)}
}

func (v *valWriter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		k := min(len(b), valFrame-len(v.buf))
		v.buf, b = append(v.buf, b[:k]...), b[k:]
		if len(v.buf) == valFrame {
			if err := v.frame(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// frame hands the full buffer to a worker and writes the oldest frames
// once more than valAhead are in flight.
func (v *valWriter) frame() error {
	raw, done := v.buf, make(chan valOut, 1)
	v.buf = make([]byte, 0, valFrame)
	v.pending = append(v.pending, done)
	if v.skip > 0 {
		v.skip--
		done <- valOut{raw, raw}
		return v.drain(valAhead)
	}
	v.slots <- struct{}{}
	go func() {
		defer func() { <-v.slots }()
		var z bytes.Buffer
		zw := flaters.Get().(*flate.Writer)
		zw.Reset(&z)
		zw.Write(raw) // a bytes.Buffer does not fail
		zw.Close()
		zw.Reset(io.Discard)
		flaters.Put(zw)
		stored := z.Bytes()
		if len(stored) >= len(raw) {
			stored = raw
		}
		done <- valOut{stored, raw}
	}()
	return v.drain(valAhead)
}

// drain writes frames in order until at most keep are in flight.
func (v *valWriter) drain(keep int) error {
	for len(v.pending) > keep {
		out := <-v.pending[0]
		v.pending = v.pending[1:]
		if len(out.stored) > len(out.raw)-len(out.raw)/16 && v.skip == 0 {
			v.skip = valSkip
		}
		v.frames = append(v.frames, valRef{v.off, uint32(len(out.stored)), uint32(len(out.raw))})
		v.off += uint64(len(out.stored))
		if _, err := v.bw.Write(out.stored); err != nil {
			return err
		}
	}
	return nil
}

// Flush ends the file: the last frame, the index and the footer.
func (v *valWriter) Flush() error {
	if len(v.buf) > 0 {
		if err := v.frame(); err != nil {
			return err
		}
	}
	if err := v.drain(0); err != nil {
		return err
	}
	var index []byte
	for _, f := range v.frames {
		index = binary.LittleEndian.AppendUint64(index, f.off)
		index = binary.LittleEndian.AppendUint32(index, f.stored)
		index = binary.LittleEndian.AppendUint32(index, f.size)
	}
	foot := binary.LittleEndian.AppendUint64(nil, v.off)
	foot = binary.LittleEndian.AppendUint32(foot, uint32(len(v.frames)))
	foot = binary.LittleEndian.AppendUint32(foot, crc32.Checksum(index, crcTable))
	v.bw.Write(index)
	v.bw.Write(append(foot, "ZLV1"...))
	return v.bw.Flush()
}

// valFile is an open payload file: its frames, where each starts in the
// payload bytes (starts), and size, the payload bytes it holds.
type valFile struct {
	File
	id     uint64
	frames []valRef
	starts []uint64
	size   int64
}

func openVal(f File, id uint64) (valFile, error) {
	v := valFile{File: f, id: id}
	end, err := f.Size()
	if err != nil {
		return v, err
	}
	foot := make([]byte, valFooterLen)
	if _, err := f.ReadAt(foot, max(end-valFooterLen, 0)); err != nil || string(foot[16:]) != "ZLV1" {
		return v, fmt.Errorf("zonelog: payload file %d: bad footer", id)
	}
	at, n := binary.LittleEndian.Uint64(foot), int(binary.LittleEndian.Uint32(foot[8:]))
	index := make([]byte, 16*n)
	if int64(at)+int64(len(index)) != end-valFooterLen {
		return v, fmt.Errorf("zonelog: payload file %d: bad index", id)
	}
	if _, err := f.ReadAt(index, int64(at)); err != nil || crc32.Checksum(index, crcTable) != binary.LittleEndian.Uint32(foot[12:]) {
		return v, fmt.Errorf("zonelog: payload file %d: damaged index", id)
	}
	for i := range n {
		r := index[16*i:]
		ref := valRef{binary.LittleEndian.Uint64(r), binary.LittleEndian.Uint32(r[8:]), binary.LittleEndian.Uint32(r[12:])}
		v.frames, v.starts = append(v.frames, ref), append(v.starts, uint64(v.size))
		v.size += int64(ref.size)
	}
	return v, nil
}

// read copies payload bytes [off, off+len(out)) into out.
func (v valFile) read(c *cache, out []byte, off uint64) error {
	i := sort.Search(len(v.starts), func(i int) bool { return v.starts[i] > off }) - 1
	for len(out) > 0 {
		if i < 0 || i >= len(v.frames) {
			return fmt.Errorf("zonelog: payload file %d: no bytes at %d", v.id, off)
		}
		frame, err := c.get([2]uint64{v.id | 1<<63, uint64(i)}, func() (any, int, error) {
			b, err := v.frame(i)
			return b, len(b), err
		})
		if err != nil {
			return err
		}
		b := frame.([]byte)[off-v.starts[i]:]
		k := copy(out, b)
		out, off, i = out[k:], off+uint64(k), i+1
	}
	return nil
}

func (v valFile) frame(i int) ([]byte, error) {
	f := v.frames[i]
	stored := make([]byte, f.stored)
	if _, err := v.ReadAt(stored, int64(f.off)); err != nil {
		return nil, err
	}
	if f.stored == f.size {
		return stored, nil
	}
	out := make([]byte, f.size)
	if _, err := io.ReadFull(flate.NewReader(bytes.NewReader(stored)), out); err != nil {
		return nil, fmt.Errorf("zonelog: payload file %d: frame %d: %v", v.id, i, err)
	}
	return out, nil
}
