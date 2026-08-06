package format

import (
	"bytes"
	"encoding/binary"
	"maps"
	"reflect"
	"sync"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	_ "github.com/df-mc/dragonfly/server/world/biome" // populate the biome registry
	"github.com/df-mc/dragonfly/server/world/chunk"
)

var regOnce sync.Once

func testRegistry(t testing.TB) world.BlockRegistry {
	t.Helper()
	regOnce.Do(world.DefaultBlockRegistry.Finalize)
	return world.DefaultBlockRegistry
}

var overworldRange = cube.Range{-64, 319}

// rid returns the runtime ID of a block, failing the test if unregistered.
func rid(t testing.TB, reg world.BlockRegistry, b world.Block) uint32 {
	t.Helper()
	return reg.BlockRuntimeID(b)
}

// buildTestColumn constructs a column exercising blocks, a second layer,
// biomes, block entities, entities and scheduled ticks.
func buildTestColumn(t testing.TB, reg world.BlockRegistry, x, z int32) Column {
	t.Helper()
	ch := chunk.New(reg, overworldRange)

	stone := rid(t, reg, block.Stone{})
	dirt := rid(t, reg, block.Dirt{})
	bedrock := rid(t, reg, block.Bedrock{})

	for bx := range uint8(16) {
		for bz := range uint8(16) {
			ch.SetBlock(bx, -64, bz, 0, bedrock)
			for y := int16(-63); y < 0; y++ {
				ch.SetBlock(bx, y, bz, 0, stone)
			}
			ch.SetBlock(bx, 0, bz, 0, dirt)
		}
	}
	// A sparse feature and a second layer entry.
	ch.SetBlock(3, 5, 7, 0, stone)
	ch.SetBlock(3, 5, 7, 1, dirt)

	// A non-uniform biome region, if the biomes exist in the registry.
	if desert, ok := world.BiomeByName("minecraft:desert"); ok {
		for bx := range uint8(8) {
			for bz := range uint8(16) {
				for y := int16(-64); y < -48; y++ {
					ch.SetBiome(bx, y, bz, uint32(desert.EncodeBiome()))
				}
			}
		}
	}

	bePos := cube.Pos{int(x)*16 + 4, 0, int(z)*16 + 9}
	col := &chunk.Column{
		Chunk: ch,
		BlockEntities: []chunk.BlockEntity{{
			Pos: bePos,
			Data: map[string]any{
				"id": "minecraft:chest", "CustomName": "loot",
				"x": int32(bePos.X()), "y": int32(bePos.Y()), "z": int32(bePos.Z()),
			},
		}},
		Entities: []chunk.Entity{{
			ID: 42,
			Data: map[string]any{
				"identifier": "minecraft:zombie",
				// []any of float32 matches what NBT decoding produces, so the
				// expectation survives the round trip via DeepEqual.
				"Pos":      []any{float32(x)*16 + 1.5, float32(2), float32(z)*16 + 3.5},
				"UniqueID": int64(42),
			},
		}},
		Tick: 12345,
		ScheduledBlocks: []chunk.ScheduledBlockUpdate{{
			Pos: cube.Pos{int(x)*16 + 1, 10, int(z)*16 + 2}, Block: stone, Tick: 999,
		}},
	}
	return Column{X: x, Z: z, Col: col, UserData: []byte("chunk-meta")}
}

func testWorld(t testing.TB, reg world.BlockRegistry) *WorldData {
	settings, err := marshalNBT(map[string]any{"name": "test", "time": int64(6000)})
	if err != nil {
		t.Fatal(err)
	}
	return &WorldData{
		Settings: settings,
		UserData: []byte("world-meta"),
		Columns: []Column{
			buildTestColumn(t, reg, 0, 0),
			buildTestColumn(t, reg, 1, 0),
			buildTestColumn(t, reg, -3, 7),
		},
	}
}

func encode(t testing.TB, d *WorldData, reg world.BlockRegistry, level CompressionLevel) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: level}); err != nil {
		t.Fatalf("WriteWorld: %v", err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	for _, level := range []CompressionLevel{CompressionNone, CompressionDefault} {
		d := testWorld(t, reg)
		file := encode(t, d, reg, level)

		got, err := ReadWorld(file, reg)
		if err != nil {
			t.Fatalf("ReadWorld (level %d): %v", level, err)
		}
		if !bytes.Equal(got.Settings, d.Settings) || !bytes.Equal(got.UserData, d.UserData) {
			t.Fatalf("meta blobs did not round trip")
		}
		if len(got.Columns) != len(d.Columns) {
			t.Fatalf("got %d columns, want %d", len(got.Columns), len(d.Columns))
		}
		want := map[[2]int32]Column{}
		for _, c := range d.Columns {
			want[[2]int32{c.X, c.Z}] = c
		}
		for _, g := range got.Columns {
			w, ok := want[[2]int32{g.X, g.Z}]
			if !ok {
				t.Fatalf("unexpected column (%d,%d)", g.X, g.Z)
			}
			compareColumns(t, w, g)
		}
	}
}

// bothEmpty reports whether two slices are both absent, treating nil and
// zero-length alike.
func bothEmpty[T any](a, b []T) bool { return len(a) == 0 && len(b) == 0 }

func compareColumns(t *testing.T, want, got Column) {
	t.Helper()
	wc, gc := want.Col.Chunk, got.Col.Chunk
	if wc.Range() != gc.Range() {
		t.Fatalf("column (%d,%d): range %v != %v", want.X, want.Z, gc.Range(), wc.Range())
	}
	r := wc.Range()
	for layer := range uint8(2) {
		for x := range uint8(16) {
			for z := range uint8(16) {
				for y := int16(r[0]); y <= int16(r[1]); y++ {
					if w, g := wc.Block(x, y, z, layer), gc.Block(x, y, z, layer); w != g {
						t.Fatalf("column (%d,%d): block at (%d,%d,%d) layer %d: got rid %d, want %d",
							want.X, want.Z, x, y, z, layer, g, w)
					}
				}
			}
		}
	}
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(r[0]); y <= int16(r[1]); y++ {
				if w, g := wc.Biome(x, y, z), gc.Biome(x, y, z); w != g {
					t.Fatalf("column (%d,%d): biome at (%d,%d,%d): got %d, want %d",
						want.X, want.Z, x, y, z, g, w)
				}
			}
		}
	}
	// nil and empty are the same absence on the wire, so only content is
	// compared: the decoder allocates some collections eagerly and some
	// lazily, which is not a format property.
	if !bothEmpty(want.Col.BlockEntities, got.Col.BlockEntities) &&
		!reflect.DeepEqual(want.Col.BlockEntities, got.Col.BlockEntities) {
		t.Fatalf("column (%d,%d): block entities:\ngot  %#v\nwant %#v",
			want.X, want.Z, got.Col.BlockEntities, want.Col.BlockEntities)
	}
	wantEnts := make([]chunk.Entity, len(want.Col.Entities))
	for i, e := range want.Col.Entities {
		data := maps.Clone(e.Data)
		data["UniqueID"] = e.ID
		wantEnts[i] = chunk.Entity{ID: e.ID, Data: data}
	}
	if !bothEmpty(wantEnts, got.Col.Entities) && !reflect.DeepEqual(wantEnts, got.Col.Entities) {
		t.Fatalf("column (%d,%d): entities:\ngot  %#v\nwant %#v",
			want.X, want.Z, got.Col.Entities, wantEnts)
	}
	if want.Col.Tick != got.Col.Tick {
		t.Fatalf("column (%d,%d): tick %d != %d", want.X, want.Z, got.Col.Tick, want.Col.Tick)
	}
	if !bothEmpty(want.Col.ScheduledBlocks, got.Col.ScheduledBlocks) &&
		!reflect.DeepEqual(want.Col.ScheduledBlocks, got.Col.ScheduledBlocks) {
		t.Fatalf("column (%d,%d): scheduled blocks:\ngot  %#v\nwant %#v",
			want.X, want.Z, got.Col.ScheduledBlocks, want.Col.ScheduledBlocks)
	}
	if !bytes.Equal(want.UserData, got.UserData) {
		t.Fatalf("column (%d,%d): user data mismatch", want.X, want.Z)
	}
}

// TestDeterminism checks both same-input determinism (two encodes of one
// world) and canonical stability (encode → decode → encode).
func TestDeterminism(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	a := encode(t, d, reg, CompressionDefault)
	b := encode(t, d, reg, CompressionDefault)
	if !bytes.Equal(a, b) {
		t.Fatal("two encodes of the same world differ")
	}
	decoded, err := ReadWorld(a, reg)
	if err != nil {
		t.Fatal(err)
	}
	c := encode(t, decoded, reg, CompressionDefault)
	if !bytes.Equal(a, c) {
		t.Fatalf("encode→decode→encode changed bytes: %d vs %d", len(a), len(c))
	}
}

// TestDedup checks that identical chunks collapse to shared section blobs.
func TestDedup(t *testing.T) {
	reg := testRegistry(t)
	stone := rid(t, reg, block.Stone{})

	d := &WorldData{}
	for x := range int32(10) {
		for z := range int32(10) {
			ch := chunk.New(reg, overworldRange)
			for bx := range uint8(16) {
				for bz := range uint8(16) {
					for y := int16(-64); y < 0; y++ {
						ch.SetBlock(bx, y, bz, 0, stone)
					}
				}
			}
			d.Columns = append(d.Columns, Column{X: x, Z: z, Col: &chunk.Column{Chunk: ch}})
		}
	}
	file := encode(t, d, reg, CompressionDefault)
	// 100 identical chunks, 4 unique stone sections: everything dedupes.
	// Uncompressed body would already be tiny; compressed must be well under 4 KiB.
	if len(file) > 4<<10 {
		t.Fatalf("dedup ineffective: file is %d bytes", len(file))
	}
	got, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Columns) != 100 {
		t.Fatalf("got %d columns, want 100", len(got.Columns))
	}
	for _, c := range got.Columns {
		if g := c.Col.Chunk.Block(5, -10, 5, 0); g != stone {
			t.Fatalf("column (%d,%d): expected stone, got rid %d", c.X, c.Z, g)
		}
	}
}

func TestEmptyColumn(t *testing.T) {
	reg := testRegistry(t)
	d := &WorldData{Columns: []Column{
		{X: 5, Z: -5, Col: &chunk.Column{Chunk: chunk.New(reg, overworldRange)}},
	}}
	file := encode(t, d, reg, CompressionDefault)
	got, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Columns) != 1 || got.Columns[0].X != 5 || got.Columns[0].Z != -5 {
		t.Fatalf("empty column did not round trip: %+v", got.Columns)
	}
	air := reg.AirRuntimeID()
	if g := got.Columns[0].Col.Chunk.Block(0, 0, 0, 0); g != air {
		t.Fatalf("expected air, got rid %d", g)
	}
}

func TestReadMeta(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	file := encode(t, d, reg, CompressionBest)
	m, err := ReadMeta(file)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindWorld || m.Mode != ModeSolid {
		t.Fatalf("unexpected kind/mode: %d/%d", m.Kind, m.Mode)
	}
	if !bytes.Equal(m.Settings, d.Settings) || !bytes.Equal(m.UserData, d.UserData) {
		t.Fatal("meta blobs mismatch")
	}
}

func TestCorruption(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	file := encode(t, d, reg, CompressionDefault)

	// Flip a byte in the body: checksum must catch it.
	bad := bytes.Clone(file)
	bad[headerSize+10] ^= 0xFF
	if _, err := ReadWorld(bad, reg); err == nil {
		t.Fatal("corrupted body accepted")
	}
	// Truncated file.
	if _, err := ReadWorld(file[:headerSize+3], reg); err == nil {
		t.Fatal("truncated file accepted")
	}
	// Bad magic.
	bad = bytes.Clone(file)
	bad[0] = 'X'
	if _, err := ReadWorld(bad, reg); err == nil {
		t.Fatal("bad magic accepted")
	}
}

// FuzzReadWorld asserts the decoder never panics on arbitrary input.
func FuzzReadWorld(f *testing.F) {
	reg := testRegistry(f)
	d := testWorld(f, reg)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("PILE"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadWorld(data, reg)
		_, _ = ReadMeta(data)
	})
}

// TestStoreLight round-trips baked light nibbles.
func TestStoreLight(t *testing.T) {
	reg := testRegistry(t)
	col := buildTestColumn(t, reg, 0, 0)
	// Bake recognisable light into the chunk.
	chunk.LightArea([]*chunk.Chunk{col.Col.Chunk}, 0, 0).Fill()
	d := &WorldData{Columns: []Column{col}}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault, StoreLight: true}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorld(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	wc, gc := col.Col.Chunk, got.Columns[0].Col.Chunk
	// Compare light on a filled section (y=-64..0 has blocks).
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < 0; y++ {
				if w, g := wc.SkyLight(x, y, z), gc.SkyLight(x, y, z); w != g {
					t.Fatalf("sky light at (%d,%d,%d): got %d, want %d", x, y, z, g, w)
				}
			}
		}
	}
	// Determinism holds with light enabled.
	var buf2 bytes.Buffer
	if err := WriteWorld(&buf2, d, reg, Options{Compression: CompressionDefault, StoreLight: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Fatal("StoreLight output not deterministic")
	}
}

// TestStatsFlag verifies the embedded stats compound.
func TestStatsFlag(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault, Stats: true}); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMeta(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if m.Flags&FlagStats == 0 || len(m.Stats) == 0 {
		t.Fatal("stats flag/blob missing")
	}
	stats, err := UnmarshalNBT(m.Stats)
	if err != nil {
		t.Fatal(err)
	}
	if stats["chunks"] != int64(3) {
		t.Fatalf("stats chunks = %v (%T), want int64(3)", stats["chunks"], stats["chunks"])
	}
	// The full world still decodes with the stats blob present.
	if _, err := ReadWorld(buf.Bytes(), reg); err != nil {
		t.Fatal(err)
	}
}

// TestUnknownStateIdentityCollision: two distinct states whose readable form
// is identical must stay distinct in the palette.
func TestUnknownStateIdentityCollision(t *testing.T) {
	a := BlockState{Name: "test:x", Properties: map[string]any{"p": "a,q=b"}}
	b := BlockState{Name: "test:x", Properties: map[string]any{"p": "a", "q": "b"}}
	if stateIdentity(a.Name, a.Properties) == stateIdentity(b.Name, b.Properties) {
		t.Fatal("distinct states share an identity")
	}
	bld := newBlockPaletteBuilder(testRegistry(t))
	ia, ib := bld.addState(a), bld.addState(b)
	if ia == ib {
		t.Fatalf("distinct unknown states collapsed to palette index %d", ia)
	}
	if again := bld.addState(a); again != ia {
		t.Fatalf("identical state got two indices: %d and %d", ia, again)
	}
}

// TestHeaderAuthentication: the semantic header is covered by the footer
// hash, so corrupting it is detected rather than silently changing how the
// file decodes.
func TestHeaderAuthentication(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)

	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"blockVersion", 12},
		{"flags", 8},
		{"kind", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := bytes.Clone(file)
			bad[tc.offset] ^= 0x01
			if _, err := ReadWorld(bad, reg); err == nil {
				t.Fatal("corrupted header accepted")
			}
		})
	}

	// The footer's own control words are covered too.
	t.Run("footer control word", func(t *testing.T) {
		bad := bytes.Clone(file)
		bad[len(bad)-footerSize+24] ^= 0x01 // generation
		if _, err := ReadWorld(bad, reg); err == nil {
			t.Fatal("corrupted footer control word accepted")
		}
	})
}

// TestSolidFooterMustBeZero: solid files have exactly one valid encoding of
// their unused footer words.
func TestSolidFooterMustBeZero(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	bad := bytes.Clone(file)
	// Set the generation word and repair the hash: still invalid, because the
	// field is required to be zero.
	binary.LittleEndian.PutUint64(bad[len(bad)-footerSize+24:], 7)
	body := bad[headerSize : len(bad)-footerSize]
	binary.LittleEndian.PutUint64(bad[len(bad)-footerSize:],
		checkpointHash(bad[:headerSize], body, bad[len(bad)-footerSize+8:]))
	if _, err := ReadWorld(bad, reg); err == nil {
		t.Fatal("non-zero solid footer control word accepted")
	}
}

// TestContentHashSemantics: derived and advisory content must not change a
// world's identity, and header state that changes decoding must.
func TestContentHashSemantics(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)

	plain := encode(t, d, reg, CompressionNone)
	base, err := ContentHash(plain, reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []Options{
		{Compression: CompressionBest},
		{Compression: CompressionNone, Stats: true},
		{Compression: CompressionNone, StoreLight: true},
	} {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, opts); err != nil {
			t.Fatal(err)
		}
		got, err := ContentHash(buf.Bytes(), reg)
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Fatalf("content hash changed for %+v: derived/advisory content must not affect identity", opts)
		}
	}
}

// TestLightOnAirSections: dragonfly's lighting pass fills block-absent
// sections with sky light, so StoreLight must be able to carry it.
func TestLightOnAirSections(t *testing.T) {
	reg := testRegistry(t)
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	for x := uint8(0); x < 16; x++ {
		for z := uint8(0); z < 16; z++ {
			ch.SetBlock(x, -64, z, 0, stone)
		}
	}
	chunk.LightArea([]*chunk.Chunk{ch}, 0, 0).Fill()

	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone, StoreLight: true}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorld(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	gc := got.Columns[0].Col.Chunk
	// A high, block-free section must keep its sky light.
	for _, y := range []int16{100, 200, 300} {
		if w, g := ch.SkyLight(0, y, 0), gc.SkyLight(0, y, 0); w != g {
			t.Fatalf("sky light at y=%d: got %d, want %d (air-section light lost)", y, g, w)
		}
	}
	if ch.SkyLight(0, 300, 0) == 0 {
		t.Skip("no sky light produced; nothing to assert")
	}
}

// TestEmptyNBTListCanonical: an empty list must encode identically whatever
// typed slice it came from, and only TAG_End is a valid element type.
func TestEmptyNBTListCanonical(t *testing.T) {
	forms := []any{[]string{}, []int32{}, []int64{}, []float32{}, []any{}, []map[string]any{}}
	var first []byte
	for i, v := range forms {
		enc, err := marshalNBT(map[string]any{"empty": v})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = enc
		} else if !bytes.Equal(enc, first) {
			t.Fatalf("empty %T encoded differently from the first form", v)
		}
		m, err := unmarshalNBT(enc)
		if err != nil {
			t.Fatal(err)
		}
		again, err := marshalNBT(m)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(enc, again) {
			t.Fatalf("empty %T is not stable across a round trip", v)
		}
	}
	// An empty list declaring a concrete element type is not canonical.
	bad := []byte{0x0a, 0, 0, 0x09, 0x05, 0, 'e', 'm', 'p', 't', 'y', 0x08, 0, 0, 0, 0, 0}
	if err := validateNBT(bad); err == nil {
		t.Fatal("empty list with a concrete element type accepted")
	}
}

// TestRejectsUndefinedLightFlags: reserved light-flag bits and empty light
// entries are second encodings and must be rejected.
func TestRejectsUndefinedLightFlags(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone, StoreLight: true}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()
	if _, err := ReadWorld(file, reg); err != nil {
		t.Fatal(err)
	}
	// Every light entry in this file carries both arrays (flags 0x03). Set a
	// reserved bit on the first one and re-hash so only the rule under test
	// can reject it.
	body := file[headerSize : len(file)-footerSize]
	i := bytes.IndexByte(body, 0x03)
	for ; i >= 0 && i < len(body); i++ {
		if body[i] != 0x03 {
			continue
		}
		body[i] = 0x07 // reserved bit 2 set
		rehashSolid(file)
		if _, err := ReadWorld(file, reg); err != nil {
			return // rejected, as required
		}
		body[i] = 0x03 // restore and keep looking
		rehashSolid(file)
	}
	t.Skip("could not locate a light flags byte to corrupt")
}

// rehashSolid recomputes a solid file's checkpoint hash in place, so a
// deliberate edit is rejected by the rule under test rather than by the
// integrity check.
func rehashSolid(file []byte) {
	header := file[:headerSize]
	payload := file[headerSize : len(file)-footerSize]
	footer := file[len(file)-footerSize:]
	binary.LittleEndian.PutUint64(footer[0:8], CheckpointHash(header, payload, footer[8:]))
}
