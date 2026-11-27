package main

// Binary payloads of the scene's meshes and ground tiles: the JSON payloads of
// today's builders (a few fields, the arrays gzipped in base64) rewritten as a
// small fixed header followed by the arrays themselves, each starting on a
// 4-byte boundary, so that a reader takes typed-array views on them with no
// decode step. Layouts (little endian), mirrored by the readers in
// experiences (src/experiences/point-cloud/buildingsTree.js, terrainTiles.js,
// docs/collections.md "Binary payloads"):
//
//	IXBM  a building's near record (indexus-building-v3)
//	  0 "IXBM" | 4 u16 version 1 | 6 u8 meshes | 7 u8 colours
//	  8 f64 lat | 16 f64 lng | 24 f64 altitude | 32 f64 step
//	  40 u32 bytes of the JSON tail | 44 u32 levels
//	  48 levels × (u32 vertices, u32 triangles, f64 error)
//	  then colours × (u8 r, g, b, 0)
//	  then meshes (levels 0 .. meshes-1) × mesh
//	  then the JSON tail: the payload's other fields (source, outline, footprint, ...)
//	IXBF  a building's fine record (indexus-building-fine-v1)
//	  0 "IXBF" | 4 u16 version 1 | 6 u16 0 | 8 u32 vertices | 12 u32 triangles | 16 mesh
//	mesh: i16 x, y, z per vertex | u8 part per vertex | u16 3 indices per triangle,
//	  each array padded to 4 bytes
//	IXTT  a ground tile (indexus-terrain-tile-v2)
//	  0 "IXTT" | 4 u16 version 1 | 6 u16 size | 8 i32 level | 12 i32 row | 16 i32 col
//	  20 u32 flags (bit 0: values are deltas along each row) | 24 f64 scale | 32 f64 offset
//	  40 u16 values, size × size, row-major from the south-west corner

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// binaryCollections are written with binary payloads (payloadEncoding "binary").
var binaryCollections = map[string]bool{
	"BuildLidHD2025v2": true,
	"BuildLidHD2025f2": true,
	"TerraLidHD2025v1": true,
}

const payloadVersion = 1

// binaryPayload rewrites a JSON payload of a known format in its binary layout.
func binaryPayload(raw json.RawMessage) ([]byte, error) {
	var head struct{ Format string }
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	switch head.Format {
	case "indexus-building-v3":
		return buildingPayload(raw)
	case "indexus-building-fine-v1":
		return finePayload(raw)
	case "indexus-terrain-tile-v2":
		return tilePayload(raw)
	}
	return nil, fmt.Errorf("no binary layout for payload format %q", head.Format)
}

type meshLevel struct {
	Vertices  int     `json:"vertices"`
	Triangles int     `json:"triangles"`
	Error     float64 `json:"error"`
}

func gunzipBase64(text string) ([]byte, error) {
	packed, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// appendMeshes copies the meshes of body (each: xyz, parts, indices, unpadded)
// into out with every array on a 4-byte boundary.
func appendMeshes(out, body []byte, levels []meshLevel) ([]byte, error) {
	at := 0
	for _, l := range levels {
		sizes := []int{6 * l.Vertices, l.Vertices, 6 * l.Triangles}
		for _, n := range sizes {
			if at+n > len(body) {
				return nil, fmt.Errorf("mesh data ends early")
			}
			out = pad4(append(out, body[at:at+n]...))
			at += n
		}
	}
	if at != len(body) {
		return nil, fmt.Errorf("mesh data: %d bytes left over", len(body)-at)
	}
	return out, nil
}

func buildingPayload(raw json.RawMessage) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	var p struct {
		Origin []float64   `json:"origin"`
		Step   float64     `json:"step"`
		Colors [][]int     `json:"colors"`
		Levels []meshLevel `json:"levels"`
		Data   string      `json:"data"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Origin) != 3 || len(p.Levels) != 3 || len(p.Colors) > 255 {
		return nil, fmt.Errorf("building payload: origin %d, levels %d, colours %d", len(p.Origin), len(p.Levels), len(p.Colors))
	}
	body, err := gunzipBase64(p.Data)
	if err != nil {
		return nil, err
	}
	// what the header carries leaves the tail; the rest stays as JSON
	tail := map[string]json.RawMessage{}
	for k, v := range fields {
		switch k {
		case "format", "origin", "step", "colors", "levels", "data":
		default:
			tail[k] = v
		}
	}
	extra, err := marshalSorted(tail)
	if err != nil {
		return nil, err
	}
	const meshes = 2 // the near record carries the block and the 2 m mesh
	out := make([]byte, 48, 48+48+4*len(p.Colors)+len(body)+16+len(extra))
	copy(out, "IXBM")
	le := binary.LittleEndian
	le.PutUint16(out[4:], payloadVersion)
	out[6] = meshes
	out[7] = byte(len(p.Colors))
	for i, v := range append(p.Origin, p.Step) {
		le.PutUint64(out[8+8*i:], math.Float64bits(v))
	}
	le.PutUint32(out[40:], uint32(len(extra)))
	le.PutUint32(out[44:], uint32(len(p.Levels)))
	for _, l := range p.Levels {
		out = le.AppendUint32(out, uint32(l.Vertices))
		out = le.AppendUint32(out, uint32(l.Triangles))
		out = le.AppendUint64(out, math.Float64bits(l.Error))
	}
	for _, c := range p.Colors {
		if len(c) != 3 {
			return nil, fmt.Errorf("building payload: a colour of %d channels", len(c))
		}
		out = append(out, byte(c[0]), byte(c[1]), byte(c[2]), 0)
	}
	if out, err = appendMeshes(out, body, p.Levels[:meshes]); err != nil {
		return nil, err
	}
	return append(out, extra...), nil
}

func finePayload(raw json.RawMessage) ([]byte, error) {
	var p struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	body, err := gunzipBase64(p.Data)
	if err != nil {
		return nil, err
	}
	// the counts are not in the JSON payload (the near record has them): found from the bytes,
	// 13 bytes per vertex and 6 per triangle with the vertex count as small as can be
	v, t, ok := fineCounts(body)
	if !ok {
		return nil, fmt.Errorf("fine payload: %d bytes are no mesh", len(body))
	}
	out := make([]byte, 16, 16+len(body)+12)
	copy(out, "IXBF")
	le := binary.LittleEndian
	le.PutUint16(out[4:], payloadVersion)
	le.PutUint32(out[8:], uint32(v))
	le.PutUint32(out[12:], uint32(t))
	return appendMeshes(out, body, []meshLevel{{Vertices: v, Triangles: t}})
}

// fineCounts finds the vertex and triangle counts of one mesh of 7 bytes per
// vertex and 6 per triangle: the split where every index is below the vertex
// count and the last vertex is used. Several splits fitting is an error (the
// proof checks every record against its near record's counts).
func fineCounts(body []byte) (int, int, bool) {
	le := binary.LittleEndian
	found, fv, ft := 0, 0, 0
	for v := 3; 7*v <= len(body); v++ {
		rest := len(body) - 7*v
		if rest%6 != 0 || rest == 0 {
			continue
		}
		t, idx, top := rest/6, body[7*v:], -1
		for i := 0; i < 3*t; i++ {
			k := int(le.Uint16(idx[2*i:]))
			if k >= v {
				top = -2
				break
			}
			top = max(top, k)
		}
		if top == v-1 {
			found, fv, ft = found+1, v, t
		}
	}
	return fv, ft, found == 1
}

func tilePayload(raw json.RawMessage) ([]byte, error) {
	var p struct {
		Level, Row, Col int
		Size            int
		Scale, Offset   float64
		Delta           string
		Data            string
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	values, err := gunzipBase64(p.Data)
	if err != nil {
		return nil, err
	}
	if len(values) != 2*p.Size*p.Size {
		return nil, fmt.Errorf("tile payload: %d bytes for size %d", len(values), p.Size)
	}
	flags := uint32(0)
	switch p.Delta {
	case "x":
		flags = 1
	case "", "none":
	default:
		return nil, fmt.Errorf("tile payload: delta %q", p.Delta)
	}
	out := make([]byte, 40, 40+len(values))
	copy(out, "IXTT")
	le := binary.LittleEndian
	le.PutUint16(out[4:], payloadVersion)
	le.PutUint16(out[6:], uint16(p.Size))
	le.PutUint32(out[8:], uint32(int32(p.Level)))
	le.PutUint32(out[12:], uint32(int32(p.Row)))
	le.PutUint32(out[16:], uint32(int32(p.Col)))
	le.PutUint32(out[20:], flags)
	le.PutUint64(out[24:], math.Float64bits(p.Scale))
	le.PutUint64(out[32:], math.Float64bits(p.Offset))
	return append(out, values...), nil
}

// marshalSorted is encoding/json of a map (keys sorted), or nothing for an empty one.
func marshalSorted(m map[string]json.RawMessage) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		var v bytes.Buffer
		if err := json.Compact(&v, m[k]); err != nil {
			return nil, err
		}
		b.Write(v.Bytes())
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// payloads writes, for every record of a binary collection in the files, the
// record with its binary payload in base64 (to compare readers of both forms).
func payloads(files []string, w io.Writer) error {
	enc := json.NewEncoder(w)
	for _, name := range files {
		if err := eachLine(name, func(r record) error {
			if !binaryCollections[r.Collection] || len(r.Payload) == 0 {
				return nil
			}
			bin, err := binaryPayload(r.Payload)
			if err != nil {
				return fmt.Errorf("%s %s/%s: %w", r.Collection, r.Location, r.ID, err)
			}
			return enc.Encode(struct {
				Collection, Location, ID string
				Payload                  []byte
			}{r.Collection, r.Location, r.ID, bin})
		}); err != nil {
			return err
		}
	}
	return nil
}

func eachLine(name string, f func(record) error) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 1<<20), 64<<20)
	for scan.Scan() {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			return err
		}
		if err := f(r); err != nil {
			return err
		}
	}
	return scan.Err()
}
