package wire

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

// BenchmarkEncodeRecords: /records?format=binary for 1 000 records whose
// payloads are binary (1 KiB, as storage hands them: a base64 JSON string)
// or JSON text.
func BenchmarkEncodeRecords(b *testing.B) {
	for _, binary := range []bool{true, false} {
		items := make([]Item, 1000)
		raw := make([]byte, 1024)
		for i := range raw {
			raw[i] = byte(i * 13)
		}
		for i := range items {
			items[i] = Item{Collection: "c", Location: "7Bxy", Id: fmt.Sprintf("id-%04d", i), Metrics: []float64{1, 2, 3}}
			if binary {
				items[i].Payload, _ = json.Marshal(base64.StdEncoding.EncodeToString(raw))
				items[i].Binary = true
			} else {
				items[i].Payload = json.RawMessage(`{"name":"a building","levels":[1,2,3],"height":12.5}`)
			}
		}
		b.Run(fmt.Sprintf("binary=%v", binary), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := EncodeRecords(io.Discard, items); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// binaryPayload answers as json.Unmarshal into []byte for every shape a
// payload can take, including the ones its fast path hands back to it.
func TestBinaryPayloadMatchesJSON(t *testing.T) {
	raw := []byte{0, 1, 2, 250, 251, 252, 253, 254, 255}
	cases := []string{`""`, `"AA=="`, `"AAE="`, `"AAEC"`, `null`, `"A"`, `"AA"`, `"AA=A"`, `"!!!!"`, `"AA=="`, `"AA\n=="`, "\"AA\n==\"", "\"AA\r\n==\"", `"AA\/A"`, `12`, `{}`, `"a b"`, `"`, `"\"`}
	for _, n := range []int{0, 1, 2, 3, 4, 100, 1023} {
		cases = append(cases, `"`+base64.StdEncoding.EncodeToString(append(raw, make([]byte, n)...))+`"`)
	}
	scratch := make([]byte, 3)
	for _, c := range cases {
		want := []byte(c)
		wantErr := json.Unmarshal([]byte(c), &want)
		got, buf, err := binaryPayload(json.RawMessage(c), scratch)
		scratch = buf
		if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) || string(got) != string(want) || (got == nil) != (want == nil) {
			t.Errorf("%q: got %x %v, want %x %v", c, got, err, want, wantErr)
		}
	}
}
