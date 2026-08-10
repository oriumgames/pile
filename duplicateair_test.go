package pile

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// bedrockStorage writes one paletted storage as Bedrock stores it: a header of
// (bits << 1) | 1, the packed index words, the palette count, and the entries.
func bedrockStorage(t *testing.T, bits int, indices []uint16, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteByte(byte(bits<<1) | 1)
	per := 32 / bits
	words := (4096 + per - 1) / per
	packed := make([]uint32, words)
	for i, v := range indices {
		packed[i/per] |= uint32(v) << ((i % per) * bits)
	}
	for _, w := range packed {
		_ = binary.Write(&b, binary.LittleEndian, w)
	}
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(names)))
	enc := nbt.NewEncoderWithEncoding(&b, nbt.LittleEndian)
	for _, n := range names {
		if err := enc.Encode(struct {
			Name    string         `nbt:"name"`
			State   map[string]any `nbt:"states"`
			Version int32          `nbt:"version"`
		}{n, map[string]any{}, chunk.CurrentBlockVersion}); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

// TestDuplicateAirPaletteRoundTrips.
//
// A Bedrock storage may list air in more than one palette slot; the game writes
// one when an edit removes the last non-air block without rebuilding the
// palette, and dragonfly's SubChunk.Empty only recognises the single-slot form,
// so the section arrives looking like content.
//
// pile's writer agreed with that reading and stored the layer. Global
// resolution then folded both slots onto the one air entry and the layer went
// out uniformly air -- which §4.3 forbids and this package's own reader
// refuses. One chunk of a converted 1 806-chunk skyblock lobby was shaped this
// way, and it made the entire converted world unopenable while `pile convert`
// reported success.
func TestDuplicateAirPaletteRoundTrips(t *testing.T) {
	reg := world.DefaultBlockRegistry
	reg.Finalize()

	// Indices split between two palette slots that are both air, which is what
	// makes the palette two entries long after unused slots are dropped.
	idx := make([]uint16, 4096)
	for i := range idx {
		idx[i] = uint16(i % 2)
	}
	sub := append([]byte{9, 1, 0},
		bedrockStorage(t, 1, idx, "minecraft:air", "minecraft:air")...)

	data := chunk.SerialisedData{Biomes: make([]byte, 0)}
	data.SubChunks = make([][]byte, 24)
	data.SubChunks[4] = sub
	// A real block elsewhere, so the column is not simply empty and the
	// section under test is the one deciding the outcome.
	data.SubChunks[5] = append([]byte{9, 1, 1},
		bedrockStorage(t, 1, make([]uint16, 4096), "minecraft:stone")...)

	ch, err := chunk.DiskDecode(reg, data, cube.Range{-64, 319})
	if err != nil {
		t.Skipf("could not build the fixture through DiskDecode: %v", err)
	}
	if got := len(ch.Sub()[4].Layers()); got != 1 {
		t.Fatalf("the fixture's section has %d layers, want 1", got)
	}
	if ch.Sub()[4].Empty() {
		t.Fatal("the fixture's two-slot air section reads as empty, so it proves nothing")
	}

	var buf bytes.Buffer
	d := &format.WorldData{Columns: []format.Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatalf("write refused a legal world: %v", err)
	}
	back, err := format.ReadWorld(buf.Bytes(), reg)
	if err != nil {
		t.Fatalf("the writer produced a file it cannot read back: %v", err)
	}
	if len(back.Columns) != 1 {
		t.Fatalf("read back %d columns, want 1", len(back.Columns))
	}
	// The air section is absent, and the real one survived.
	air := reg.AirRuntimeID()
	got := back.Columns[0].Col.Chunk
	if rid := got.Block(0, 4*16-64, 0, 0); rid != air {
		t.Errorf("the all-air section came back as %d, want air", rid)
	}
	if rid := got.Block(0, 5*16-64, 0, 0); rid == air {
		t.Error("the section holding stone was lost")
	}
}
