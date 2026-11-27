package convergent

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
)

// NewVersion builds a version and its digest.
func NewVersion(id string, ver uint64, deleted bool, value int64, dot Dot) Version {
	if deleted {
		value = 0
	}
	return Version{ID: id, Ver: ver, Deleted: deleted, Value: value, Digest: contentDigest(id, deleted, value), Dot: dot}
}

func contentDigest(id string, deleted bool, value int64) [32]byte {
	h := sha256.New()
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(id))))
	h.Write([]byte(id))
	if deleted {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(value)))
	return [32]byte(h.Sum(nil))
}

// Receipt remembers the result of a request ID so a retry on any replica
// returns it. Command is the digest of the request's content.
type Receipt struct {
	RequestID string
	Command   [32]byte
	Result    Version
}

// receiptWins orders two receipts of one request ID: the smaller command
// digest, then the winning result. Two different commands under one ID can
// only meet after writers that could not see each other.
func receiptWins(a, b Receipt) bool {
	if c := bytes.Compare(a.Command[:], b.Command[:]); c != 0 {
		return c < 0
	}
	return modelWins(a.Result, b.Result)
}

// modelWins exercises the runtime comparator in every positive scenario. Its
// two deliberately broken variants are negative controls of the historical
// model; fault switches never enter the production comparator.
func modelWins(a, b Version) bool {
	if a.Ver == b.Ver {
		if injected == NoDigestRule {
			if a.Deleted != b.Deleted {
				return a.Deleted
			}
			return false
		}
		if injected == NoDeleteWins {
			a.Deleted, b.Deleted = false, false
		}
	}
	return Wins(a, b)
}
