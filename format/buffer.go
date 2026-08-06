package format

import (
	"encoding/binary"
	"fmt"
	"math"
)

// writer is an append-only byte buffer with typed little-endian writes.
// It is a thin wrapper over a byte slice; all methods are inlineable.
type writer struct {
	b []byte
}

func (w *writer) bytes() []byte    { return w.b }
func (w *writer) len() int         { return len(w.b) }
func (w *writer) u8(v uint8)       { w.b = append(w.b, v) }
func (w *writer) raw(p []byte)     { w.b = append(w.b, p...) }
func (w *writer) u16(v uint16)     { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32)     { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64)     { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *writer) i32(v int32)      { w.u32(uint32(v)) }
func (w *writer) f32(v float32)    { w.u32(math.Float32bits(v)) }
func (w *writer) f64(v float64)    { w.u64(math.Float64bits(v)) }
func (w *writer) uvarint(v uint64) { w.b = binary.AppendUvarint(w.b, v) }
func (w *writer) svarint(v int64)  { w.b = binary.AppendVarint(w.b, v) }

func (w *writer) str(s string) {
	w.uvarint(uint64(len(s)))
	w.b = append(w.b, s...)
}

func (w *writer) blob(p []byte) {
	w.uvarint(uint64(len(p)))
	w.b = append(w.b, p...)
}

// checkBlob reports whether a blob is within the decoder's limit, so writers
// never emit a file they could not read back.
func checkBlob(p []byte, what string) error {
	if len(p) > maxBlobLen {
		return fmt.Errorf("pile: %s is %d bytes, limit %d", what, len(p), maxBlobLen)
	}
	return nil
}

// checkString reports whether a string is within the decoder's limit.
func checkString(s, what string) error {
	if len(s) > maxStringLen {
		return fmt.Errorf("pile: %s is %d bytes, limit %d", what, len(s), maxStringLen)
	}
	return nil
}

// reader consumes a byte slice with typed little-endian reads. All reads
// validate remaining length and report ErrCorrupt-wrapped errors.
type reader struct {
	b   []byte
	off int
}

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, corruptf("unexpected end of data (want %d bytes, have %d)", n, r.remaining())
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p, nil
}

func (r *reader) u8() (uint8, error) {
	p, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return p[0], nil
}

func (r *reader) u32() (uint32, error) {
	p, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(p), nil
}

func (r *reader) u64() (uint64, error) {
	p, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(p), nil
}

func (r *reader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		return 0, corruptf("invalid uvarint at offset %d", r.off)
	}
	// Reject overlong encodings: canonical form is required so that identical
	// content has exactly one valid representation.
	if n != uvarintLen(v) {
		return 0, corruptf("non-minimal uvarint at offset %d", r.off)
	}
	r.off += n
	return v, nil
}

// uvarintLen returns the number of bytes the minimal encoding of v occupies.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func (r *reader) svarint() (int64, error) {
	v, n := binary.Varint(r.b[r.off:])
	if n <= 0 {
		return 0, corruptf("invalid svarint at offset %d", r.off)
	}
	if n != uvarintLen(uint64(v)<<1^uint64(v>>63)) {
		return 0, corruptf("non-minimal svarint at offset %d", r.off)
	}
	r.off += n
	return v, nil
}

// svarint32 reads a signed varint that must fit in an int32.
func (r *reader) svarint32(what string) (int32, error) {
	v, err := r.svarint()
	if err != nil {
		return 0, err
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, corruptf("%s %d out of int32 range", what, v)
	}
	return int32(v), nil
}

// bitsetTail reports whether the padding bits above n in a bitset are zero,
// so a bitset has exactly one encoding for a given set of bits.
func bitsetTail(b []byte, n int) error {
	if n%8 == 0 {
		return nil
	}
	if last := b[len(b)-1]; last>>(n%8) != 0 {
		return corruptf("non-zero padding bits in bitset")
	}
	return nil
}

// count reads a uvarint and validates it against an upper bound.
func (r *reader) count(max uint64, what string) (int, error) {
	v, err := r.uvarint()
	if err != nil {
		return 0, err
	}
	if v > max {
		return 0, corruptf("%s count %d exceeds limit %d", what, v, max)
	}
	return int(v), nil
}

func (r *reader) str() (string, error) {
	n, err := r.count(maxStringLen, "string length")
	if err != nil {
		return "", err
	}
	p, err := r.take(n)
	if err != nil {
		return "", err
	}
	return string(p), nil
}

// blob reads a length-prefixed byte slice. The returned slice aliases the
// reader's backing array; callers that retain it beyond the decode must copy.
func (r *reader) blob() ([]byte, error) {
	n, err := r.count(maxBlobLen, "blob length")
	if err != nil {
		return nil, err
	}
	return r.take(n)
}
