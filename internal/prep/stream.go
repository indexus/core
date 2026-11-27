package prep

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// ReadPart reads a part from a stream, once, front to back: a node takes a
// batch as it arrives. It checks what Open and Each check (header, block
// CRCs, key order, index, footer) and the part's size and SHA-256 against
// info. visit sees each row as soon as its block's columns are read; when
// the part turns out bad, ReadPart returns an error after having shown rows
// of it, so a caller installs nothing before ReadPart returned nil. A row's
// slices are valid until visit returns. Payloads are read one at a time: a
// block of large payloads is never held whole.
func ReadPart(r io.Reader, info PartInfo, m int, visit func(Row) error) error {
	sum := sha256.New()
	in := &counter{r: bufio.NewReaderSize(io.TeeReader(r, sum), 1<<20)}
	h := make([]byte, headerLen)
	if _, err := io.ReadFull(in, h); err != nil {
		return err
	}
	if string(h[:4]) != magic || binary.LittleEndian.Uint16(h[4:]) != version {
		return errors.New("prep: not an IXP1 part")
	}
	if int(binary.LittleEndian.Uint16(h[6:])) != m {
		return fmt.Errorf("prep: part of %d metrics, the batch has %d", binary.LittleEndian.Uint16(h[6:]), m)
	}
	payloads := h[8]&1 != 0
	var index bytes.Buffer
	var rows uint64
	var blocks uint32
	var prev Head
	var prevID []byte
	for rows < info.Records {
		start := in.n
		n, size, err := readBlock(in, m, payloads, func(r Row) error {
			if rows > 0 && compareKey(prev, prevID, r.Head, r.ID) >= 0 {
				return fmt.Errorf("prep: %s/%s out of order", r.Head.Location(), r.ID)
			}
			prev, prevID = r.Head, append(prevID[:0], r.ID...)
			rows++
			return visit(r)
		})
		if err != nil {
			return err
		}
		blocks++
		ix := make([]byte, indexLen) // the head part is left to the block CRCs
		binary.LittleEndian.PutUint64(ix[16:], uint64(start))
		binary.LittleEndian.PutUint32(ix[24:], uint32(size))
		binary.LittleEndian.PutUint32(ix[28:], uint32(n))
		index.Write(ix)
	}
	at := in.n
	rest, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if len(rest) != int(blocks)*indexLen+footerLen || rows != info.Records {
		return errors.New("prep: part does not end after its rows")
	}
	ix, foot := rest[:len(rest)-footerLen], rest[len(rest)-footerLen:]
	if binary.LittleEndian.Uint64(foot) != uint64(at) || binary.LittleEndian.Uint64(foot[8:]) != rows || binary.LittleEndian.Uint32(foot[16:]) != blocks ||
		binary.LittleEndian.Uint32(foot[20:]) != crc32.ChecksumIEEE(ix) || string(foot[24:28]) != magic {
		return errors.New("prep: bad footer")
	}
	// The index names the blocks just read: offsets, sizes, rows.
	for i := range int(blocks) {
		if !bytes.Equal(ix[i*indexLen+16:(i+1)*indexLen], index.Bytes()[i*indexLen+16:(i+1)*indexLen]) {
			return errors.New("prep: index differs from the blocks")
		}
	}
	if uint64(in.n) != info.Bytes || fmt.Sprintf("%x", sum.Sum(nil)) != info.SHA256 {
		return fmt.Errorf("prep: part sha256 or size differs from the manifest")
	}
	return nil
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readBlock reads one block: its columns at once, then its payloads row by
// row, with the CRC kept as the bytes go by. It returns the rows and the
// block's size.
func readBlock(in io.Reader, m int, payloads bool, visit func(Row) error) (int, int, error) {
	crc := uint32(0)
	read := func(b []byte) error {
		if _, err := io.ReadFull(in, b); err != nil {
			return err
		}
		crc = crc32.Update(crc, crc32.IEEETable, b)
		return nil
	}
	head := make([]byte, 16)
	if err := read(head); err != nil {
		return 0, 0, err
	}
	n, idBytes, payBytes := int(binary.LittleEndian.Uint32(head)), int(binary.LittleEndian.Uint32(head[4:])), int64(binary.LittleEndian.Uint64(head[8:]))
	if n == 0 || n > maxRows || idBytes > n*255 || payBytes < 0 || !payloads && payBytes > 0 {
		return 0, 0, errors.New("prep: bad block header")
	}
	pad := func(k int) int { return k + (8-k%8)%8 }
	columns := pad(n*headLen) + pad(4*n) + 8*n*(1+m)
	if payloads {
		columns += 8 * n
	}
	body := make([]byte, columns+pad(idBytes))
	if err := read(body); err != nil {
		return 0, 0, err
	}
	heads, idEnd := body[:n*headLen], body[pad(n*headLen):]
	at := pad(n*headLen) + pad(4*n)
	var payEnd []byte
	if payloads {
		payEnd, at = body[at:at+8*n], at+8*n
	}
	nums, ids := body[at:at+8*n*(1+m)], body[columns:columns+idBytes]
	var row Row
	row.Metrics = make([]float64, m)
	var buf []byte
	idAt, payAt := 0, int64(0)
	for i := range n {
		copy(row.Head[:], heads[i*headLen:])
		e := int(binary.LittleEndian.Uint32(idEnd[4*i:]))
		if e < idAt || e > idBytes {
			return 0, 0, errors.New("prep: id offsets")
		}
		row.ID, idAt = ids[idAt:e], e
		row.Payload = nil
		if payloads {
			e := int64(binary.LittleEndian.Uint64(payEnd[8*i:]))
			if e < payAt || e > payBytes {
				return 0, 0, errors.New("prep: payload offsets")
			}
			if int64(cap(buf)) < e-payAt {
				buf = make([]byte, e-payAt)
			}
			row.Payload = buf[:e-payAt]
			if err := read(row.Payload); err != nil {
				return 0, 0, err
			}
			payAt = e
		}
		row.Weight = math.Float64frombits(binary.LittleEndian.Uint64(nums[8*i:]))
		for j := range m {
			row.Metrics[j] = math.Float64frombits(binary.LittleEndian.Uint64(nums[8*(n*(1+j)+i):]))
		}
		if err := visit(row); err != nil {
			return 0, 0, err
		}
	}
	if payAt != payBytes || idAt != idBytes {
		return 0, 0, errors.New("prep: block offsets do not end the block")
	}
	tail := make([]byte, pad(int(payBytes))-int(payBytes))
	if err := read(tail); err != nil {
		return 0, 0, err
	}
	end := make([]byte, 8)
	if _, err := io.ReadFull(in, end); err != nil {
		return 0, 0, err
	}
	if binary.LittleEndian.Uint32(end) != crc {
		return 0, 0, errors.New("prep: block CRC")
	}
	size := 16 + len(body) + int(payBytes) + len(tail) + 8
	return n, size, nil
}
