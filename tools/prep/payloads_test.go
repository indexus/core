package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
)

// The vector shared with experiences (test/point-cloud/fixtures/binary-payloads.json):
// the JSON payloads of three records and the binary payloads they become.
func TestBinaryPayloadsMatchTheSharedVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/binary-payloads.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Collection string
		ID         string
		JSON       json.RawMessage
		Binary     []byte
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("%d cases", len(cases))
	}
	for _, c := range cases {
		if !binaryCollections[c.Collection] {
			t.Errorf("%s is not written binary", c.Collection)
		}
		got, err := binaryPayload(c.JSON)
		if err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		if !bytes.Equal(got, c.Binary) {
			t.Errorf("%s %s: %d bytes, want the %d of the vector", c.Collection, c.ID, len(got), len(c.Binary))
		}
	}
}

func TestBinaryLayoutsKeepEveryArrayOn4Bytes(t *testing.T) {
	raw, _ := os.ReadFile("testdata/binary-payloads.json")
	var cases []struct{ Binary []byte }
	json.Unmarshal(raw, &cases)
	le := binary.LittleEndian
	near, fine, tile := cases[0].Binary, cases[1].Binary, cases[2].Binary
	if string(near[:4]) != "IXBM" || string(fine[:4]) != "IXBF" || string(tile[:4]) != "IXTT" {
		t.Fatal("magics")
	}
	// near: header, levels, colours, then two meshes whose arrays start on 4 bytes
	levels, colours := int(le.Uint32(near[44:])), int(near[7])
	at := 48 + 16*levels + 4*colours
	for k := 0; k < int(near[6]); k++ {
		v, tri := int(le.Uint32(near[48+16*k:])), int(le.Uint32(near[52+16*k:]))
		for _, n := range []int{6 * v, v, 6 * tri} {
			if at%4 != 0 {
				t.Fatalf("mesh %d: an array at %d", k, at)
			}
			at += (n + 3) &^ 3
		}
	}
	if extra := int(le.Uint32(near[40:])); at+extra != len(near) {
		t.Fatalf("near record: %d bytes, layout says %d", len(near), at+extra)
	}
	if size := int(le.Uint16(tile[6:])); len(tile) != 40+2*size*size {
		t.Fatalf("tile: %d bytes for size %d", len(tile), size)
	}
}

func TestUnknownPayloadsStayJSON(t *testing.T) {
	if _, err := binaryPayload(json.RawMessage(`{"format":"something-else"}`)); err == nil {
		t.Fatal("an unknown format was rewritten")
	}
}
