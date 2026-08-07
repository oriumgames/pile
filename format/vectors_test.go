package format

// The conformance vector appendix (FREEZE.md, "Conformance").
//
// A vector is a file whose exact bytes are checked into
// testdata/vectors, together with what a conforming implementation must
// conclude from them. They exist because the specification concedes that where
// prose and implementation disagree the implementation wins: a second
// implementation needs something more precise than prose to check itself
// against, and this is it.
//
// Three things are asserted per positive vector:
//
//  1. the checked-in bytes are exactly what the writer produces for the
//     vector's content (so a change in the writer fails here as it does in the
//     golden suite);
//  2. the bytes decode, and decoding and re-encoding reproduces them;
//  3. an independent walker (vectorwalk_test.go), written from format.md
//     rather than from this package's decoder, parses the file, accounts for
//     every byte, and agrees with the specification about the fields it finds.
//
// Negative vectors are files a conforming reader must reject. Each is a named
// mutation of a positive vector with its checkpoint hash repaired, so the file
// fails for the rule it is named after rather than for a checksum.
//
// Regenerating is guarded exactly as the golden suite is: `-update` rewrites
// the files, and `-update` refuses to bless changed bytes while format.Version
// is unchanged unless `-format-change` says the change is deliberate. The same
// two flags gate both suites, so locking out `-update` at freeze locks both.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

const (
	vectorDir      = "testdata/vectors"
	vectorManifest = vectorDir + "/vectors_manifest.txt"
	negManifest    = vectorDir + "/vectors_negative_manifest.txt"

	vectorManifestHeader = "# Conformance vectors (format/vectors.md). Regenerate with:\n" +
		"#   go test ./format -run TestConformanceVectors -update\n" +
		"# name  xxhash64(file)  format.ContentHash\n"
	negManifestHeader = "# Negative conformance vectors: files a conforming reader must reject.\n" +
		"#   go test ./format -run TestConformanceVectorsNegative -update\n" +
		"# name  xxhash64(file)  (no content hash: these files do not decode)\n"
)

// vecOneSection is the smallest legal vertical range: aligned to 16 and one
// section deep. Records store minSection and sectionN, so a range like this
// costs two varints and one presence bit per bitset.
var vecOneSection = cube.Range{-64, -49}

// ---------------------------------------------------------------------------
// Vector definitions
// ---------------------------------------------------------------------------

// vectorCase is one positive vector.
type vectorCase struct {
	name string
	// build returns the file bytes. Every positive vector is written
	// uncompressed: compressed bytes are not part of the format's identity
	// (§4.8), so there is nothing in them for a vector to pin.
	build func(t *testing.T, reg world.BlockRegistry) []byte
	// check asserts the facts the specification fixes about this vector,
	// against the independent walker's view of the bytes. It is what stops a
	// vector from merely moving with the implementation.
	check func(t *testing.T, l *vecLayout, file []byte)
	// contentHash is skipped for indexed files, which parseFrame will not
	// accept; those carry indexedIdentity instead.
	indexed bool
}

func vectorCases() []vectorCase {
	return []vectorCase{
		{name: "world_minimal", build: buildWorldMinimal, check: checkWorldMinimal},
		{name: "world_empty_chunk", build: buildWorldEmptyChunk, check: checkWorldEmptyChunk},
		{name: "world_waterlogged", build: buildWorldWaterlogged, check: checkWorldWaterlogged},
		{name: "world_palette_256", build: buildWorldPalette256, check: checkPalette256},
		{name: "world_palette_257", build: buildWorldPalette257, check: checkPalette257},
		{name: "world_layers", build: buildWorldLayers, check: checkWorldLayers},
		{name: "world_default_biome", build: buildWorldDefaultBiome, check: checkWorldDefaultBiome},
		{name: "world_dedup_morton", build: buildWorldDedupMorton, check: checkWorldDedupMorton},
		{name: "world_collections", build: buildWorldCollections, check: checkWorldCollections},
		{name: "world_light", build: buildWorldLight, check: checkWorldLight},
		{name: "world_stats", build: buildWorldStats, check: checkWorldStats},
		{name: "world_preserved", build: buildWorldPreserved, check: checkWorldPreserved},
		{name: "structure_edge_padding", build: buildStructureEdge, check: checkStructureEdge},
		{name: "structure_full", build: buildStructureFull, check: checkStructureFull},
		{name: "indexed_torn", build: buildIndexedTorn, indexed: true},
	}
}

// ---------------------------------------------------------------------------
// Positive vector builders
// ---------------------------------------------------------------------------

func vecWrite(t *testing.T, d *WorldData, reg world.BlockRegistry, opts Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, opts); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var vecPlain = Options{Compression: CompressionNone}

// minimalWorld is one column holding one section of solid stone: the smallest
// world with any block in it. It is also the overworld member of the dimension
// trio, so the three dimension vectors differ only in the header's flag bits.
func minimalWorld(t *testing.T, reg world.BlockRegistry) *WorldData {
	t.Helper()
	ch := chunk.New(reg, vecOneSection)
	stone := rid(t, reg, block.Stone{})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y <= -49; y++ {
				ch.SetBlock(x, y, z, 0, stone)
			}
		}
	}
	return &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
}

func buildWorldMinimal(t *testing.T, reg world.BlockRegistry) []byte {
	return vecWrite(t, minimalWorld(t, reg), reg, vecPlain)
}

// buildWorldEmptyChunk stores a column with no blocks at all: every block
// presence bit clear over the dimension's full span (§4.3).
func buildWorldEmptyChunk(t *testing.T, reg world.BlockRegistry) []byte {
	ch := chunk.New(reg, overworldRange)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	return vecWrite(t, d, reg, vecPlain)
}

// buildWorldWaterlogged puts water in layer 1 with nothing in layer 0, the
// case §4.3 calls out: layer 0 is uniform air and is still stored, because a
// decoder that treated it as an absent section would lose the water above it.
func buildWorldWaterlogged(t *testing.T, reg world.BlockRegistry) []byte {
	ch := chunk.New(reg, vecOneSection)
	water := rid(t, reg, block.Water{Depth: 8, Still: true})
	ch.SetBlock(0, -64, 0, 1, water)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	return vecWrite(t, d, reg, vecPlain)
}

// paletteWorld fills one section with n distinct block states, so the
// section-local palette has exactly n entries. n = 256 is the widest palette a
// u8 index array can address and n = 257 the narrowest that needs u16 (§3.3).
//
// The states are synthetic preserved states rather than registry blocks: a
// registry-independent fixture does not move when dragonfly renumbers its
// block table, and the index-width path is the same either way.
func paletteWorld(t *testing.T, reg world.BlockRegistry, n int) *WorldData {
	t.Helper()
	ch := chunk.New(reg, vecOneSection)
	placeholder := placeholderRid(reg)
	states := make([]BlockState, n)
	for i := range states {
		states[i] = BlockState{Name: fmt.Sprintf("vector:state_%04d", i)}
	}
	unknown := make([]UnknownBlock, 0, 4096)
	i := 0
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := range uint8(16) {
				ch.SetBlock(x, int16(-64)+int16(y), z, 0, placeholder)
				unknown = append(unknown, UnknownBlock{
					Section: -4, Layer: 0,
					Index: uint16(x)<<8 | uint16(z)<<4 | uint16(y),
					State: uint32(i % n),
				})
				i++
			}
		}
	}
	return &WorldData{Columns: []Column{{
		X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
		Unknown: unknown, UnknownStates: states,
	}}}
}

func buildWorldPalette256(t *testing.T, reg world.BlockRegistry) []byte {
	return vecWrite(t, paletteWorld(t, reg, 256), reg, vecPlain)
}

func buildWorldPalette257(t *testing.T, reg world.BlockRegistry) []byte {
	return vecWrite(t, paletteWorld(t, reg, 257), reg, vecPlain)
}

// buildWorldLayers exercises §4.3's layer numbering: layer 1 is all air and is
// kept because layer 2 holds content, while everything above layer 2 is
// dropped. Removing the internal layer would renumber the layers above it and
// turn waterlogging into a solid liquid.
func buildWorldLayers(t *testing.T, reg world.BlockRegistry) []byte {
	ch := chunk.New(reg, vecOneSection)
	stone := rid(t, reg, block.Stone{})
	water := rid(t, reg, block.Water{Depth: 8, Still: true})
	ch.SetBlock(1, -64, 1, 0, stone)
	ch.SetBlock(2, -63, 2, 2, water)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	return vecWrite(t, d, reg, vecPlain)
}

// buildWorldDefaultBiome exercises §4.7. Two biomes each hold two uniform
// sections, so their uniform-section counts tie and only the stated tie-break
// (lowest global biome palette reference) decides which one the file elides.
func buildWorldDefaultBiome(t *testing.T, reg world.BlockRegistry) []byte {
	desert, ok := lookupBiome("minecraft:desert")
	if !ok {
		t.Skip("minecraft:desert is not registered")
	}
	plains, ok := lookupBiome("minecraft:plains")
	if !ok {
		t.Skip("minecraft:plains is not registered")
	}
	// Five sections: two uniformly desert, two uniformly plains, one mixed.
	rng := cube.Range{-64, 15}
	ch := chunk.New(reg, rng)
	stone := rid(t, reg, block.Stone{})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y <= 15; y++ {
				sec := (y + 64) / 16
				var b world.Biome
				switch {
				case sec < 2:
					b = desert
				case sec < 4:
					b = plains
				default:
					if x < 8 {
						b = desert
					} else {
						b = plains
					}
				}
				ch.SetBiome(x, y, z, uint32(b.EncodeBiome()))
			}
		}
	}
	ch.SetBlock(0, -64, 0, 0, stone)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	return vecWrite(t, d, reg, vecPlain)
}

// buildWorldDedupMorton stores four identical columns. Identical section blobs
// share one blob table entry (§3.4) and the records come out in Morton order
// rather than in the order they were handed over (§4).
func buildWorldDedupMorton(t *testing.T, reg world.BlockRegistry) []byte {
	stone := rid(t, reg, block.Stone{})
	col := func(x, z int32) Column {
		ch := chunk.New(reg, vecOneSection)
		for bx := range uint8(16) {
			for bz := range uint8(16) {
				ch.SetBlock(bx, -64, bz, 0, stone)
			}
		}
		return Column{X: x, Z: z, Col: &chunk.Column{Chunk: ch}}
	}
	// Handed over in an order the file must not preserve.
	d := &WorldData{Columns: []Column{col(1, 1), col(0, 1), col(1, 0), col(0, 0)}}
	return vecWrite(t, d, reg, vecPlain)
}

// buildWorldCollections pins the per-column collections and the total orders
// §4.8 fixes for them: block entities by (y, z, x), entities by UniqueID,
// scheduled updates by (y, z, x) then tick then block reference.
func buildWorldCollections(t *testing.T, reg world.BlockRegistry) []byte {
	ch := chunk.New(reg, vecOneSection)
	stone := rid(t, reg, block.Stone{})
	water := rid(t, reg, block.Water{Depth: 8, Still: true})
	ch.SetBlock(1, -64, 1, 0, stone)
	col := Column{
		X: 0, Z: 0,
		UserData: []byte("chunk-user-data"),
		Col: &chunk.Column{
			Chunk: ch,
			Tick:  4242,
			// Handed over out of order, so the file pins the sort and not the
			// caller's slice.
			BlockEntities: []chunk.BlockEntity{{
				Pos:  cube.Pos{5, -60, 6},
				Data: map[string]any{"id": "minecraft:furnace", "x": int32(5), "y": int32(-60), "z": int32(6)},
			}, {
				Pos:  cube.Pos{1, -64, 2},
				Data: map[string]any{"id": "minecraft:chest", "CustomName": "vector", "x": int32(1), "y": int32(-64), "z": int32(2)},
			}},
			Entities: []chunk.Entity{{ID: 9, Data: map[string]any{
				"identifier": "minecraft:pig", "UniqueID": int64(9),
				"Pos": []any{float32(3.5), float32(-60), float32(4.5)},
			}}, {ID: 7, Data: map[string]any{
				"identifier": "minecraft:cow", "UniqueID": int64(7),
				"Pos": []any{float32(1.5), float32(-60), float32(2.5)},
			}}},
			ScheduledBlocks: []chunk.ScheduledBlockUpdate{
				{Pos: cube.Pos{2, -59, 3}, Block: water, Tick: 120},
				{Pos: cube.Pos{0, -64, 0}, Block: stone, Tick: 99},
			},
		},
	}
	settings, err := marshalNBT(map[string]any{"name": "vector", "time": int64(1234), "difficulty": int32(2)})
	if err != nil {
		t.Fatal(err)
	}
	markers, err := marshalNBT(map[string]any{"markers": []map[string]any{
		{"name": "arena", "kind": "region", "pos": []any{0.0, 0.0, 0.0}},
		{"name": "spawn", "kind": "spawn", "pos": []any{1.5, -60.0, -2.5}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	border, err := marshalNBT(map[string]any{"min": [2]int32{-512, -512}, "max": [2]int32{512, 512}})
	if err != nil {
		t.Fatal(err)
	}
	d := &WorldData{
		Settings: settings, UserData: []byte("world-user-data"),
		Markers: markers, Border: border,
		Columns: []Column{col},
	}
	return vecWrite(t, d, reg, vecPlain)
}

// buildWorldLight sets flag StoreLight over a column whose light is baked.
// §4.6 makes light presence independent of block presence, which this file
// shows directly: sections with no blocks still carry sky light.
func buildWorldLight(t *testing.T, reg world.BlockRegistry) []byte {
	rng := cube.Range{-64, -17}
	ch := chunk.New(reg, rng)
	stone := rid(t, reg, block.Stone{})
	for x := range uint8(16) {
		for z := range uint8(16) {
			ch.SetBlock(x, -64, z, 0, stone)
		}
	}
	chunk.LightArea([]*chunk.Chunk{ch}, 0, 0).Fill()
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	return vecWrite(t, d, reg, Options{Compression: CompressionNone, StoreLight: true})
}

// buildWorldStats sets flag Stats, so the meta block carries the §4.2 summary.
func buildWorldStats(t *testing.T, reg world.BlockRegistry) []byte {
	return vecWrite(t, minimalWorld(t, reg), reg, Options{Compression: CompressionNone, Stats: true})
}

// buildWorldPreserved is the preserved-state sidecar case (§9): block states
// and a biome name that no registry resolves, carried through as palette
// entries with their own block versions, plus a scheduled update naming one of
// them. The two states sit at different versions, so the §3.1 override table
// has two entries and its delta encoding is exercised.
func buildWorldPreserved(t *testing.T, reg world.BlockRegistry) []byte {
	ch := chunk.New(reg, vecOneSection)
	placeholder := placeholderRid(reg)
	stone := rid(t, reg, block.Stone{})
	// The preserved biome sidecar re-emits its name wherever the fallback
	// biome (plains) is still in place, so the section has to hold it.
	plains, ok := lookupBiome(plainsBiomeName())
	if !ok {
		t.Skip("the fallback biome is not registered")
	}
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y <= -49; y++ {
				ch.SetBiome(x, y, z, uint32(plains.EncodeBiome()))
			}
		}
	}
	ch.SetBlock(1, -64, 1, 0, placeholder)
	ch.SetBlock(2, -64, 2, 0, placeholder)
	ch.SetBlock(3, -64, 3, 0, stone)
	col := Column{
		X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch, ScheduledBlocks: []chunk.ScheduledBlockUpdate{
			{Pos: cube.Pos{0, -64, 0}, Block: placeholder, Tick: 99},
		}},
		UnknownStates: []BlockState{
			// Two versions, neither of them the palette's own, so both entries
			// need an override and the delta encoding has two entries to walk.
			// Two properties, so the vector also pins §3.1's property block:
			// keys ascend bytewise and each carries its own type byte.
			{Name: "vector:missing", Properties: map[string]any{"level": int32(2), "variant": "a"}, Version: 17825806},
			{Name: "vector:other", Properties: map[string]any{"n": int32(3)}, Version: 17959425},
		},
		Unknown: []UnknownBlock{
			{Section: -4, Layer: 0, Index: uint16(1)<<8 | uint16(1)<<4, State: 0},
			{Section: -4, Layer: 0, Index: uint16(2)<<8 | uint16(2)<<4, State: 1},
		},
		UnknownTicks: []UnknownTick{{Pos: [3]int32{0, -64, 0}, At: 99, State: 1}},
		// A biome name the registry does not resolve travels the same way.
		UnknownBiomes:     []UnknownBlock{{Section: -4, Index: WholeStorage, State: 0}},
		UnknownBiomeNames: []string{"vector:void"},
	}
	d := &WorldData{Columns: []Column{col}}
	return vecWrite(t, d, reg, vecPlain)
}

// edgeStructure is a box whose sides are not multiples of 16, so its cells
// have padding outside the declared box. §6 requires that padding to be air in
// every layer: two structures differing only there are the same structure and
// must encode identically, which buildStructureEdge asserts directly.
func edgeStructure(t *testing.T, reg world.BlockRegistry, dirtyPadding bool) *StructureData {
	t.Helper()
	s, err := NewStructureData([3]int32{17, 3, 18})
	if err != nil {
		t.Fatal(err)
	}
	s.Origin = [3]int32{-8, 0, -9}
	stone := rid(t, reg, block.Stone{})
	dirt := rid(t, reg, block.Dirt{})
	air := reg.AirRuntimeID()
	set := func(x, y, z int32, r uint32) {
		cx, cy, cz := x>>4, y>>4, z>>4
		i := CellIndex(s.Size, cx, cy, cz)
		if s.Cells[i] == nil {
			s.Cells[i] = chunk.NewSubChunk(air)
		}
		s.Cells[i].SetBlock(uint8(x&15), uint8(y&15), uint8(z&15), 0, r)
	}
	set(0, 0, 0, stone)
	set(16, 0, 17, dirt) // the far corner, inside the box and inside an edge cell
	set(16, 2, 0, stone)
	if dirtyPadding {
		// Outside the declared box, inside an edge cell. A conforming writer
		// clears these before encoding.
		set(17, 0, 0, dirt)
		set(16, 0, 18, stone)
		set(0, 3, 0, stone)
	}
	return s
}

func buildStructureEdge(t *testing.T, reg world.BlockRegistry) []byte {
	var buf bytes.Buffer
	if err := WriteStructure(&buf, edgeStructure(t, reg, false), reg, vecPlain); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildStructureFull carries the parts of §6 the edge vector leaves out: a
// negative origin, an internal all-air layer, block entities, entities and a
// user data blob.
func buildStructureFull(t *testing.T, reg world.BlockRegistry) []byte {
	s, err := NewStructureData([3]int32{16, 16, 32})
	if err != nil {
		t.Fatal(err)
	}
	s.Origin = [3]int32{-3, -4, -5}
	s.UserData = []byte("structure-user-data")
	stone := rid(t, reg, block.Stone{})
	water := rid(t, reg, block.Water{Depth: 8, Still: true})
	air := reg.AirRuntimeID()
	cell := func(i int) *chunk.SubChunk {
		if s.Cells[i] == nil {
			s.Cells[i] = chunk.NewSubChunk(air)
		}
		return s.Cells[i]
	}
	cell(CellIndex(s.Size, 0, 0, 0)).SetBlock(0, 0, 0, 0, stone)
	// Layer 1 left as air under a populated layer 2: an internal all-air layer
	// is kept because layer numbers are semantic (§6).
	cell(CellIndex(s.Size, 0, 0, 0)).SetBlock(1, 1, 1, 2, water)
	cell(CellIndex(s.Size, 0, 0, 1)).SetBlock(2, 2, 2, 0, stone)
	s.BlockEntities = []StructureBlockEntity{
		{Pos: [3]int32{2, 2, 18}, Data: map[string]any{"id": "minecraft:chest"}},
		{Pos: [3]int32{0, 0, 0}, Data: map[string]any{"id": "minecraft:furnace"}},
	}
	s.Entities = []map[string]any{
		{"identifier": "minecraft:pig", "UniqueID": int64(2), "Pos": []any{float32(1), float32(1), float32(1)}},
		{"identifier": "minecraft:cow", "UniqueID": int64(1), "Pos": []any{float32(0), float32(0), float32(0)}},
	}
	var buf bytes.Buffer
	if err := WriteStructure(&buf, s, reg, vecPlain); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildIndexedTorn writes an indexed world with two checkpoints and destroys
// the newest footer, the torn write §5.6 exists for. Opening it must fall back
// to the previous checkpoint rather than fail.
func buildIndexedTorn(t *testing.T, reg world.BlockRegistry) []byte {
	path := filepath.Join(t.TempDir(), "torn.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	d := minimalWorld(t, reg)
	c := d.Columns[0]
	if err := w.Store(c); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// A second generation, whose footer is then destroyed.
	c2 := c
	c2.X = 1
	if err := w.Store(c2); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Clone(b)
	tail := b[len(b)-footerSize:]
	for i := range tail {
		tail[i] ^= 0xff
	}
	return b
}

// ---------------------------------------------------------------------------
// Per-vector specification assertions
// ---------------------------------------------------------------------------

// vecField fetches a named span, failing the test when the walker never produced
// one: an assertion on a vecField that does not exist asserts nothing.
func vecField(t *testing.T, l *vecLayout, path string) vecSpan {
	t.Helper()
	s, err := l.look(path)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return s
}

func wantField(t *testing.T, l *vecLayout, path string, off, size int, val uint64) {
	t.Helper()
	s := vecField(t, l, path)
	if s.off != off || s.size != size || s.val != val {
		t.Errorf("%s: at offset %d, %d bytes, value %d; want offset %d, %d bytes, value %d",
			path, s.off, s.size, s.val, off, size, val)
	}
}

// checkHeaderLayout asserts §2.1's vecField offsets and widths on every vector.
func checkHeaderLayout(t *testing.T, l *vecLayout, file []byte) {
	t.Helper()
	wantField(t, l, "header.magic", 0, 4, 0)
	wantField(t, l, "header.version", 4, 2, 2)
	wantField(t, l, "header.kind", 6, 1, uint64(l.kind))
	wantField(t, l, "header.mode", 7, 1, 0)
	wantField(t, l, "header.flags", 8, 4, uint64(l.flags))
	wantField(t, l, "header.blockVersion", 12, 4, uint64(uint32(l.blockVersion)))
	// §2.2: the footer is the last 44 bytes and its magic the last four.
	wantField(t, l, "footer.magic", len(file)-4, 4, 0)
	wantField(t, l, "footer.hash", len(file)-44, 8, xxhashOf(file))
}

// xxhashOf recomputes the §2.4 checkpoint hash over a solid file.
func xxhashOf(file []byte) uint64 {
	h := xxhash.New()
	_, _ = h.Write(file[:16])
	_, _ = h.Write(file[16 : len(file)-44])
	_, _ = h.Write(file[len(file)-44+8:])
	return h.Sum64()
}

func checkWorldMinimal(t *testing.T, l *vecLayout, file []byte) {
	if len(l.records) != 1 {
		t.Fatalf("got %d records, want 1", len(l.records))
	}
	r := l.records[0]
	if r.x != 0 || r.z != 0 || r.minSection != -4 || r.sectionN != 1 {
		t.Errorf("record = (%d,%d) minSection %d sectionN %d; want (0,0) -4 1", r.x, r.z, r.minSection, r.sectionN)
	}
	if len(r.blockSections) != 1 || len(r.blockSections[0]) != 1 {
		t.Fatalf("want one present section with one layer, got %v", r.blockSections)
	}
	if len(l.blobs) != 1 {
		t.Fatalf("want a single blob, got %d", len(l.blobs))
	}
	// A section of one block state is uniform: width 0 and no index array
	// (§3.3), which is what makes a flat world collapse to a few bytes.
	if l.blobs[0].width != 0 || len(l.blobs[0].refs) != 1 || l.blobs[0].idx != nil {
		t.Errorf("blob 0 = width %d, %d refs, %d indices; want a uniform blob", l.blobs[0].width, len(l.blobs[0].refs), len(l.blobs[0].idx))
	}
	if len(l.blockPalette) != 1 || l.blockPalette[0].name != "minecraft:stone" {
		t.Errorf("block palette = %v; want one entry, minecraft:stone", l.blockPalette)
	}
	// The single biome section is uniform, so §4.7 requires the flag to be set
	// and the section elided.
	if l.flags&vecFlagDefaultBiome == 0 {
		t.Errorf("flags %#x: DefaultBiome must be set when a uniform biome section exists", l.flags)
	}
	if len(r.biomeSections) != 0 {
		t.Errorf("uniform-default biome sections must be omitted, got %v", r.biomeSections)
	}
	if (l.flags & vecReservedDimBits) != 0 {
		t.Errorf("flags %#x: bits 5-7 are reserved and must be zero", l.flags)
	}
}

func checkWorldEmptyChunk(t *testing.T, l *vecLayout, file []byte) {
	if len(l.records) != 1 {
		t.Fatalf("got %d records, want 1", len(l.records))
	}
	r := l.records[0]
	// §4.3: an empty chunk carries the dimension's full span with every
	// presence bit clear. Trimming would give one chunk several encodings.
	if r.minSection != -4 || r.sectionN != 24 {
		t.Errorf("span = minSection %d sectionN %d; want -4 and 24, the full overworld range", r.minSection, r.sectionN)
	}
	if len(r.blockSections) != 0 {
		t.Errorf("want every block presence bit clear, got %v", r.blockSections)
	}
	if len(l.blobs) != 0 {
		t.Errorf("want an empty blob table, got %d blobs", len(l.blobs))
	}
	if len(l.blockPalette) != 0 {
		t.Errorf("want an empty block palette, got %d entries", len(l.blockPalette))
	}
	pres := vecField(t, l, "record[0].blockPresence")
	if pres.size != 3 {
		t.Errorf("blockPresence is %d bytes; bitset(24) is 3", pres.size)
	}
}

func checkWorldWaterlogged(t *testing.T, l *vecLayout, file []byte) {
	r := l.records[0]
	layers, ok := r.blockSections[0]
	if !ok {
		t.Fatalf("section 0 must be present: its layer 1 holds water")
	}
	if len(layers) != 2 {
		t.Fatalf("want 2 layers, got %d", len(layers))
	}
	// Layer 0 is uniform air and is stored anyway: dropping it would lose the
	// water above it, and a reader must not read a uniform-air layer 0 as an
	// absent section (§4.3).
	l0 := l.blobs[layers[0]]
	if l0.width != 0 || len(l0.refs) != 1 {
		t.Errorf("layer 0 blob = width %d with %d refs; want uniform", l0.width, len(l0.refs))
	}
	if got := l.blockPalette[l0.refs[0]].name; got != "minecraft:air" {
		t.Errorf("layer 0 holds %q; want minecraft:air", got)
	}
	l1 := l.blobs[layers[1]]
	if l1.width != 1 || len(l1.refs) != 2 {
		t.Errorf("layer 1 blob = width %d with %d refs; want width 1 and two refs", l1.width, len(l1.refs))
	}
}

func checkPaletteWidth(t *testing.T, l *vecLayout, n int, wantWidth uint8, wantBytes int) {
	t.Helper()
	r := l.records[0]
	layers := r.blockSections[0]
	if len(layers) != 1 {
		t.Fatalf("want one layer, got %d", len(layers))
	}
	b := l.blobs[layers[0]]
	if len(b.refs) != n {
		t.Fatalf("local palette has %d entries, want %d", len(b.refs), n)
	}
	if b.width != wantWidth {
		t.Fatalf("index width is %d, want %d for a %d-entry palette", b.width, wantWidth, n)
	}
	idx := vecField(t, l, fmt.Sprintf("blobTable.blob[%d].indices", layers[0]))
	if idx.size != wantBytes {
		t.Errorf("index array is %d bytes, want %d", idx.size, wantBytes)
	}
	if len(l.blockPalette) != n {
		t.Errorf("global palette has %d entries, want %d", len(l.blockPalette), n)
	}
}

func checkPalette256(t *testing.T, l *vecLayout, file []byte) {
	// 256 entries is the largest a u8 index can address, so width 1 and 4096
	// index bytes. A reader that widens here produces a second encoding.
	checkPaletteWidth(t, l, 256, 1, 4096)
}

func checkPalette257(t *testing.T, l *vecLayout, file []byte) {
	// One more entry and u8 no longer suffices: width 2 and 8192 bytes.
	checkPaletteWidth(t, l, 257, 2, 8192)
}

func checkWorldLayers(t *testing.T, l *vecLayout, file []byte) {
	layers := l.records[0].blockSections[0]
	if len(layers) != 3 {
		t.Fatalf("want 3 layers (0 stone, 1 all air, 2 water), got %d", len(layers))
	}
	mid := l.blobs[layers[1]]
	if mid.width != 0 || len(mid.refs) != 1 {
		t.Fatalf("layer 1 must be a uniform blob, got width %d with %d refs", mid.width, len(mid.refs))
	}
	if got := l.blockPalette[mid.refs[0]].name; got != "minecraft:air" {
		t.Errorf("internal layer 1 holds %q; an internal all-air layer is kept as uniform air", got)
	}
	// Nothing above layer 2: trailing all-air layers are dropped.
	if n := vecField(t, l, "record[0].section[0].layerN").val; n != 3 {
		t.Errorf("layerN is %d, want 3", n)
	}
}

func checkWorldDefaultBiome(t *testing.T, l *vecLayout, file []byte) {
	if l.flags&vecFlagDefaultBiome == 0 {
		t.Fatalf("flags %#x: DefaultBiome must be set", l.flags)
	}
	ref := l.flags >> vecDefaultBiomeShift
	if int(ref) >= len(l.biomePalette) {
		t.Fatalf("defaultBiomeRef %d is past the biome palette of %d", ref, len(l.biomePalette))
	}
	// The uniform-section counts tie, so §4.7's tie-break decides: the lowest
	// global biome palette reference wins.
	if ref != 0 {
		t.Errorf("defaultBiomeRef is %d; a tie is broken by the lowest reference, which is 0", ref)
	}
	// Sections uniformly the default are omitted; the others are stored.
	r := l.records[0]
	if len(r.biomeSections) != 3 {
		t.Errorf("stored biome sections = %v; want the three that are not uniformly the default", r.biomeSections)
	}
	if len(l.biomePalette) != 2 {
		t.Errorf("biome palette = %v; want two entries", l.biomePalette)
	}
}

func checkWorldDedupMorton(t *testing.T, l *vecLayout, file []byte) {
	if len(l.blobs) != 1 {
		t.Errorf("four identical columns must share one blob, got %d", len(l.blobs))
	}
	want := [][2]int32{{0, 0}, {1, 0}, {0, 1}, {1, 1}}
	got := make([][2]int32, len(l.records))
	for i, r := range l.records {
		got[i] = [2]int32{r.x, r.z}
	}
	if !slices.Equal(got, want) {
		t.Errorf("record order = %v; want Morton order %v regardless of the order columns were handed over", got, want)
	}
}

func checkWorldCollections(t *testing.T, l *vecLayout, file []byte) {
	r := l.records[0]
	if r.blockEntities != 2 || r.entities != 2 || r.ticks != 2 {
		t.Errorf("collections = %d block entities, %d entities, %d ticks; want 2 each", r.blockEntities, r.entities, r.ticks)
	}
	if r.tick != 4242 {
		t.Errorf("column tick = %d, want 4242", r.tick)
	}
	// §4.4: the block entity written first is the one lowest in (y, z, x). The
	// walker already rejects a file whose order disagrees; this pins which
	// entry that is, so the vector shows the order rather than just permitting
	// it.
	if y := int64(vecField(t, l, "record[0].be[0].y").val); y != -64 {
		t.Errorf("first block entity y = %d; (y,z,x) order puts the y=-64 entry first", y)
	}
	if p := vecField(t, l, "record[0].st[0].y").val; int64(p) != -64 {
		t.Errorf("first scheduled update y = %d; (y,z,x) order puts the y=-64 entry first", int64(p))
	}
	if ud := vecField(t, l, "record[0].userData"); string(file[ud.off:ud.off+ud.size]) != "chunk-user-data" {
		t.Errorf("record user data = %q", file[ud.off:ud.off+ud.size])
	}
}

func checkWorldLight(t *testing.T, l *vecLayout, file []byte) {
	if l.flags&vecFlagStoreLight == 0 {
		t.Fatalf("flags %#x: StoreLight must be set", l.flags)
	}
	r := l.records[0]
	if len(r.light) == 0 {
		t.Fatalf("StoreLight is set but no section carries light")
	}
	// §4.6: light presence is independent of block presence. Sections with no
	// blocks still carry sky light, and this vector holds at least one.
	var lightNoBlocks int
	for s := range r.light {
		if _, ok := r.blockSections[s]; !ok {
			lightNoBlocks++
		}
	}
	if lightNoBlocks == 0 {
		t.Errorf("no section carries light without blocks; light presence must not be tied to block presence")
	}
	for s, f := range r.light {
		if f == 0 || f&^0b11 != 0 {
			t.Errorf("section %d light flags %#x: bit 0 is block light, bit 1 sky light, the rest zero, and not all clear", s, f)
		}
	}
}

func checkWorldStats(t *testing.T, l *vecLayout, file []byte) {
	if l.flags&vecFlagStats == 0 {
		t.Fatalf("flags %#x: Stats must be set", l.flags)
	}
	s := vecField(t, l, "meta.stats")
	m, err := unmarshalNBT(file[s.off : s.off+s.size])
	if err != nil {
		t.Fatalf("stats blob: %v", err)
	}
	// §4.2: every counter is a long, because the format's own ceilings exceed
	// what a 32-bit tag holds.
	for _, k := range []string{"chunks", "filledSections", "uniqueBlobs", "blockStates", "biomes"} {
		v, ok := m[k]
		if !ok {
			t.Errorf("stats has no %q", k)
			continue
		}
		if _, ok := v.(int64); !ok {
			t.Errorf("stats %q is %T, want a long", k, v)
		}
	}
	if m["chunks"] != int64(1) || m["uniqueBlobs"] != int64(1) {
		t.Errorf("stats = %v; want chunks 1 and uniqueBlobs 1", m)
	}
}

func checkWorldPreserved(t *testing.T, l *vecLayout, file []byte) {
	// §3.1: preserved states are ordinary palette entries carrying a version
	// override, and only they can. Every other entry sits at the palette's own
	// version.
	var overrides int
	for _, e := range l.blockPalette {
		if e.version != 0 {
			overrides++
			if e.version == l.blockVersion {
				t.Errorf("override version %d repeats the palette's own", e.version)
			}
		}
	}
	if overrides != 2 {
		t.Errorf("got %d version overrides, want 2", overrides)
	}
	names := make([]string, len(l.blockPalette))
	for i, e := range l.blockPalette {
		names[i] = e.name
	}
	for _, want := range []string{"vector:missing", "vector:other"} {
		if !slices.Contains(names, want) {
			t.Errorf("block palette %v is missing the preserved state %q", names, want)
		}
	}
	if !slices.Contains(l.biomePalette, "vector:void") {
		t.Errorf("biome palette %v is missing the preserved biome name", l.biomePalette)
	}
	// The override indices strictly ascend and the first deltas from zero.
	if d := vecField(t, l, "blockPalette.override[1].indexDelta").val; d == 0 {
		t.Errorf("the second override's delta is zero; indices strictly ascend")
	}
}

func checkStructureEdge(t *testing.T, l *vecLayout, file []byte) {
	if l.kind != 1 {
		t.Fatalf("kind = %d, want 1", l.kind)
	}
	if l.size != [3]uint64{17, 3, 18} {
		t.Fatalf("size = %v, want 17x3x18", l.size)
	}
	if l.origin != [3]int64{-8, 0, -9} {
		t.Errorf("origin = %v, want -8,0,-9", l.origin)
	}
	// §6: cells = ceil(size/16) per axis, x-major then z then y.
	cells := vecField(t, l, "structure.cellPresence")
	if cells.size != 1 {
		t.Errorf("cellPresence is %d bytes; ceil(17/16)*ceil(3/16)*ceil(18/16) = 4 cells is one byte", cells.size)
	}
	if len(l.biomePalette) != 0 {
		t.Errorf("a structure's biome palette must have zero entries, got %d", len(l.biomePalette))
	}
	if l.flags&^vecFlagUncompressed != 0 {
		t.Errorf("flags %#x: a structure sets no flag but Uncompressed", l.flags)
	}
}

func checkStructureFull(t *testing.T, l *vecLayout, file []byte) {
	if l.origin != [3]int64{-3, -4, -5} {
		t.Errorf("origin = %v, want -3,-4,-5", l.origin)
	}
	if len(l.cells) != 2 {
		t.Fatalf("want 2 present cells, got %d", len(l.cells))
	}
	// The first present cell carries three layers, the middle one all air:
	// internal all-air layers are kept because layer numbers are semantic.
	n := vecField(t, l, fmt.Sprintf("structure.cell[%d].layerN", l.cells[0])).val
	if n != 3 {
		t.Fatalf("cell %d has layerN %d, want 3", l.cells[0], n)
	}
	mid := vecField(t, l, fmt.Sprintf("structure.cell[%d].blobRef[1]", l.cells[0])).val
	b := l.blobs[mid]
	if b.width != 0 || l.blockPalette[b.refs[0]].name != "minecraft:air" {
		t.Errorf("cell layer 1 is not uniform air: width %d, %q", b.width, l.blockPalette[b.refs[0]].name)
	}
	if v := vecField(t, l, "structure.beN").val; v != 2 {
		t.Errorf("beN = %d, want 2", v)
	}
	if v := vecField(t, l, "structure.entN").val; v != 2 {
		t.Errorf("entN = %d, want 2", v)
	}
	ud := vecField(t, l, "meta.userData")
	if string(file[ud.off:ud.off+ud.size]) != "structure-user-data" {
		t.Errorf("structure user data = %q", file[ud.off:ud.off+ud.size])
	}
	for _, name := range []string{"meta.settings", "meta.markers", "meta.border"} {
		if s := vecField(t, l, name); s.size != 0 {
			t.Errorf("%s is %d bytes; a structure's settings, markers and border are empty", name, s.size)
		}
	}
}

// ---------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------

type vectorRecord struct {
	fileHash    uint64
	contentHash uint64
}

func readVectorManifest(t *testing.T, path string) (int, map[string]vectorRecord) {
	t.Helper()
	out := map[string]vectorRecord{}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, out
	}
	version := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "version" {
			version, _ = strconv.Atoi(f[1])
			continue
		}
		if len(f) != 2 && len(f) != 3 {
			continue
		}
		fh, _ := strconv.ParseUint(f[1], 16, 64)
		var ch uint64
		if len(f) == 3 {
			ch, _ = strconv.ParseUint(f[2], 16, 64)
		}
		out[f[0]] = vectorRecord{fileHash: fh, contentHash: ch}
	}
	return version, out
}

func writeVectorManifest(t *testing.T, path, header string, recs map[string]vectorRecord) {
	t.Helper()
	names := make([]string, 0, len(recs))
	for n := range recs {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString(header)
	fmt.Fprintf(&sb, "version %d\n", Version)
	for _, n := range names {
		if recs[n].contentHash == 0 {
			// A negative vector does not decode, so it has no content identity.
			fmt.Fprintf(&sb, "%s %016x\n", n, recs[n].fileHash)
			continue
		}
		fmt.Fprintf(&sb, "%s %016x %016x\n", n, recs[n].fileHash, recs[n].contentHash)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// vectorManifestGuard is the golden suite's guard, applied to the vectors: a
// regeneration that changes bytes while format.Version stays put is a wire
// format change made by accident unless -format-change says otherwise. The
// vectors are as much the arbiter as the goldens, so the freeze lockout applies
// to them too: requireUnfrozen refuses -update outright while format.Version is
// format.FrozenVersion, before either suite writes a file.
func vectorManifestGuard(t *testing.T, path, header string, prevVersion int, prev, next map[string]vectorRecord) {
	t.Helper()
	if !*update {
		return
	}
	var changed []string
	for name, rec := range next {
		if old, ok := prev[name]; ok && old != rec {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 && prevVersion == Version && !*formatChange {
		t.Fatalf("refusing to bless a wire format change: %v changed while format.Version is still %d.\n"+
			"v%d is not the frozen version (format.FrozenVersion = %d), so re-run with -format-change "+
			"if the change is deliberate, or bump format.Version if v%d has been released.",
			changed, Version, Version, FrozenVersion, Version)
	}
	writeVectorManifest(t, path, header, next)
}

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

// TestConformanceVectors generates (under -update) and verifies the vector
// appendix. Without -update it is a pure check: bytes, content hashes, a
// decode/re-encode round trip and the independent walker's reading of every
// vecField.
func TestConformanceVectors(t *testing.T) {
	requireUnfrozen(t)
	reg := testRegistry(t)
	prevVersion, prev := readVectorManifest(t, vectorManifest)
	next := map[string]vectorRecord{}
	t.Cleanup(func() {
		vectorManifestGuard(t, vectorManifest, vectorManifestHeader, prevVersion, prev, next)
	})

	if *update {
		if err := os.MkdirAll(vectorDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range vectorCases() {
		t.Run(c.name, func(t *testing.T) {
			got := c.build(t, reg)
			path := filepath.Join(vectorDir, c.name+".pile")
			var content uint64
			if c.indexed {
				content = vectorIndexedIdentity(t, got, reg)
			} else {
				h, err := ContentHash(got, reg)
				if err != nil {
					t.Fatalf("ContentHash: %v", err)
				}
				content = h
			}
			next[c.name] = vectorRecord{fileHash: xxhash.Sum64(got), contentHash: content}

			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s rewritten: %d bytes, ContentHash %016x", path, len(got), content)
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (regenerate with -update if the format changed deliberately)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the writer no longer produces %s.\ngot  %d bytes: %s\nwant %d bytes: %s",
					path, len(got), hex.EncodeToString(got[:min(64, len(got))]),
					len(want), hex.EncodeToString(want[:min(64, len(want))]))
			}
			if rec, ok := prev[c.name]; ok && rec.contentHash != content {
				t.Errorf("ContentHash is %016x, the manifest records %016x", content, rec.contentHash)
			}

			if c.indexed {
				return
			}

			// The bytes decode, and decode + re-encode reproduces them.
			vectorRoundTrip(t, want, reg)

			// An independent reading of the same bytes, from the specification.
			l, err := vecWalk(want)
			if err != nil {
				t.Fatalf("independent walk of %s failed: %v", path, err)
			}
			checkHeaderLayout(t, l, want)
			if c.check != nil {
				c.check(t, l, want)
			}
		})
	}
}

// vectorRoundTrip decodes a vector and re-encodes it, requiring the bytes back.
// This is the property that makes a ContentHash meaningful: the canonical form
// is a fixed point of decode-then-encode.
func vectorRoundTrip(t *testing.T, file []byte, reg world.BlockRegistry) {
	t.Helper()
	m, err := ReadMeta(file)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	var out bytes.Buffer
	if m.Kind == KindStructure {
		s, err := ReadStructure(file, reg)
		if err != nil {
			t.Fatalf("ReadStructure: %v", err)
		}
		if err := WriteStructure(&out, s, reg, vecPlain); err != nil {
			t.Fatalf("WriteStructure: %v", err)
		}
	} else {
		d, err := ReadWorld(file, reg)
		if err != nil {
			t.Fatalf("ReadWorld: %v", err)
		}
		opts := vecPlain
		opts.StoreLight = m.Flags&FlagStoreLight != 0
		opts.Stats = m.Flags&FlagStats != 0
		if err := WriteWorld(&out, d, reg, opts); err != nil {
			t.Fatalf("WriteWorld: %v", err)
		}
	}
	if !bytes.Equal(out.Bytes(), file) {
		t.Fatalf("decode + re-encode changed the bytes: %d in, %d out", len(file), out.Len())
	}
}

// vectorIndexedIdentity reduces an indexed file to the identity of the content
// it holds, by opening it and re-encoding what it yields as a solid file.
// Indexed bytes are history-dependent (§5); their content is not.
func vectorIndexedIdentity(t *testing.T, file []byte, reg world.BlockRegistry) uint64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vector.pile")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("OpenIndexed on a torn file must recover: %v", err)
	}
	defer w.Close()
	settings, userData, markers, border := w.Meta()
	d := &WorldData{
		Settings: settings, UserData: userData, Markers: markers, Border: border,
	}
	for _, k := range w.Positions() {
		c, err := w.Column(k[0], k[1])
		if err != nil {
			t.Fatal(err)
		}
		d.Columns = append(d.Columns, c)
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, vecPlain); err != nil {
		t.Fatal(err)
	}
	h, err := ContentHash(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestVectorStructurePaddingIsNotStored is the one §6 rule a reader cannot
// check: padding lies outside the declared box by definition, so a file
// carrying it decodes to the same structure as one that cleared it. It is
// verified the only way it can be, by encoding both and comparing.
func TestVectorStructurePaddingIsNotStored(t *testing.T) {
	reg := testRegistry(t)
	var clean, dirty bytes.Buffer
	if err := WriteStructure(&clean, edgeStructure(t, reg, false), reg, vecPlain); err != nil {
		t.Fatal(err)
	}
	if err := WriteStructure(&dirty, edgeStructure(t, reg, true), reg, vecPlain); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clean.Bytes(), dirty.Bytes()) {
		t.Fatalf("blocks outside the declared box reached the file: %d bytes clean, %d dirty",
			clean.Len(), dirty.Len())
	}
	want, err := os.ReadFile(filepath.Join(vectorDir, "structure_edge_padding.pile"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clean.Bytes(), want) {
		t.Fatalf("the padded structure does not encode to the checked-in vector")
	}
}
