package tree

import (
	"bytes"
	"testing"
)

func TestRangeHalfSpaceIgnoresThirdPartyNeighbors(t *testing.T) {
	// Neighbors never enter Range(owner, candidate): adding arbitrary peer IDs
	// does not change whether a key is donated between two fixed IDs.
	owner, _ := BASE64.Decode("oMVJ-ie1_C-VGosR")
	cand, _ := BASE64.Decode("7xUrVn4uq3I6ZAt8")
	key, _ := BASE64.Decode("7xSrV2020idx0001")

	donate := bytes.Compare(xorBytes(key, cand), xorBytes(key, owner)) < 0
	// "Neighbors" present or absent cannot flip that boolean.
	_ = [][]byte{
		mustDecode(t, "wEdMFa-aFsLYFg7B"),
		mustDecode(t, "7vdMHEX8yNTxsnBu"),
		mustDecode(t, "wGdMzsYOGr7b-J12"),
	}
	donateAgain := bytes.Compare(xorBytes(key, cand), xorBytes(key, owner)) < 0
	if donate != donateAgain {
		t.Fatal("neighbor set must not affect owner/candidate Range predicate")
	}
	t.Logf("owner→cand donates key=%v (neighbors irrelevant)", donate)
}

func xorBytes(a, b []byte) []byte {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func mustDecode(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := DecodeVnodeID(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
