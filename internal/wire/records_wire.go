package wire

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// RecordsContentType is /records?format=binary: IXRB, the records of a read
// with their payloads as bytes (docs/protocol/wire-block.md, IXRB). A binary
// payload goes as its bytes, not as the base64 of a JSON string.
const RecordsContentType = "application/vnd.indexus.records"

const (
	recordsMagic   = "IXRB"
	recordsVersion = 2 // 2: each payload starts at a multiple of 4
	// recordsWide is version 2 with the metric count of every record on u16:
	// written only when a record has more than 255 metrics (an embedding),
	// so the bytes of every other read do not change.
	recordsWide = 3
	// MaxRecordMetrics is the widest record IXRB carries.
	MaxRecordMetrics = math.MaxUint16
)

const (
	recordBinary = 1 << iota // the payload is raw bytes, else JSON text
)

// EncodeRecords writes items as IXRB, version 2 (or 3):
//
//	"IXRB" | u8 version 2 | 3 × u8 0 | u32 count | count × record
//	record: u16 len, location | u16 len, id | u8 flags | f64 weight |
//	        u8 n, n × f64 metrics | u32 len | 0 to 3 zero bytes | payload
//
// Version 3 counts the metrics of every record on u16 instead of u8; it is
// used when a record has more than 255 metrics.
//
// little endian. The zero bytes put every payload at a body offset that is a
// multiple of 4, so a client can view it as 32-bit words where it lies. It
// streams: nothing but one record is held.
func EncodeRecords(w io.Writer, items []Item) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	version := byte(recordsVersion)
	for _, it := range items {
		if len(it.Metrics) > math.MaxUint8 {
			version = recordsWide
			break
		}
	}
	var b []byte
	b = append(b, recordsMagic...)
	b = append(b, version, 0, 0, 0)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(items)))
	off := 0           // body bytes before b
	var scratch []byte // the decoded binary payload, reused
	for _, it := range items {
		payload := []byte(it.Payload)
		flags := byte(0)
		if it.Binary {
			var err error
			if payload, scratch, err = binaryPayload(it.Payload, scratch); err != nil {
				return err
			}
			flags |= recordBinary
		}
		if len(it.Location) > math.MaxUint16 || len(it.Id) > math.MaxUint16 || len(it.Metrics) > MaxRecordMetrics {
			return errors.New("records: a record too large for IXRB")
		}
		b = binary.LittleEndian.AppendUint16(b, uint16(len(it.Location)))
		b = append(b, it.Location...)
		b = binary.LittleEndian.AppendUint16(b, uint16(len(it.Id)))
		b = append(b, it.Id...)
		b = append(b, flags)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(it.Weight))
		if version == recordsWide {
			b = binary.LittleEndian.AppendUint16(b, uint16(len(it.Metrics)))
		} else {
			b = append(b, byte(len(it.Metrics)))
		}
		for _, v := range it.Metrics {
			b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
		}
		b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
		for (off+len(b))%4 != 0 {
			b = append(b, 0)
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
		if _, err := bw.Write(payload); err != nil {
			return err
		}
		off += len(b) + len(payload)
		b = b[:0]
	}
	if len(b) > 0 {
		if _, err := bw.Write(b); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// binaryPayload returns the bytes of a binary payload, the JSON string of
// their base64 as storage keeps it. A plain quoted string is decoded into
// buf as json.Unmarshal into []byte would (the same base64.StdEncoding), so
// without its reflection and copies; anything else (escapes, null, an
// error) goes through json.Unmarshal itself.
func binaryPayload(p json.RawMessage, buf []byte) ([]byte, []byte, error) {
	if n := len(p); n >= 2 && p[0] == '"' && p[n-1] == '"' && isBase64Text(p[1:n-1]) {
		s := p[1 : n-1]
		if size := base64.StdEncoding.DecodedLen(len(s)); cap(buf) < size {
			buf = make([]byte, size)
		}
		if m, err := base64.StdEncoding.Decode(buf[:cap(buf)], s); err == nil {
			return buf[:m], buf, nil
		}
	}
	var payload []byte = p
	err := json.Unmarshal(p, &payload)
	return payload, buf, err
}

// isBase64Text: only the letters of base64.StdEncoding and its padding, so
// the string has no JSON escape and nothing base64 would skip (\r, \n).
func isBase64Text(s []byte) bool {
	for _, c := range s {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

// DecodeRecords reads an IXRB body. A binary payload comes back as its bytes
// in Payload, with Binary set: not JSON.
func DecodeRecords(collection string, buf []byte) ([]Item, error) {
	short := errors.New("records: truncated IXRB")
	if len(buf) < 12 || string(buf[:4]) != recordsMagic {
		return nil, errors.New("records: not IXRB")
	}
	if buf[4] != recordsVersion && buf[4] != recordsWide {
		return nil, fmt.Errorf("records: IXRB version %d, not %d or %d", buf[4], recordsVersion, recordsWide)
	}
	wide := buf[4] == recordsWide
	n := int(binary.LittleEndian.Uint32(buf[8:]))
	at := 12
	take := func(k int) ([]byte, error) {
		if k < 0 || len(buf)-at < k {
			return nil, short
		}
		at += k
		return buf[at-k : at], nil
	}
	str := func() (string, error) {
		l, err := take(2)
		if err != nil {
			return "", err
		}
		s, err := take(int(binary.LittleEndian.Uint16(l)))
		return string(s), err
	}
	out := make([]Item, 0, min(n, len(buf)/16))
	for range n {
		it := Item{Collection: collection}
		var err error
		if it.Location, err = str(); err != nil {
			return nil, err
		}
		if it.Id, err = str(); err != nil {
			return nil, err
		}
		head, err := take(9)
		if err != nil {
			return nil, err
		}
		it.Binary = head[0]&recordBinary != 0
		it.Weight = math.Float64frombits(binary.LittleEndian.Uint64(head[1:]))
		countBytes := 1
		if wide {
			countBytes = 2
		}
		cnt, err := take(countBytes)
		if err != nil {
			return nil, err
		}
		m := int(cnt[0])
		if wide {
			m = int(binary.LittleEndian.Uint16(cnt))
		}
		metrics, err := take(8 * m)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(metrics); i += 8 {
			it.Metrics = append(it.Metrics, math.Float64frombits(binary.LittleEndian.Uint64(metrics[i:])))
		}
		l, err := take(4)
		if err != nil {
			return nil, err
		}
		pad, err := take((4 - at%4) % 4)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(pad, make([]byte, len(pad))) {
			return nil, errors.New("records: IXRB padding is not zero")
		}
		if it.Payload, err = take(int(binary.LittleEndian.Uint32(l))); err != nil {
			return nil, err
		}
		if len(it.Payload) == 0 {
			it.Payload = nil
		}
		out = append(out, it)
	}
	if at != len(buf) {
		return nil, errors.New("records: bytes after the last IXRB record")
	}
	return out, nil
}
