package zonelog

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"
	"time"
)

// The log is a sequence of files of frames. A frame is one atomic batch:
//
//	u32 body bytes | u32 CRC-32C of the body | body
//	body: u64 first seq | u32 seqs | u32 count | count × (u64 request hash |
//	      u16 key bytes | key | IXR1 value)
//
// seqs is count, except for the frame of a bulk segment, which uses one seq
// and carries no mutation. Frames follow each other without a hole in the seqs.
type logFile struct {
	name        string
	first, last uint64 // seqs; last is 0 while the file is empty
	size        int64
}

func appendFrame(b []byte, first uint64, seqs uint32, rows []*row) []byte {
	start := len(b)
	b = append(b, make([]byte, 8)...)
	b = binary.LittleEndian.AppendUint64(b, first)
	b = binary.LittleEndian.AppendUint32(b, seqs)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(rows)))
	for _, r := range rows {
		b = binary.LittleEndian.AppendUint64(b, r.req)
		b = binary.LittleEndian.AppendUint16(b, uint16(len(r.key)))
		b = appendValue(append(b, r.key...), r)
	}
	binary.LittleEndian.PutUint32(b[start:], uint32(len(b)-start-8))
	binary.LittleEndian.PutUint32(b[start+4:], crc32.Checksum(b[start+8:], crcTable))
	return b
}

// readFrames calls visit with the rows of every whole frame of raw, in
// order, and returns the bytes of whole frames. A frame that is short or
// fails its CRC ends the file: it was never acknowledged.
func readFrames(raw []byte, visit func(first uint64, seqs uint32, rows []*row) error) (good int, err error) {
	for len(raw)-good >= 8 {
		b := raw[good:]
		n := int(binary.LittleEndian.Uint32(b))
		if n < 16 || len(b) < 8+n || crc32.Checksum(b[8:8+n], crcTable) != binary.LittleEndian.Uint32(b[4:]) {
			break
		}
		body := b[8 : 8+n]
		first, seqs := binary.LittleEndian.Uint64(body), binary.LittleEndian.Uint32(body[8:])
		rows := make([]*row, binary.LittleEndian.Uint32(body[12:]))
		body = body[16:]
		for i := range rows {
			k := int(binary.LittleEndian.Uint16(body[8:]))
			r := &row{req: binary.LittleEndian.Uint64(body), key: bytes.Clone(body[10 : 10+k]), seq: first + uint64(i)}
			used := readValue(body[10+k:], r)
			if used == 0 {
				return good, fmt.Errorf("zonelog: damaged frame body")
			}
			rows[i], body = r, body[10+k+used:]
		}
		if err := visit(first, seqs, rows); err != nil {
			return good, err
		}
		good += 8 + n
	}
	return good, nil
}

// recover replays the log. Frames after the durable seq rebuild the
// memtable. A file may end in a torn frame; the next file then starts at the
// following seq. Any other hole above the durable seq is lost data, and the
// store refuses to open.
func (s *Store) recover(names []string) error {
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		raw, err := readAll(s.fs, name)
		if err != nil {
			return err
		}
		lf := logFile{name: name}
		fmt.Sscanf(name, "%d", &lf.first)
		good, err := readFrames(raw, func(first uint64, seqs uint32, rows []*row) error {
			if first > s.seq+1 && first-1 > s.durable {
				return fmt.Errorf("zonelog: the log has a hole before seq %d", first)
			}
			lf.last = first + uint64(seqs) - 1
			s.seq = max(s.seq, lf.last)
			for _, r := range rows {
				if r.seq <= s.durable {
					continue
				}
				prev, err := s.lookup(r.key, ^uint64(0))
				if err != nil {
					return err
				}
				s.place(r, prev)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if lf.size = int64(good); good == 0 {
			s.fs.Remove(name) // nothing whole in it; its name may be needed again
			continue
		}
		s.logs = append(s.logs, lf)
	}
	s.synced, s.visible = s.seq, s.seq
	return s.openLog(s.seq + 1)
}

// openLog starts a new log file named by the first seq it will hold. The
// file must be in the directory durably before a frame in it is acknowledged.
func (s *Store) openLog(first uint64) (err error) {
	if s.log != nil {
		s.log.Close()
	}
	lf := logFile{name: fmt.Sprintf("%020d.log", first), first: first}
	if s.log, err = s.fs.Create(lf.name); err == nil && !s.opt.NoSync {
		err = s.fs.Sync()
	}
	s.logs = append(s.logs, lf)
	return err
}

// sync makes the log durable through seq upto, and only then lets readers
// see those rows. Whoever holds syncMu writes everything pending, so callers
// that waited find their seq already synced: one fsync per group.
func (s *Store) sync(upto uint64) error {
	started := time.Now()
	s.syncMu.Lock()
	s.timing.syncLock.Observe(time.Since(started))
	defer s.syncMu.Unlock()
	s.mu.Lock()
	buf, last, durable, err := s.pending, s.seq, s.durable, s.err
	if s.synced < upto {
		s.pending = nil
	}
	s.mu.Unlock()
	if err != nil || s.synced >= upto || len(buf) == 0 {
		return err
	}
	at := &s.logs[len(s.logs)-1]
	if _, err = s.log.Write(buf); err == nil && !s.opt.NoSync {
		err = s.log.Sync()
	}
	at.size, at.last = at.size+int64(len(buf)), last
	if err == nil && at.size >= min(16<<20, s.opt.LogRetain/4) {
		// Start a new file, then drop the oldest ones that a segment covers,
		// keeping LogRetain bytes of them.
		total := int64(0)
		for _, lf := range s.logs {
			total += lf.size
		}
		for len(s.logs) > 1 && s.logs[0].last <= durable && total-s.logs[0].size >= s.opt.LogRetain {
			total -= s.logs[0].size
			s.fs.Remove(s.logs[0].name)
			s.logs = s.logs[1:]
		}
		err = s.openLog(last + 1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.err = err
		return err
	}
	s.synced, s.visible = last, last
	return nil
}
