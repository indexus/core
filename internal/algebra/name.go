package algebra

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// A collection is named by its description: the name is the first 96 bits of
// the SHA-256 of the canonical encoding, printed in the routing alphabet. Any
// node can check a description received from anyone against the name it was
// asked for, without an authority: the name is self-certifying.
//
// Canonical encoding IXC1 (little-endian, no padding):
//
//	"IXC1" u8 version=1 u8 bitsPerChar u8 width u8 0
//	[16] nonce  u8 recordLo u8 recordHi
//	space:      u8 axes; per axis f64 min f64 max
//	            [c*W]u8 schedule (the axis cut by every bit of the address),
//	            absent when there are no axes (opaque addresses)
//	schema:     u16 runs; per run u8 code u16 count
//	            code 0 sum, 1 min, 2 max, 3 first, 4 last: count columns
//	            code 5 affine: count pairs (affine.a, affine.b)
//	            two neighbouring runs never have the same code
//
// The encoding is the transfer format too: nodes exchange these bytes.

const (
	nameAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	nameDomain   = "indexus.collection.v1\x00"
)

var (
	// ErrEncoding reports bytes that are not a canonical IXC1 description.
	ErrEncoding = errors.New("algebra: not a canonical collection description")
	// ErrName reports a description that does not hash to the expected name.
	ErrName = errors.New("algebra: description does not match the collection name")
	// ErrCollision reports two different descriptions with the same name.
	ErrCollision = errors.New("algebra: two descriptions share one name")
)

// Canonical returns the IXC1 bytes of a valid description.
func (d *Descriptor) Canonical() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("IXC1")
	b.Write([]byte{1, byte(d.BitsPerChar), byte(d.Width), 0})
	b.Write(d.Nonce[:])
	b.Write([]byte{byte(d.RecordDepths[0]), byte(d.RecordDepths[1])})
	b.Write(d.Space.canonical())
	runs := d.Schema.runs()
	b.Write(binary.LittleEndian.AppendUint16(nil, uint16(len(runs))))
	for _, r := range runs {
		b.WriteByte(r.code)
		b.Write(binary.LittleEndian.AppendUint16(nil, r.count))
	}
	return b.Bytes(), nil
}

// Digest is the full SHA-256 of the description; the name is its first 96 bits.
func Digest(canonical []byte) [32]byte {
	return sha256.Sum256(append([]byte(nameDomain), canonical...))
}

// NameOf prints the first 96 bits of a digest as 16 routing characters.
func NameOf(digest [32]byte) string {
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		bit := i * 6
		var v uint16
		v = uint16(digest[bit/8])<<8 | uint16(digest[bit/8+1])
		out[i] = nameAlphabet[(v>>(10-bit%8))&63]
	}
	return string(out)
}

// Name is the collection name of a valid description.
func (d *Descriptor) Name() (string, error) {
	c, err := d.Canonical()
	if err != nil {
		return "", err
	}
	return NameOf(Digest(c)), nil
}

func (s Space) canonical() []byte {
	var b bytes.Buffer
	b.WriteByte(byte(len(s.Axes)))
	for _, a := range s.Axes {
		var u [16]byte
		binary.LittleEndian.PutUint64(u[:8], math.Float64bits(a.Min))
		binary.LittleEndian.PutUint64(u[8:16], math.Float64bits(a.Max))
		b.Write(u[:])
	}
	b.Write(s.Schedule)
	return b.Bytes()
}

// SpaceDigest identifies a Space on its own: collections with the same
// SpaceDigest have the same Cell for the same place, so a client may join
// them cell by cell.
func (s Space) SpaceDigest() [32]byte {
	return sha256.Sum256(append([]byte("indexus.space.v1\x00"), s.canonical()...))
}

// Decode parses IXC1 bytes and checks they are canonical (re-encoding gives
// the same bytes).
func Decode(data []byte) (*Descriptor, error) {
	r := bytes.NewReader(data)
	read := func(n int) []byte {
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return nil
		}
		return p
	}
	u8 := func() (int, bool) {
		c, err := r.ReadByte()
		return int(c), err == nil
	}
	head := read(8)
	if head == nil || string(head[:4]) != "IXC1" || head[4] != 1 || head[7] != 0 {
		return nil, ErrEncoding
	}
	d := &Descriptor{BitsPerChar: int(head[5]), Width: int(head[6])}
	nonce := read(16)
	if nonce == nil {
		return nil, ErrEncoding
	}
	copy(d.Nonce[:], nonce)
	lo, ok1 := u8()
	hi, ok2 := u8()
	if !ok1 || !ok2 {
		return nil, ErrEncoding
	}
	d.RecordDepths = [2]int{lo, hi}
	axes, ok := u8()
	if !ok {
		return nil, ErrEncoding
	}
	for a := 0; a < axes; a++ {
		p := read(16)
		if p == nil {
			return nil, ErrEncoding
		}
		d.Space.Axes = append(d.Space.Axes, Axis{
			Min: math.Float64frombits(binary.LittleEndian.Uint64(p[:8])),
			Max: math.Float64frombits(binary.LittleEndian.Uint64(p[8:16])),
		})
	}
	if axes > 0 && d.BitsPerChar*d.Width > 0 {
		sched := read(d.BitsPerChar * d.Width)
		if sched == nil {
			return nil, ErrEncoding
		}
		d.Space.Schedule = sched
	}
	nr := read(2)
	if nr == nil {
		return nil, ErrEncoding
	}
	for k := 0; k < int(binary.LittleEndian.Uint16(nr)); k++ {
		p := read(3)
		if p == nil {
			return nil, ErrEncoding
		}
		code, count := p[0], int(binary.LittleEndian.Uint16(p[1:]))
		if count == 0 || code > runAffine || len(d.Schema)+count > MaxColumns {
			return nil, ErrEncoding
		}
		for c := 0; c < count; c++ {
			if code == runAffine {
				d.Schema = append(d.Schema, AffineA, AffineB)
			} else {
				d.Schema = append(d.Schema, Kind(code))
			}
		}
	}
	if r.Len() != 0 {
		return nil, ErrEncoding
	}
	again, err := d.Canonical()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(again, data) {
		return nil, ErrEncoding
	}
	return d, nil
}

// Verify decodes a description received for a collection and checks that it
// is the one the name commits to.
func Verify(name string, data []byte) (*Descriptor, [32]byte, error) {
	d, err := Decode(data)
	if err != nil {
		return nil, [32]byte{}, err
	}
	digest := Digest(data)
	if NameOf(digest) != name {
		return nil, [32]byte{}, fmt.Errorf("%w: %s", ErrName, name)
	}
	return d, digest, nil
}

// Catalog is what a node keeps: the verified description of every collection
// it holds zones of, with the full digest. Two descriptions with one name can
// only exist if someone produced a 96-bit collision on purpose (about 2^48
// hashes for their own collection; taking over someone else's name needs a
// second preimage, about 2^96): the catalog refuses the second one.
type Catalog map[string]struct {
	Digest [32]byte
	Bytes  []byte
	Desc   *Descriptor
}

// Admit verifies and stores a description; it is idempotent.
func (c Catalog) Admit(name string, data []byte) (*Descriptor, error) {
	d, digest, err := Verify(name, data)
	if err != nil {
		return nil, err
	}
	if held, ok := c[name]; ok {
		if held.Digest != digest {
			return nil, fmt.Errorf("%w: %s", ErrCollision, name)
		}
		return held.Desc, nil
	}
	c[name] = struct {
		Digest [32]byte
		Bytes  []byte
		Desc   *Descriptor
	}{digest, append([]byte(nil), data...), d}
	return d, nil
}

// MaxColumns bounds the width of a schema: an embedding of a few thousand
// dimensions, one sum each.
const MaxColumns = 4096

const runAffine = 5

type run struct {
	code  byte
	count uint16
}

// runs is the schema as maximal runs of one code; an affine pair counts as
// one element of code 5.
func (s Schema) runs() []run {
	var out []run
	for i := 0; i < len(s); i++ {
		code := byte(s[i])
		if s[i] == AffineA {
			code = runAffine
			i++
		}
		if len(out) > 0 && out[len(out)-1].code == code {
			out[len(out)-1].count++
			continue
		}
		out = append(out, run{code, 1})
	}
	return out
}
