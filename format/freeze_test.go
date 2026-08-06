package format

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Tests for the normative validity rules the freeze review added: a decoder
// must accept exactly one encoding of any given content.

func TestRejectsNonMinimalVarints(t *testing.T) {
	r := &reader{b: []byte{0x80, 0x00}} // overlong zero
	if _, err := r.uvarint(); err == nil {
		t.Error("overlong uvarint accepted")
	}
	r = &reader{b: []byte{0x80, 0x00}}
	if _, err := r.svarint(); err == nil {
		t.Error("overlong svarint accepted")
	}
	r = &reader{b: []byte{0x00}}
	if v, err := r.uvarint(); err != nil || v != 0 {
		t.Errorf("minimal zero rejected: %v %v", v, err)
	}
}

func TestRejectsNonCanonicalBlob(t *testing.T) {
	// Descending references are not canonical.
	w := &writer{}
	w.uvarint(2)
	w.uvarint(5)
	w.uvarint(3)
	w.u8(widthU8)
	w.raw(make([]byte, 4096))
	if _, err := decodeOneBlob(&reader{b: w.bytes()}); err == nil {
		t.Error("descending palette references accepted")
	}

	// A two-entry palette must use u8 indices, not u16.
	w = &writer{}
	w.uvarint(2)
	w.uvarint(0)
	w.uvarint(1)
	w.u8(widthU16)
	w.raw(make([]byte, 8192))
	if _, err := decodeOneBlob(&reader{b: w.bytes()}); err == nil {
		t.Error("non-minimal index width accepted")
	}
}

func TestRejectsReservedFlags(t *testing.T) {
	reg := testRegistry(t)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, &WorldData{}, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()
	for _, bit := range []uint32{1 << 2, 1 << 5, 1 << 15} {
		bad := bytes.Clone(file)
		flags := uint32(bad[8]) | uint32(bad[9])<<8 | uint32(bad[10])<<16 | uint32(bad[11])<<24
		flags |= bit
		bad[8], bad[9], bad[10], bad[11] = byte(flags), byte(flags>>8), byte(flags>>16), byte(flags>>24)
		if _, err := ReadWorld(bad, reg); err == nil {
			t.Errorf("reserved flag bit 0x%X accepted", bit)
		}
	}
}

func TestRejectsUnalignedRange(t *testing.T) {
	reg := testRegistry(t)
	if AlignedRange(cube.Range{1, 16}) {
		t.Error("unaligned range reported as aligned")
	}
	if !AlignedRange(cube.Range{-64, 319}) {
		t.Error("the overworld range reported as unaligned")
	}
	ch := chunk.New(reg, cube.Range{1, 16})
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	if err := WriteWorld(&bytes.Buffer{}, d, reg, Options{}); err == nil {
		t.Error("a column with an unaligned range was encoded")
	}
}

// TestIndexedDetectsFrameCorruption: a shared frame must be authenticated, so
// bit rot in a palette segment or the metadata cannot silently alter content.
func TestIndexedDetectsFrameCorruption(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "auth.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	settings, _ := marshalNBT(map[string]any{"name": "abc"})
	if err := w.SetMeta(settings, []byte("abc"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(raw, []byte("abc"))
	if idx < 0 {
		t.Fatal("metadata not found in the uncompressed file")
	}
	raw[idx] = 'z'
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// The newest checkpoint references a frame that no longer matches its
	// hash. Serving the altered value would be the failure; falling back to
	// an older complete checkpoint (or refusing to open) is correct, and the
	// fallback must be visible to the caller.
	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		return // refused outright: also acceptable
	}
	defer r.Close()
	_, ud, _, _ := r.Meta()
	if string(ud) == "zbc" {
		t.Fatal("a corrupted metadata frame was served as valid")
	}
	if !r.Recovered() {
		t.Fatal("fell back to an older checkpoint without reporting it")
	}
}

// TestSettingsExtraKeysPreserved is the metadata-extensibility rule.
func TestSettingsExtraKeysPreserved(t *testing.T) {
	reg := testRegistry(t)
	settings, err := marshalNBT(map[string]any{
		"name": "x", "futureSetting": int32(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, &WorldData{Settings: settings}, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMeta(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	m, err := UnmarshalNBT(got.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if m["futureSetting"] != int32(7) {
		t.Fatalf("format dropped an unknown settings key: %v", m)
	}
	_ = world.Overworld
}
