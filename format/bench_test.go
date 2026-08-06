package format

import (
	"bytes"
	"path/filepath"
	"testing"
)

// benchWorld builds a 64-chunk world with realistic content.
func benchWorld(b *testing.B) *WorldData {
	reg := testRegistry(b)
	d := &WorldData{}
	for x := range int32(8) {
		for z := range int32(8) {
			d.Columns = append(d.Columns, buildTestColumn(b, reg, x, z))
		}
	}
	return d
}

func BenchmarkWriteWorld(b *testing.B) {
	reg := testRegistry(b)
	d := benchWorld(b)
	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(buf.Len()))
	}
}

func BenchmarkWriteWorldFast(b *testing.B) {
	reg := testRegistry(b)
	d := benchWorld(b)
	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault, FastCompression: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadWorld(b *testing.B) {
	reg := testRegistry(b)
	d := benchWorld(b)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
		b.Fatal(err)
	}
	file := buf.Bytes()
	b.SetBytes(int64(len(file)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := ReadWorld(file, reg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIndexedStore(b *testing.B) {
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	col := buildTestColumn(b, reg, 0, 0)
	b.ResetTimer()
	for b.Loop() {
		if err := w.Store(col); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIndexedColumn(b *testing.B) {
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	if err := w.Store(buildTestColumn(b, reg, 0, 0)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := w.Column(0, 0); err != nil {
			b.Fatal(err)
		}
	}
}
