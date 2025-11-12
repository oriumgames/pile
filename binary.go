package pile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// buffer is a helper for writing binary data with convenient typed methods.
type buffer struct {
	bytes.Buffer
}

// newBuffer creates a new buffer.
func newBuffer() *buffer {
	return &buffer{}
}

// WriteUInt64 writes a uint64 in big-endian format.
func (b *buffer) WriteUInt64(v uint64) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// WriteInt64 writes an int64 in big-endian format.
func (b *buffer) WriteInt64(v int64) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// WriteUInt32 writes a uint32 in big-endian format.
func (b *buffer) WriteUInt32(v uint32) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// WriteInt32 writes an int32 in big-endian format.
func (b *buffer) WriteInt32(v int32) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// WriteInt16 writes an int16 in big-endian format.
func (b *buffer) WriteInt16(v int16) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// WriteInt8 writes an int8.
func (b *buffer) WriteInt8(v int8) {
	_ = b.WriteByte(byte(v))
}

// WriteBool writes a boolean as a byte (0 or 1).
func (b *buffer) WriteBool(v bool) {
	if v {
		_ = b.WriteByte(1)
	} else {
		_ = b.WriteByte(0)
	}
}

// WriteVarInt writes a variable-length integer.
func (b *buffer) WriteVarInt(v int64) {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutVarint(buf, v)
	_, _ = b.Write(buf[:n])
}

// WriteString writes a string with its length as a varint.
func (b *buffer) WriteString(s string) {
	b.WriteVarInt(int64(len(s)))
	_, _ = b.Write([]byte(s))
}

// WriteBytes writes a byte slice with its length as a varint.
func (b *buffer) WriteBytes(data []byte) {
	b.WriteVarInt(int64(len(data)))
	_, _ = b.Write(data)
}

// writeVarInt writes a variable-length integer to a writer.
func writeVarInt(w io.Writer, v int64) error {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutVarint(buf, v)
	_, err := w.Write(buf[:n])
	return err
}

// readVarInt reads a variable-length integer from a reader.
func readVarInt(r io.Reader) (int64, error) {
	// Use io.ByteReader interface for binary.ReadVarint
	br, ok := r.(io.ByteReader)
	if !ok {
		// Wrap in a byte reader
		br = &byteReader{r: r}
	}
	v, err := binary.ReadVarint(br)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// byteReader wraps an io.Reader to implement io.ByteReader
type byteReader struct {
	r io.Reader
}

func (br *byteReader) ReadByte() (byte, error) {
	b := make([]byte, 1)
	n, err := br.r.Read(b)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return b[0], nil
}

// reader is a helper for reading binary data with convenient typed methods.
type reader struct {
	r io.Reader
}

// newReader creates a new reader wrapping the given io.Reader.
func newReader(r io.Reader) *reader {
	return &reader{r: r}
}

// ReadUInt64 reads a uint64 in big-endian format.
func (r *reader) ReadUInt64() (uint64, error) {
	var v uint64
	err := binary.Read(r.r, binary.BigEndian, &v)
	return v, err
}

// ReadInt64 reads an int64 in big-endian format.
func (r *reader) ReadInt64() (int64, error) {
	var v int64
	err := binary.Read(r.r, binary.BigEndian, &v)
	return v, err
}

// ReadUInt32 reads a uint32 in big-endian format.
func (r *reader) ReadUInt32() (uint32, error) {
	var v uint32
	err := binary.Read(r.r, binary.BigEndian, &v)
	return v, err
}

// ReadInt32 reads an int32 in big-endian format.
func (r *reader) ReadInt32() (int32, error) {
	var v int32
	err := binary.Read(r.r, binary.BigEndian, &v)
	return v, err
}

// ReadInt16 reads an int16 in big-endian format.
func (r *reader) ReadInt16() (int16, error) {
	var v int16
	err := binary.Read(r.r, binary.BigEndian, &v)
	return v, err
}

// ReadInt8 reads an int8.
func (r *reader) ReadInt8() (int8, error) {
	b, err := r.ReadByte()
	return int8(b), err
}

// ReadByte reads a single byte.
func (r *reader) ReadByte() (byte, error) {
	b := make([]byte, 1)
	_, err := io.ReadFull(r.r, b)
	return b[0], err
}

// ReadBool reads a boolean (0 or 1).
func (r *reader) ReadBool() (bool, error) {
	b, err := r.ReadByte()
	return b != 0, err
}

// ReadVarInt reads a variable-length integer.
func (r *reader) ReadVarInt() (int64, error) {
	return readVarInt(r.r)
}

// ReadString reads a string with its length as a varint.
func (r *reader) ReadString() (string, error) {
	length, err := r.ReadVarInt()
	if err != nil {
		return "", err
	}
	if length < 0 || length > 1<<20 { // 1MB limit
		return "", fmt.Errorf("invalid string length: %d", length)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// ReadBytes reads a byte slice with its length as a varint.
func (r *reader) ReadBytes() ([]byte, error) {
	length, err := r.ReadVarInt()
	if err != nil {
		return nil, err
	}
	if length < 0 || length > 1<<24 { // 16MB limit
		return nil, fmt.Errorf("invalid byte array length: %d", length)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ReadN reads exactly n bytes.
func (r *reader) ReadN(n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(r.r, buf)
	return buf, err
}
