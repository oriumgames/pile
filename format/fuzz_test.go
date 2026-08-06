package format

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/world/chunk"
)

// FuzzReadStructure asserts the structure decoder never panics on arbitrary
// input.
func FuzzReadStructure(f *testing.F) {
	reg := testRegistry(f)
	data, err := NewStructureData([3]int32{20, 10, 20})
	if err != nil {
		f.Fatal(err)
	}
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	cell := chunk.NewSubChunk(reg.AirRuntimeID())
	cell.SetBlock(1, 2, 3, 0, stone)
	data.Cells[0] = cell
	data.BlockEntities = append(data.BlockEntities, StructureBlockEntity{
		Pos: [3]int32{1, 2, 3}, Data: map[string]any{"id": "minecraft:chest"},
	})
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionDefault}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("PILE"))
	f.Fuzz(func(t *testing.T, in []byte) {
		_, _ = ReadStructure(in, reg)
		_, _ = UnresolvedStates(in, reg)
	})
}

// FuzzOpenIndexed asserts opening and fully reading arbitrary bytes as an
// indexed world never panics.
func FuzzOpenIndexed(f *testing.F) {
	reg := testRegistry(f)
	// Seed: a valid indexed file.
	dir := f.TempDir()
	path := filepath.Join(dir, "seed.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		f.Fatal(err)
	}
	if err := w.Store(buildTestColumn(f, reg, 0, 0)); err != nil {
		f.Fatal(err)
	}
	if err := w.Close(); err != nil {
		f.Fatal(err)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Fuzz(func(t *testing.T, in []byte) {
		p := filepath.Join(t.TempDir(), "fuzz.pile")
		if err := os.WriteFile(p, in, 0o644); err != nil {
			t.Skip()
		}
		iw, err := OpenIndexed(p, reg, true)
		if err != nil {
			return
		}
		for _, k := range iw.Positions() {
			_, _ = iw.Column(k[0], k[1])
		}
		_, _ = iw.UnresolvedStates()
		_ = iw.Close()
	})
}

// FuzzNBTStability: any bytes that decode as NBT must re-encode
// deterministically and re-decode to the same value (canonical stability of
// the deterministic encoder against gophertunnel's decoder).
func FuzzNBTStability(f *testing.F) {
	seed, _ := marshalNBT(map[string]any{
		"a": int64(1), "b": "x", "c": []any{float32(1), float32(2)},
		"d": map[string]any{"e": byte(3)}, "f": []int32{1, 2, 3},
	})
	f.Add(seed)
	f.Add([]byte{10, 0, 0, 0}) // empty root compound
	f.Fuzz(func(t *testing.T, in []byte) {
		m, err := unmarshalNBT(in)
		if err != nil {
			return
		}
		enc1, err := marshalNBT(m)
		if err != nil {
			// Values gophertunnel produces must always be encodable.
			t.Fatalf("decoded NBT failed to re-encode: %v (%#v)", err, m)
		}
		m2, err := unmarshalNBT(enc1)
		if err != nil {
			t.Fatalf("re-encoded NBT failed to decode: %v", err)
		}
		enc2, err := marshalNBT(m2)
		if err != nil {
			t.Fatal(err)
		}
		// Compare encodings rather than values: they express the same
		// equivalence for storage purposes and, unlike reflect.DeepEqual,
		// treat NaN payloads as equal to themselves.
		if !bytes.Equal(enc1, enc2) {
			t.Fatalf("NBT value changed across round trip:\n%#v\n%#v", m, m2)
		}
	})
}
